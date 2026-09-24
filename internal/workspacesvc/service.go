// Package workspacesvc implements workspace.v1.BranchSessionService: the trusted
// entry point for creating sessions (which derives the role + immutable preset)
// and the read/browse surface for the webui.
package workspacesvc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
	"github.com/abcp-sdk/workspace-gateway/internal/gitcommit"
	"github.com/abcp-sdk/workspace-gateway/internal/gitimport"
	"github.com/abcp-sdk/workspace-gateway/internal/gitmerge"
	"github.com/abcp-sdk/workspace-gateway/internal/imagebuild"
	"github.com/abcp-sdk/workspace-gateway/internal/members"
	"github.com/abcp-sdk/workspace-gateway/internal/roles"
	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/servicesmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/workerclient"
)

// Service implements wsv1connect.BranchSessionServiceHandler.
type Service struct {
	agent        agentv1connect.AgentServiceClient
	members      *members.Store
	git          *forgejo.Client
	sbx          *sandboxmgr.Client
	services     *servicesmgr.Client
	builder      *imagebuild.Builder
	runtime      runtimeprofiles.Settings
	// sandboxOrg is the ONLY registry org a sandbox image may come from. The
	// deployment pre-imports worker-bundled images there (see sandbox-images/),
	// so a sandbox can never run an arbitrary upstream image.
	sandboxOrg string
	// defaultSandboxImage is used when CreateSandbox omits an image.
	defaultSandboxImage string
	toolchainOrg        string // default owner for ListOCIImages
	svcToken     string // shared service token (sandbox-only service-to-service)
	svcTenant    string // tenant the service token resolves to (sandbox ownership)
	commits      *gitcommit.Manager
	sandboxNS    string // namespace services/sandboxes live in (public-host inference)
	// previewTTL reclaims a preview service after this long (0 = no TTL).
	previewTTL time.Duration
	// serviceLogTail is the default number of log lines returned.
	serviceLogTail int64
	// publicServiceDomain, when set, forces the domain services are published
	// under (`<name>.<ns>.<domain>`). Empty = infer from the request Host.
	publicServiceDomain string
	// pvcStorageClass is the StorageClass for CreatePVC; "" = the cluster
	// default. Only the self-hosted local-path class is supported today.
	pvcStorageClass string
	// pvcDefaultSize is used when CreatePVC omits a size.
	pvcDefaultSize string
}

// Deps configures the service.
type Deps struct {
	Agent   agentv1connect.AgentServiceClient
	Members *members.Store
	Forgejo *forgejo.Client
	Sandbox *sandboxmgr.Client
	// Services manages long-lived Deployments.
	Services *servicesmgr.Client
	// Builder builds/derives images (repo Dockerfile / base image -> registry).
	Builder *imagebuild.Builder
	// Runtime holds the deployment's runtime knobs (KVM/GPU devices).
	Runtime runtimeprofiles.Settings
	// SandboxOrg is the registry org sandbox images MUST come from. A sandbox
	// request naming an image outside it is refused.
	SandboxOrg string
	// DefaultSandboxImage is used when CreateSandbox omits an image.
	DefaultSandboxImage string
	// ToolchainOrg is the default owner ListOCIImages browses.
	ToolchainOrg string
	// ServiceToken + ServiceTenant enable the sandbox-only service-to-service
	// path used by the workspace extension (which has no tenant token). Empty
	// disables it.
	ServiceToken  string
	ServiceTenant string
	// GitCloneDir is where branch-staging clones are cached (empty = temp).
	GitCloneDir string
	// SandboxNamespace is the namespace services/sandboxes live in; used to
	// build the public host `<name>.<ns>.<domain>`.
	SandboxNamespace string
	// PublicServiceDomain forces the domain services are published under. Empty
	// = infer from the request Host (see publicDomainFor).
	PublicServiceDomain string
	// PreviewTTL reclaims a preview service after this long (0 = no TTL).
	PreviewTTL time.Duration
	// ServiceLogTail is the default number of service-log lines returned.
	ServiceLogTail int64
	// PVCStorageClass is the StorageClass CreatePVC uses ("" = cluster default).
	// Only the self-hosted local-path class is supported.
	PVCStorageClass string
	// PVCDefaultSize is used when CreatePVC omits a size (e.g. "1Gi").
	PVCDefaultSize string
}

// New builds the service.
func New(d Deps) *Service {
	commits := gitcommit.NewManager()
	commits.SetBaseDir(d.GitCloneDir)
	return &Service{
		agent: d.Agent, members: d.Members, git: d.Forgejo, sbx: d.Sandbox,
		services: d.Services,
		builder: d.Builder, runtime: d.Runtime,
		sandboxOrg: d.SandboxOrg, defaultSandboxImage: d.DefaultSandboxImage,
		toolchainOrg: d.ToolchainOrg,
		svcToken: d.ServiceToken, svcTenant: d.ServiceTenant,
		commits:             commits,
		sandboxNS:           d.SandboxNamespace,
		publicServiceDomain: d.PublicServiceDomain,
		previewTTL:          d.PreviewTTL,
		serviceLogTail:      d.ServiceLogTail,
		pvcStorageClass:     d.PVCStorageClass,
		pvcDefaultSize:      d.PVCDefaultSize,
	}
}

// ReapPreviewServices deletes expired preview services. Exposed so the
// deployment can run it on a ticker.
func (s *Service) ReapPreviewServices(ctx context.Context) ([]string, error) {
	if s.services == nil {
		return nil, nil
	}
	return s.services.ReapExpired(ctx, time.Now().UnixMilli())
}

// renderRuntime maps the caller's (kvm, gpuCount) to pod-level settings using
// the deployment's runtime knobs.
func (s *Service) renderRuntime(kvm bool, gpuCount int32) runtimeprofiles.Rendered {
	return s.runtime.Render(kvm, int(gpuCount))
}

// sandboxAuth resolves the caller's tenant for a sandbox RPC. A request bearing
// the shared service token resolves to the configured service tenant (unless it
// names a real tenant via X-Abc-Tenant); every other request resolves through
// the agent identity as usual.
func (s *Service) sandboxAuth(ctx context.Context, hdr map[string][]string) (string, error) {
	if t, ok := s.serviceTenant(hdr); ok {
		return t, nil
	}
	return s.resolveTenant(ctx, hdr)
}

// serviceTenant resolves a SERVICE-TOKEN caller's tenant: the `X-Abc-Tenant`
// header when present and valid (the workspace extension acts on behalf of the
// real caller), else the configured synthetic service tenant. ok=false when the
// request does not carry the shared service token.
func (s *Service) serviceTenant(hdr map[string][]string) (string, bool) {
	if s.svcToken == "" || bearerToken(hdr) != s.svcToken {
		return "", false
	}
	if t := headerValue(hdr, "X-Abc-Tenant"); validTenantID(t) {
		return t, true
	}
	return s.svcTenant, true
}

// validTenantID mirrors the agent's tenant charset ^[A-Za-z0-9_-]{1,64}$.
func validTenantID(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for _, r := range t {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func headerValue(hdr map[string][]string, name string) string {
	for k, vs := range hdr {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

// forwardedHost returns the public host the client used: X-Forwarded-Host
// (set by the edge) when present, else the Host header.
func forwardedHost(hdr map[string][]string) string {
	if h := headerValue(hdr, "X-Forwarded-Host"); h != "" {
		return h
	}
	return headerValue(hdr, "Host")
}

func bearerToken(hdr map[string][]string) string {
	for k, vs := range hdr {
		if strings.EqualFold(k, "Authorization") {
			for _, v := range vs {
				if strings.HasPrefix(v, "Bearer ") {
					return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
				}
			}
		}
	}
	return ""
}

// ---- tenant ----

// resolveTenant asks the agent who the caller is (forwarding their token). A
// service-token caller is resolved from X-Abc-Tenant / the service tenant so the
// extension can act for the real tenant without an agent identity round-trip.
func (s *Service) resolveTenant(ctx context.Context, hdr map[string][]string) (string, error) {
	if t, ok := s.serviceTenant(hdr); ok {
		return t, nil
	}
	req := connect.NewRequest(&agentv1.GetIdentityRequest{})
	copyHeaders(req, hdr)
	res, err := s.agent.GetIdentity(ctx, req)
	if err != nil {
		return "", err
	}
	t := res.Msg.GetTenant()
	if t == "" {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("no tenant for credential"))
	}
	return t, nil
}

// ---- workspaces ----

// EnsureBranchSession ensures `org:repo:branch` exists as a session (repo:branch
// <-> session, 1:1) and returns it. The gateway ensures the Forgejo repo, records
// ownership, and — when the session is ABSENT — creates it with the role's
// immutable preset. When it ALREADY exists the call is IDEMPOTENT (returns the
// existing session); this is what lets a UI/tool path "just ensure" a branch has
// a session.
//
// Ownership rule: a repo that already exists belongs to whoever created it. A
// tenant may NOT claim a repo it does not own (it would otherwise be added to
// the visibility table and could browse another tenant's repository).
//
// The deployment's default branch is ALWAYS `main`: `branch` empty means `main`.
func (s *Service) EnsureBranchSession(ctx context.Context, req *connect.Request[wsv1.EnsureBranchSessionRequest]) (*connect.Response[wsv1.EnsureBranchSessionResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	org, repo, branch := req.Msg.GetOrg(), req.Msg.GetRepo(), req.Msg.GetBranch()
	if branch == "" {
		branch = roles.MainBranch
	}
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) || !roles.ValidComponent(branch) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo/branch must be simple names"))
	}
	if err := s.claimRepo(ctx, tenant, org, repo); err != nil {
		return nil, err
	}

	session := roles.SessionName(org, repo, branch)
	role := roles.RoleForBranch(branch)
	// Idempotent: create only when absent. The agent refuses a duplicate, so we
	// probe first (GetSession -> NotFound means absent).
	if !s.sessionExists(ctx, req.Header(), session) {
		if err := s.createSession(ctx, req.Header(), session, roles.PresetFor(role), req.Msg.GetModel(), req.Msg.GetLocale()); err != nil {
			return nil, err
		}
	}
	sbName, sbPhase, refs := s.sandboxFields(ctx, session)
	return connect.NewResponse(&wsv1.EnsureBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Org: org, Repo: repo, Branch: branch,
		Role: string(role), Preset: roles.PresetFor(role),
		Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
	}}), nil
}

// sessionExists reports whether the agent already holds a session with this id.
func (s *Service) sessionExists(ctx context.Context, hdr map[string][]string, session string) bool {
	r := connect.NewRequest(&agentv1.GetSessionRequest{Id: session})
	copyHeaders(r, hdr)
	res, err := s.agent.GetSession(ctx, r)
	return err == nil && res.Msg.GetSession() != nil
}

// claimOrg ensures `org` exists and is owned by tenant. A pre-existing org not
// owned by the tenant is refused (import must not land in someone else's org).
func (s *Service) claimOrg(ctx context.Context, tenant, org string) error {
	owned, err := s.members.OwnsOrg(tenant, org)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if owned {
		return nil
	}
	// An org is GLOBALLY unique: if another tenant already owns it (or it exists
	// in Forgejo under a different owner), refuse rather than double-book it.
	if other, err := s.members.OrgOwner(org); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	} else if other != "" && other != tenant {
		return connect.NewError(connect.CodePermissionDenied, errors.New("organization is owned by another tenant"))
	}
	if err := s.git.EnsureOrg(ctx, org); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("ensure org: %w", err))
	}
	if err := s.members.AddOrg(tenant, org); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	return nil
}

// claimRepo ensures org/repo exists and is owned by tenant. A pre-existing repo
// owned by another tenant is refused.
func (s *Service) claimRepo(ctx context.Context, tenant, org, repo string) error {
	exists, err := s.git.RepoExists(ctx, org, repo)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if exists {
		owned, err := s.members.OwnsRepo(tenant, org, repo)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if !owned {
			return connect.NewError(connect.CodePermissionDenied, errors.New("repository is owned by another tenant"))
		}
		return nil
	}
	if _, err := s.git.EnsureRepo(ctx, org, repo); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("ensure repo: %w", err))
	}
	if err := s.members.AddRepo(tenant, org, repo); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.members.AddOrg(tenant, org)
	return nil
}

// ForkBranchSession creates `org:repo:<branch>` as a NEW branch session forked
// from a parent session. org/repo come from the parent's name; `branch` must be
// a legal, non-main name. The preset is FORCED to developer (a fork can never
// inherit/escalate a role), and the agent publishes a `forked` lifecycle event
// the workspace-extension uses to materialize the branch from the parent's.
//
// The parent session MUST exist (branch <-> session is 1:1; the caller derives
// it from an existing branch session).
func (s *Service) ForkBranchSession(ctx context.Context, req *connect.Request[wsv1.ForkBranchSessionRequest]) (*connect.Response[wsv1.ForkBranchSessionResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	org, repo, _, ok := roles.ParseSession(req.Msg.GetSession())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session must be org:repo:branch"))
	}
	branch := req.Msg.GetBranch()
	if branch == "" || !roles.ValidComponent(branch) || branch == roles.MainBranch {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("branch must be a legal, non-main name"))
	}
	owned, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("branch session not found"))
	}
	if !s.sessionExists(ctx, req.Header(), req.Msg.GetSession()) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("parent session not found"))
	}

	session := roles.SessionName(org, repo, branch)
	role := roles.Developer
	fr := connect.NewRequest(&agentv1.ForkRequest{
		Id:        req.Msg.GetSession(),
		Name:      session,
		MessageId: req.Msg.GetMessageId(),
		Preset:    roles.PresetFor(role),
	})
	copyHeaders(fr, req.Header())
	if _, err := s.agent.Fork(ctx, fr); err != nil {
		return nil, err
	}
	sbName, sbPhase, refs := s.sandboxFields(ctx, session)
	return connect.NewResponse(&wsv1.ForkBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Org: org, Repo: repo, Branch: branch,
		Role: string(role), Preset: roles.PresetFor(role),
		Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
	}}), nil
}

// CreateFreeSession creates a standalone (non-repo-bound) session. Free
// sessions carry a tenant-scoped role: admin (manage org/repo) or explorer
// (read-only). Any number of each may exist; the role decides what the session
// may DO, never what it may SEE (visibility is the tenant).
func (s *Service) CreateFreeSession(ctx context.Context, req *connect.Request[wsv1.CreateFreeSessionRequest]) (*connect.Response[wsv1.CreateFreeSessionResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	role := roles.Role(req.Msg.GetRole())
	if role != roles.Admin && role != roles.Explorer {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("role must be admin|explorer"))
	}
	name := req.Msg.GetName()
	if name == "" || !roles.ValidComponent(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name required (simple)"))
	}
	if err := s.members.AddFreeSession(tenant, name, string(role)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.createSession(ctx, req.Header(), name, roles.PresetFor(role), req.Msg.GetModel(), req.Msg.GetLocale()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wsv1.CreateFreeSessionResponse{Session: name, Preset: roles.PresetFor(role)}), nil
}

// createSession forwards a trusted CreateSession to the agent. `locale`, when
// non-empty ("zh"/"en"), pins the session's agent language for its lifetime.
//
// An EMPTY locale means "follow the tenant default". We resolve the tenant's
// configured `locale` HERE and pass it explicitly, because the agent stores a
// pinned locale and would otherwise pin an empty request to English (the
// agent's normalizeLocale(”) == 'en'), ignoring the tenant's zh config.
func (s *Service) createSession(ctx context.Context, hdr map[string][]string, name, preset, model, locale string) error {
	if strings.TrimSpace(locale) == "" {
		locale = s.tenantLocale(ctx, hdr)
	}
	msg := &agentv1.CreateSessionRequest{Name: name, Preset: preset, Model: model, Locale: normalizeLocale(locale)}
	r := connect.NewRequest(msg)
	copyHeaders(r, hdr)
	_, err := s.agent.CreateSession(ctx, r)
	return err
}

// tenantLocale reads the tenant's configured agent language (`locale` KV),
// forwarding the caller's credential. Empty on any error (the agent then falls
// back to its own default).
func (s *Service) tenantLocale(ctx context.Context, hdr map[string][]string) string {
	r := connect.NewRequest(&agentv1.GetConfigRequest{Key: "locale"})
	copyHeaders(r, hdr)
	res, err := s.agent.GetConfig(ctx, r)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Msg.GetValue())
}

// normalizeLocale keeps only a valid pinned language ("zh"/"en"); anything else
// (including empty) becomes "" so the agent falls back to the tenant default.
func normalizeLocale(l string) string {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "zh":
		return "zh"
	case "en":
		return "en"
	}
	return ""
}

// ListBranchSessions lists the tenant's repo-bound branch sessions + free
// sessions, derived from the agent's own session list (the agent is the source
// of truth; the gateway owns only repo ownership + free-session roles).
func (s *Service) ListBranchSessions(ctx context.Context, req *connect.Request[wsv1.ListBranchSessionsRequest]) (*connect.Response[wsv1.ListBranchSessionsResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	lr := connect.NewRequest(&agentv1.ListSessionsRequest{})
	copyHeaders(lr, req.Header())
	res, err := s.agent.ListSessions(ctx, lr)
	if err != nil {
		return nil, err
	}

	// Sandbox per session (best effort). A session may own SEVERAL sandboxes
	// (named freely by the model), so attribute by the sandbox's SESSION
	// annotation — never by its name — and return the full list (representative
	// first). ONE list call, reused for every session.
	var all []sandboxmgr.Sandbox
	if sbxs, err := s.sbx.List(ctx); err == nil {
		all = sbxs
	}
	refsFor := func(session string) []*wsv1.SandboxRef {
		return sessionSandboxes(all, session)
	}
	repOf := func(refs []*wsv1.SandboxRef) (string, string) {
		if len(refs) == 0 {
			return "", ""
		}
		return refs[0].GetName(), refs[0].GetPhase()
	}

	out := []*wsv1.BranchSession{}
	for _, sess := range res.Msg.GetSessions() {
		name := sess.GetName()
		refs := refsFor(name)
		sbName, sbPhase := repOf(refs)
		if org, repo, branch, ok := roles.ParseSession(name); ok {
			owned, err := s.members.OwnsRepo(tenant, org, repo)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if !owned {
				continue
			}
			role := roles.RoleForBranch(branch)
			out = append(out, &wsv1.BranchSession{
				Session: name, Org: org, Repo: repo, Branch: branch,
				Role: string(role), Preset: roles.PresetFor(role),
				Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
			})
			continue
		}
		if role, ok, _ := s.members.FreeRole(tenant, name); ok {
			out = append(out, &wsv1.BranchSession{
				Session: name, Role: role, Preset: role,
				Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
			})
		}
	}
	return connect.NewResponse(&wsv1.ListBranchSessionsResponse{BranchSessions: out}), nil
}

// GetBranchSession returns one branch session (or free session).
func (s *Service) GetBranchSession(ctx context.Context, req *connect.Request[wsv1.GetBranchSessionRequest]) (*connect.Response[wsv1.GetBranchSessionResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	session := req.Msg.GetSession()
	if org, repo, branch, ok := roles.ParseSession(session); ok {
		owned, err := s.members.OwnsRepo(tenant, org, repo)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if !owned {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("branch session not found"))
		}
		role := roles.RoleForBranch(branch)
		sbName, sbPhase, refs := s.sandboxFields(ctx, session)
		return connect.NewResponse(&wsv1.GetBranchSessionResponse{BranchSession: &wsv1.BranchSession{
			Session: session, Org: org, Repo: repo, Branch: branch,
			Role: string(role), Preset: roles.PresetFor(role),
			Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
		}}), nil
	}
	role, ok, _ := s.members.FreeRole(tenant, session)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("branch session not found"))
	}
	sbName, sbPhase, refs := s.sandboxFields(ctx, session)
	return connect.NewResponse(&wsv1.GetBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Role: role, Preset: role,
		Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
	}}), nil
}

// DeleteBranchSession deletes a branch session (its sandbox + agent session) or
// a free session. It does NOT delete the underlying git branch.
func (s *Service) DeleteBranchSession(ctx context.Context, req *connect.Request[wsv1.DeleteBranchSessionRequest]) (*connect.Response[wsv1.DeleteBranchSessionResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	session := req.Msg.GetSession()
	if org, repo, _, ok := roles.ParseSession(session); ok {
		if owned, _ := s.members.OwnsRepo(tenant, org, repo); !owned {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("branch session not found"))
		}
	} else if _, ok, _ := s.members.FreeRole(tenant, session); !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("branch session not found"))
	}
	// Best-effort sandbox cleanup + session delete.
	s.deleteSessionCascade(ctx, req.Header(), tenant, session)
	return connect.NewResponse(&wsv1.DeleteBranchSessionResponse{Ok: true}), nil
}

// DeleteBranch removes a repo branch AND its branch session (session + sandboxes
// cascade). Only the owning tenant may delete; `main` is refused (the default
// branch is protected). The git deletion is idempotent (an already-absent
// branch is fine); the branch session is deleted even if the branch was gone.
func (s *Service) DeleteBranch(ctx context.Context, req *connect.Request[wsv1.DeleteBranchRequest]) (*connect.Response[wsv1.DeleteBranchResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	org, repo, branch := req.Msg.GetOrg(), req.Msg.GetRepo(), req.Msg.GetBranch()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) || !roles.ValidComponent(branch) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo/branch must be simple names"))
	}
	if branch == roles.MainBranch {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("refusing to delete the default branch"))
	}
	owned, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
	}
	// Delete the branch session (session + sandboxes) first, then the git branch.
	session := roles.SessionName(org, repo, branch)
	s.deleteSessionCascade(ctx, req.Header(), tenant, session)
	if err := s.git.DeleteBranch(ctx, org, repo, branch); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeleteBranchResponse{Ok: true}), nil
}

// DeleteRepo removes a repository AND every one of its branch sessions (each
// cascading to its sandboxes), then drops the git repo + ownership row. Only
// the owning tenant may delete. The org is left in place. Admin action.
func (s *Service) DeleteRepo(ctx context.Context, req *connect.Request[wsv1.DeleteRepoRequest]) (*connect.Response[wsv1.DeleteRepoResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	org, repo := req.Msg.GetOrg(), req.Msg.GetRepo()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo must be simple names"))
	}
	owned, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
	}
	// Cascade every branch session of this repo (the agent is the source of
	// truth for which sessions exist). Best-effort per session.
	prefix := org + ":" + repo + ":"
	lr := connect.NewRequest(&agentv1.ListSessionsRequest{})
	copyHeaders(lr, req.Header())
	if res, err := s.agent.ListSessions(ctx, lr); err == nil {
		for _, sess := range res.Msg.GetSessions() {
			if name := sess.GetName(); strings.HasPrefix(name, prefix) {
				s.deleteSessionCascade(ctx, req.Header(), tenant, name)
			}
		}
	}
	// Deleting a REPO removes ALL of its services — preview AND release — whose
	// session belongs to this repo (a release service normally outlives its
	// session, but not its repo).
	if s.services != nil {
		if all, err := s.services.List(ctx); err == nil {
			for _, svc := range all {
				if strings.HasPrefix(svc.Session, prefix) {
					_, _ = s.services.Delete(ctx, svc.Name)
				}
			}
		}
	}
	// Delete the git repo (idempotent) and the ownership row.
	if err := s.git.DeleteRepo(ctx, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.members.RemoveRepo(tenant, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeleteRepoResponse{Ok: true}), nil
}

// deleteSessionCascade is the SINGLE session-deletion path: it reclaims the
// session's sandboxes, deletes the agent session, and clears a free-session row.
// Every delete entry point (DeleteBranchSession / DeleteBranch / DeleteRepo)
// funnels through here so the semantics never drift.
//
// Sandboxes are matched by their `worker-manager/session` annotation (the ONLY
// reliable key: the creator annotation holds just the tenant). A sandbox with
// no session annotation falls back to a name match for legacy sandboxes.
func (s *Service) deleteSessionCascade(ctx context.Context, hdr map[string][]string, tenant, session string) {
	if s.sbx != nil {
		if sbxs, err := s.sbx.List(ctx); err == nil {
			for _, sb := range sbxs {
				owned := sb.Creator == "" || sb.Creator == tenant
				if !owned {
					continue
				}
				if sb.Session == session || (sb.Session == "" && sb.Name == session) {
					_, _ = s.sbx.Delete(ctx, sb.Name)
				}
			}
		}
	}
	// Reclaim the session's PREVIEW services (a release service outlives its
	// session). Best-effort.
	if s.services != nil && session != "" {
		_, _ = s.services.DeleteBySession(ctx, session, true)
	}
	r := connect.NewRequest(&agentv1.DeleteSessionRequest{Id: session})
	copyHeaders(r, hdr)
	_, _ = s.agent.DeleteSession(ctx, r)
	_ = s.members.DeleteFreeSession(tenant, session)
}

// ---- git browse ----

func (s *Service) ensureVisible(ctx context.Context, hdr map[string][]string, org, repo string) error {
	tenant, err := s.resolveTenant(ctx, hdr)
	if err != nil {
		return err
	}
	ok, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
	}
	return nil
}

// ListRepos lists the tenant's visible repos.
func (s *Service) ListRepos(ctx context.Context, req *connect.Request[wsv1.ListReposRequest]) (*connect.Response[wsv1.ListReposResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	repos, err := s.members.ListRepos(tenant)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.RepoInfo, 0, len(repos))
	for _, rp := range repos {
		info, gerr := s.git.GetRepo(ctx, rp[0], rp[1])
		db := roles.MainBranch
		priv := true
		if gerr == nil {
			if info.DefaultBranch != "" {
				db = info.DefaultBranch
			}
			priv = info.Private
		}
		out = append(out, &wsv1.RepoInfo{Org: rp[0], Repo: rp[1], DefaultBranch: db, Private: priv})
	}
	return connect.NewResponse(&wsv1.ListReposResponse{Repos: out}), nil
}

func (s *Service) Tree(ctx context.Context, req *connect.Request[wsv1.TreeRequest]) (*connect.Response[wsv1.TreeResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	entries, err := s.git.Tree(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.TreeEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &wsv1.TreeEntry{Path: e.Path, Type: e.Type, Size: e.Size})
	}
	return connect.NewResponse(&wsv1.TreeResponse{Entries: out}), nil
}

func (s *Service) ReadBlob(ctx context.Context, req *connect.Request[wsv1.ReadBlobRequest]) (*connect.Response[wsv1.ReadBlobResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	content, sha, err := s.git.ReadBlob(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.ReadBlobResponse{Content: content, Sha: sha}), nil
}

func (s *Service) Log(ctx context.Context, req *connect.Request[wsv1.LogRequest]) (*connect.Response[wsv1.LogResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	commits, err := s.git.Log(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath(), int(m.GetLimit()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.CommitInfo, 0, len(commits))
	for _, c := range commits {
		out = append(out, &wsv1.CommitInfo{Sha: c.SHA, Message: c.Message, Author: c.Author, Date: c.Date})
	}
	return connect.NewResponse(&wsv1.LogResponse{Commits: out}), nil
}

func (s *Service) Branches(ctx context.Context, req *connect.Request[wsv1.BranchesRequest]) (*connect.Response[wsv1.BranchesResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	branches, err := s.git.Branches(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.BranchInfo, 0, len(branches))
	for _, b := range branches {
		out = append(out, &wsv1.BranchInfo{Name: b.Name, Sha: b.SHA})
	}
	return connect.NewResponse(&wsv1.BranchesResponse{Branches: out}), nil
}

// ReadRaw returns a file's raw bytes (any content type) so the webui can
// preview binary files (images / PDF / office) with the chat's viewers.
//
// It also classifies the content: `is_text` is true only for valid UTF-8
// without NUL bytes AND at most maxTextBytes (larger files are returned as
// non-text so the client offers a download instead of highlighting).
func (s *Service) ReadRaw(ctx context.Context, req *connect.Request[wsv1.ReadRawRequest]) (*connect.Response[wsv1.ReadRawResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	data, sha, err := s.git.RawFile(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.ReadRawResponse{
		Data: data, Sha: sha, Mime: mimeOfPath(m.GetPath()),
		IsText: isTextContent(data),
	}), nil
}

// maxTextBytes caps what the client will syntax-highlight / virtualize. Larger
// files are classified non-text (download only).
const maxTextBytes = 1 << 20 // 1 MiB

// isTextContent reports whether data is plain UTF-8 text worth highlighting.
func isTextContent(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if len(data) > maxTextBytes {
		return false
	}
	// A NUL byte in the first 8 KiB is the classic binary signal.
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	for _, b := range sample {
		if b == 0 {
			return false
		}
	}
	return utf8.Valid(data)
}

// Tags lists a repository's tags.
func (s *Service) Tags(ctx context.Context, req *connect.Request[wsv1.TagsRequest]) (*connect.Response[wsv1.TagsResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	tags, err := s.git.Tags(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.TagInfo, 0, len(tags))
	for _, t := range tags {
		out = append(out, &wsv1.TagInfo{Name: t.Name, Sha: t.SHA})
	}
	return connect.NewResponse(&wsv1.TagsResponse{Tags: out}), nil
}

// ListReleases lists a repository's releases (read-only).
func (s *Service) ListReleases(ctx context.Context, req *connect.Request[wsv1.ReleasesRequest]) (*connect.Response[wsv1.ReleasesResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	rels, err := s.git.ListReleases(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.ReleaseInfo, 0, len(rels))
	for _, r := range rels {
		assets := make([]*wsv1.ReleaseAsset, 0, len(r.Assets))
		for _, a := range r.Assets {
			assets = append(assets, &wsv1.ReleaseAsset{
				Id: a.ID, Name: a.Name, Size: a.Size, DownloadCount: a.DownloadCount,
				ReleaseId: r.ID,
			})
		}
		out = append(out, &wsv1.ReleaseInfo{
			Id: r.ID, TagName: r.TagName, Name: r.Name, Body: r.Body,
			Draft: r.Draft, Prerelease: r.Prerelease, Author: r.Author,
			CreatedAt: r.CreatedAt, PublishedAt: r.PublishedAt, HtmlUrl: r.HTMLURL,
			Assets: assets,
		})
	}
	return connect.NewResponse(&wsv1.ReleasesResponse{Releases: out}), nil
}

// GetReleaseAsset proxies one release asset's bytes (the browser may not reach
// the git host directly).
func (s *Service) GetReleaseAsset(ctx context.Context, req *connect.Request[wsv1.ReleaseAssetRequest]) (*connect.Response[wsv1.ReleaseAssetResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	data, name, mime, err := s.git.GetReleaseAsset(ctx, m.GetOrg(), m.GetRepo(), m.GetReleaseId(), m.GetAssetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.ReleaseAssetResponse{Data: data, Name: name, Mime: mime}), nil
}

// GetCommit returns one commit's metadata.
func (s *Service) GetCommit(ctx context.Context, req *connect.Request[wsv1.GetCommitRequest]) (*connect.Response[wsv1.GetCommitResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	c, err := s.git.GetCommit(ctx, m.GetOrg(), m.GetRepo(), m.GetSha())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.GetCommitResponse{Commit: &wsv1.CommitDetail{
		Sha: c.SHA, Message: c.Message, Author: c.Author, AuthorEmail: c.AuthorEmail,
		Date: c.Date, Parents: c.Parents, HtmlUrl: c.HTMLURL,
	}}), nil
}

// CommitDiff returns the unified diff of one commit.
func (s *Service) CommitDiff(ctx context.Context, req *connect.Request[wsv1.CommitDiffRequest]) (*connect.Response[wsv1.DiffResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	diff, err := s.git.CommitDiff(ctx, m.GetOrg(), m.GetRepo(), m.GetSha())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.DiffResponse{Diff: diff}), nil
}

// GetMR returns one change request's full detail.
func (s *Service) GetMR(ctx context.Context, req *connect.Request[wsv1.GetMRRequest]) (*connect.Response[wsv1.GetMRResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mr, err := s.git.GetMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.GetMRResponse{Mr: toMRInfo(mr)}), nil
}

// MRDiff returns the unified diff of one change request.
func (s *Service) MRDiff(ctx context.Context, req *connect.Request[wsv1.MRDiffRequest]) (*connect.Response[wsv1.MRDiffResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	diff, err := s.git.MRDiff(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.MRDiffResponse{Diff: diff}), nil
}

// ListMRComments lists the (read-only) MR conversation.
func (s *Service) ListMRComments(ctx context.Context, req *connect.Request[wsv1.ListMRCommentsRequest]) (*connect.Response[wsv1.ListMRCommentsResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	comments, err := s.git.MRComments(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.MRCommentInfo, 0, len(comments))
	for _, c := range comments {
		out = append(out, &wsv1.MRCommentInfo{
			Id: c.ID, Author: c.Author, Body: c.Body,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	return connect.NewResponse(&wsv1.ListMRCommentsResponse{Comments: out}), nil
}

// EnsureRepo creates the org/repo (protecting main) and records ownership.
// Admin-only: creating org/repo is a gateway-verified admin action.
func (s *Service) EnsureRepo(ctx context.Context, req *connect.Request[wsv1.EnsureRepoRequest]) (*connect.Response[wsv1.EnsureRepoResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.requireAdmin(tenant); err != nil {
		return nil, err
	}
	org, repo := req.Msg.GetOrg(), req.Msg.GetRepo()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo must be simple names"))
	}
	created, err := s.git.EnsureRepo(ctx, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.members.AddRepo(tenant, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Auto-create the `org:repo:main` branch session (repo:branch <-> session,
	// 1:1). Idempotent: a re-ensure leaves an existing session untouched.
	s.ensureMainSession(ctx, req.Header(), org, repo)
	return connect.NewResponse(&wsv1.EnsureRepoResponse{Created: created}), nil
}

// CreateOrg creates an organization owned by the caller's tenant (idempotent:
// an org already owned is returned unchanged). Admin action.
func (s *Service) CreateOrg(ctx context.Context, req *connect.Request[wsv1.CreateOrgRequest]) (*connect.Response[wsv1.CreateOrgResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.requireAdmin(tenant); err != nil {
		return nil, err
	}
	org := req.Msg.GetOrg()
	if !roles.ValidComponent(org) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org must be a simple name"))
	}
	if err := s.claimOrg(ctx, tenant, org); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wsv1.CreateOrgResponse{Org: org}), nil
}

// ListOrgs lists the caller's tenant-owned orgs (including empty ones, which
// ListRepos cannot surface).
func (s *Service) ListOrgs(ctx context.Context, req *connect.Request[wsv1.ListOrgsRequest]) (*connect.Response[wsv1.ListOrgsResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	orgs, err := s.members.ListOrgs(tenant)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.ListOrgsResponse{Orgs: orgs}), nil
}

// ---- tenant-scoped repo operations (the extension's ONLY path to Forgejo) ----
//
// Every one of these first calls ensureVisible, so a tenant can only touch a
// repo it owns. This replaces the extension's former direct Forgejo calls made
// with a SHARED admin token (which exposed every tenant's repos).

// RepoMeta returns one repository's metadata (ownership-checked).
func (s *Service) RepoMeta(ctx context.Context, req *connect.Request[wsv1.RepoMetaRequest]) (*connect.Response[wsv1.RepoMetaResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	info, err := s.git.GetRepo(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.RepoMetaResponse{
		Org: info.Org, Repo: info.Repo, DefaultBranch: info.DefaultBranch, Private: info.Private, Empty: info.Empty,
	}), nil
}

// Contents reads a file (text) or lists a directory at ref/path.
func (s *Service) Contents(ctx context.Context, req *connect.Request[wsv1.ContentsRequest]) (*connect.Response[wsv1.ContentsResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	isDir, text, sha, size, entries, err := s.git.Contents(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, mrError(err)
	}
	res := &wsv1.ContentsResponse{IsDir: isDir, Text: text, Sha: sha, Size: size}
	for _, e := range entries {
		res.Entries = append(res.Entries, &wsv1.ContentDirEntry{Path: e.Path, Name: e.Name, Type: e.Type, Size: e.Size, Sha: e.SHA})
	}
	return connect.NewResponse(res), nil
}

// CommitFiles creates one commit of several file operations on a branch.
func (s *Service) CommitFiles(ctx context.Context, req *connect.Request[wsv1.CommitFilesRequest]) (*connect.Response[wsv1.CommitFilesResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if len(m.GetFiles()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("files is required"))
	}
	ops := make([]forgejo.FileOp, 0, len(m.GetFiles()))
	for _, f := range m.GetFiles() {
		ops = append(ops, forgejo.FileOp{
			Path: f.GetPath(), Op: f.GetOperation(), Content: f.GetContent(),
			Bytes: f.GetContentBytes(), SHA: f.GetSha(), FromPath: f.GetFromPath(),
		})
	}
	sha, err := s.git.CommitFiles(ctx, m.GetOrg(), m.GetRepo(), m.GetMessage(), m.GetRef(), m.GetNewBranch(), ops)
	if err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.CommitFilesResponse{Sha: sha}), nil
}

// Compare returns per-file patches between two refs.
func (s *Service) Compare(ctx context.Context, req *connect.Request[wsv1.CompareRequest]) (*connect.Response[wsv1.CompareResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	// Compute the compare with go-git: Forgejo's compare API returns an EMPTY
	// `patch` field on 1.22, so the gateway produces the full per-file diff.
	files, err := gitcommit.Compare(ctx, s.git.GitURL(m.GetOrg(), m.GetRepo()), "root", s.git.Token(), m.GetBase(), m.GetHead(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.CompareFile, 0, len(files))
	for _, f := range files {
		out = append(out, &wsv1.CompareFile{
			Path: f.Path, Status: f.Status,
			Additions: int32(f.Additions), Deletions: int32(f.Deletions), Patch: f.Patch,
		})
	}
	return connect.NewResponse(&wsv1.CompareResponse{Files: out}), nil
}

// CreateTag creates a tag at a target ref.
func (s *Service) CreateTag(ctx context.Context, req *connect.Request[wsv1.CreateTagRequest]) (*connect.Response[wsv1.CreateTagResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.git.CreateTagAt(ctx, m.GetOrg(), m.GetRepo(), m.GetName(), m.GetTarget()); err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.CreateTagResponse{Ok: true}), nil
}

// CreateBranch creates a branch from an existing ref.
func (s *Service) CreateBranch(ctx context.Context, req *connect.Request[wsv1.CreateBranchRequest]) (*connect.Response[wsv1.CreateBranchResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.git.CreateBranch(ctx, m.GetOrg(), m.GetRepo(), m.GetName(), m.GetFrom()); err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.CreateBranchResponse{Ok: true}), nil
}

// Archive returns a repo tree at a ref as a tar.gz (sandbox checkout).
func (s *Service) Archive(ctx context.Context, req *connect.Request[wsv1.ArchiveRequest]) (*connect.Response[wsv1.ArchiveResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	data, err := s.git.ArchiveTarGz(ctx, m.GetOrg(), m.GetRepo(), m.GetRef())
	if err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.ArchiveResponse{Data: data}), nil
}

// ensureMainSession idempotently creates the `org:repo:main` session bound to
// the maintainer role. Best-effort: a repo is still usable if this fails.
func (s *Service) ensureMainSession(ctx context.Context, hdr map[string][]string, org, repo string) {
	session := roles.SessionName(org, repo, roles.MainBranch)
	if s.sessionExists(ctx, hdr, session) {
		return
	}
	if err := s.createSession(ctx, hdr, session, roles.PresetFor(roles.Maintainer), "", ""); err != nil {
		log.Printf("warn: ensure main session %s: %v", session, err)
	}
}

// ImportRepo migrates an EXTERNAL git repository into `org` (which must belong
// to the caller's tenant). `ref` selects the SOURCE ref to import — a branch,
// a tag, or any revision — and ALWAYS lands on the new repo's `main`; an empty
// `ref` imports the source's HEAD branch. Only `main` is created (no other
// branches/tags are carried over). The imported repo is PUBLIC and its main
// branch session is ensured. An existing repo is refused (never overwritten).
func (s *Service) ImportRepo(ctx context.Context, req *connect.Request[wsv1.ImportRepoRequest]) (*connect.Response[wsv1.ImportRepoResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	org, url := m.GetOrg(), m.GetUrl()
	if !roles.ValidComponent(org) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org must be a simple name"))
	}
	if url == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("url is required"))
	}
	repo := m.GetRepo()
	if repo == "" {
		repo = deriveRepoName(url)
	}
	if !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("repo must be a simple name"))
	}
	// The org must belong to the tenant (an unknown org is created for it).
	if err := s.claimOrg(ctx, tenant, org); err != nil {
		return nil, err
	}
	// Refuse to overwrite.
	if ok, err := s.git.RepoExists(ctx, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	} else if ok {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("repository already exists"))
	}

	// Create an EMPTY, PUBLIC destination repo (no auto-init): the import push
	// will establish `main`. Never use Forgejo's mirror-migrate here — it
	// fetches every `refs/pull/*`, which is multi-GB/multi-minute on a popular
	// upstream (e.g. octocat/Spoon-Knife with 63k pull refs). Every repo this
	// deployment creates is public.
	if _, err := s.git.CreateEmptyRepo(ctx, org, repo, m.GetDescription()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err))
	}
	// From here a failure must not leave an empty orphan behind.
	cleanup := func(cause error) error {
		if derr := s.git.DeleteRepo(context.WithoutCancel(ctx), org, repo); derr != nil {
			log.Printf("warn: cleanup half-imported %s/%s: %v", org, repo, derr)
		}
		return connect.NewError(connect.CodeInternal, cause)
	}

	// Clone the source's HEAD BRANCHES only and push them into the empty repo.
	// go-git keeps `git` out of the gateway image and opens no subprocess.
	res, err := gitimport.Import(ctx, gitimport.Options{
		SourceURL:   url,
		DestURL:     s.git.GitURL(org, repo),
		DestUser:    "root",
		DestToken:   s.git.Token(),
		SourceUser:  m.GetAuthUser(),
		SourceToken: m.GetAuthToken(),
		Ref:         m.GetRef(),
	})
	if err != nil {
		return nil, cleanup(fmt.Errorf("import: %w", err))
	}
	branch := res.DefaultBranch
	if branch == "" {
		branch = roles.MainBranch
	}
	// Point the repo's default branch at the imported one (the empty repo has
	// none; Forgejo would otherwise guess).
	if err := s.git.SetDefaultBranch(ctx, org, repo, branch); err != nil {
		return nil, cleanup(fmt.Errorf("set default branch: %w", err))
	}
	// Record ownership so the repo becomes visible to the tenant, protect main
	// only when the default IS main, and ensure the branch session.
	if err := s.members.AddRepo(tenant, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if branch == roles.MainBranch {
		if err := s.git.ProtectMain(ctx, org, repo); err != nil {
			log.Printf("warn: protect main %s/%s: %v", org, repo, err)
		}
	}
	if !s.sessionExists(ctx, req.Header(), roles.SessionName(org, repo, branch)) {
		if err := s.createSession(ctx, req.Header(), roles.SessionName(org, repo, branch), roles.PresetFor(roles.RoleForBranch(branch)), "", ""); err != nil {
			log.Printf("warn: ensure imported session %s/%s:%s: %v", org, repo, branch, err)
		}
	}
	return connect.NewResponse(&wsv1.ImportRepoResponse{Repo: &wsv1.RepoInfo{
		Org: org, Repo: repo, DefaultBranch: branch, Private: false,
	}}), nil
}

// ---- push mirrors (admin) ----

// SetPushMirror registers a push mirror on a repo the caller's tenant owns.
// HTTPS only. A repo may hold multiple mirrors (Forgejo assigns each a
// remote_name, returned to the caller). Admin action.
func (s *Service) SetPushMirror(ctx context.Context, req *connect.Request[wsv1.SetPushMirrorRequest]) (*connect.Response[wsv1.SetPushMirrorResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	addr := strings.TrimSpace(m.GetRemoteAddress())
	if !validPushMirrorAddress(addr) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("remote_address must be an http(s) git URL"))
	}
	if iv := strings.TrimSpace(m.GetInterval()); iv != "" && !validInterval(iv) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("interval must be a duration like 8h or 30m"))
	}
	mirror, err := s.git.SetPushMirror(ctx, m.GetOrg(), m.GetRepo(), forgejo.PushMirrorOptions{
		RemoteAddress:  addr,
		RemoteUsername: m.GetRemoteUsername(),
		RemotePassword: m.GetRemotePassword(),
		SyncOnCommit:   m.GetSyncOnCommit(),
		Interval:       strings.TrimSpace(m.GetInterval()),
		BranchFilter:   strings.TrimSpace(m.GetBranchFilter()),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.SetPushMirrorResponse{Mirror: toPushMirrorInfo(mirror)}), nil
}

// ListPushMirrors lists the push mirrors of a repo the caller's tenant owns.
func (s *Service) ListPushMirrors(ctx context.Context, req *connect.Request[wsv1.ListPushMirrorsRequest]) (*connect.Response[wsv1.ListPushMirrorsResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mirrors, err := s.git.ListPushMirrors(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.PushMirrorInfo, 0, len(mirrors))
	for _, pm := range mirrors {
		out = append(out, toPushMirrorInfo(pm))
	}
	return connect.NewResponse(&wsv1.ListPushMirrorsResponse{Mirrors: out}), nil
}

// DeletePushMirror removes a push mirror (by remote_name) from a repo the
// caller's tenant owns. Admin action.
func (s *Service) DeletePushMirror(ctx context.Context, req *connect.Request[wsv1.DeletePushMirrorRequest]) (*connect.Response[wsv1.DeletePushMirrorResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(m.GetRemoteName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("remote_name is required"))
	}
	if err := s.git.DeletePushMirror(ctx, m.GetOrg(), m.GetRepo(), name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeletePushMirrorResponse{Ok: true}), nil
}

// validPushMirrorAddress accepts an http(s) git URL (SSH mirrors are not
// exposed by this surface).
func validPushMirrorAddress(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// validInterval accepts a Go duration string with a unit (e.g. 8h, 30m, 1h30m).
func validInterval(s string) bool {
	d, err := time.ParseDuration(s)
	return err == nil && d > 0
}

func toPushMirrorInfo(pm forgejo.PushMirror) *wsv1.PushMirrorInfo {
	return &wsv1.PushMirrorInfo{
		RemoteName:    pm.RemoteName,
		RemoteAddress: pm.RemoteAddress,
		Interval:      pm.Interval,
		SyncOnCommit:  pm.SyncOnCommit,
		BranchFilter:  pm.BranchFilter,
		LastError:     pm.LastError,
		LastUpdate:    pm.LastUpdate,
		Created:       pm.Created,
	}
}

// deriveRepoName takes the last path segment of a git URL (strips `.git`).
func deriveRepoName(raw string) string {
	s := strings.TrimRight(raw, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// requireAdmin gates org/repo creation. Every tenant may create org/repo
// (names are unique); this hook exists so a stricter policy can be added here.
func (s *Service) requireAdmin(_ string) error { return nil }

// ---- change requests ----

func (s *Service) ListMRs(ctx context.Context, req *connect.Request[wsv1.ListMRsRequest]) (*connect.Response[wsv1.ListMRsResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mrs, err := s.git.ListMRs(ctx, m.GetOrg(), m.GetRepo(), m.GetState())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.MRInfo, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, toMRInfo(mr))
	}
	return connect.NewResponse(&wsv1.ListMRsResponse{Mrs: out}), nil
}

// CreateMR is a developer action (propose). A maintainer cannot open MRs.
//
// GATE: a branch whose changed files still carry unresolved conflict markers
// (from SyncBranch) is refused — the markers must not reach a reviewer.
func (s *Service) CreateMR(ctx context.Context, req *connect.Request[wsv1.CreateMRRequest]) (*connect.Response[wsv1.CreateMRResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	base := m.GetBase()
	if base == "" {
		base = roles.MainBranch
	}
	if conflicts, err := s.unresolvedConflicts(ctx, m.GetOrg(), m.GetRepo(), m.GetHead()); err != nil {
		return nil, err
	} else if len(conflicts) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"branch %s has unresolved conflict markers in: %s; resolve them (see the ABCP-CONFLICT blocks) before opening an MR",
			m.GetHead(), strings.Join(conflicts, ", ")))
	}
	if err := s.stagingGate(ctx, m.GetOrg(), m.GetRepo(), m.GetHead()); err != nil {
		return nil, err
	}
	index, url, err := s.git.CreateMR(ctx, m.GetOrg(), m.GetRepo(), m.GetTitle(), m.GetHead(), base, m.GetBody())
	if err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.CreateMRResponse{Index: index, Url: url}), nil
}

func (s *Service) CommentMR(ctx context.Context, req *connect.Request[wsv1.CommentMRRequest]) (*connect.Response[wsv1.CommentMRResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.git.CommentMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex(), m.GetBody()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.CommentMRResponse{Ok: true}), nil
}

// MergeMR is a MAINTAINER action: the only way main changes.
//
// GATE: refuse when the head branch still carries conflict markers. Git judges
// "mergeable" purely by topology, so a branch that was synced (and is thus a
// descendant of main) can still hold markers — this check closes that hole.
func (s *Service) MergeMR(ctx context.Context, req *connect.Request[wsv1.MergeMRRequest]) (*connect.Response[wsv1.MergeMRResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mr, err := s.git.GetMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, mrError(err)
	}
	if !mr.Mergeable {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"change request #%d is not mergeable (the base has diverged); sync the head branch %q with a merge commit and resolve any conflicts", m.GetIndex(), mr.Head))
	}
	if conflicts, err := s.unresolvedConflicts(ctx, m.GetOrg(), m.GetRepo(), mr.Head); err != nil {
		return nil, err
	} else if len(conflicts) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"change request #%d still has unresolved conflict markers in: %s", m.GetIndex(), strings.Join(conflicts, ", ")))
	}
	if err := s.stagingGate(ctx, m.GetOrg(), m.GetRepo(), mr.Head); err != nil {
		return nil, err
	}
	// Pin the merge to the reviewed content: strip a trailing empty placeholder
	// from the head branch (the gateway force-pushes the FEATURE branch, which
	// is unprotected), then let Forgejo merge. Forgejo bypasses main's
	// protection for an MR merge; a direct gateway push to main would be refused.
	pin, err := s.mergeTipFor(ctx, m.GetOrg(), m.GetRepo(), mr.Head)
	if err != nil {
		return nil, err
	}
	if pin != "" && pin != mr.HeadSHA {
		opts, oerr := s.commitOpts(ctx, m.GetOrg(), m.GetRepo(), mr.Head)
		if oerr != nil {
			return nil, oerr
		}
		if rerr := s.commits.ResetTo(ctx, opts, pin); rerr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pin head: %w", rerr))
		}
	}
	if err := s.git.MergeMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex(), pin); err != nil {
		return nil, mrError(err)
	}
	return connect.NewResponse(&wsv1.MergeMRResponse{Ok: true}), nil
}

// mergeTipFor returns the sha a merge of `branch` should pin (the parent of a
// placeholder HEAD, else the tip). Empty when the branch cannot be inspected.
func (s *Service) mergeTipFor(ctx context.Context, org, repo, branch string) (string, error) {
	opts, err := s.commitOpts(ctx, org, repo, branch)
	if err != nil {
		return "", nil
	}
	st, err := s.commits.Status(ctx, opts)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if st.Placeholder {
		return st.MergeTip, nil
	}
	return st.Tip, nil
}

// SyncBranch (re)integrates `main` into a NON-MAIN branch with a two-parent
// merge commit, leaving marker blocks where the three-way merge cannot decide.
// It is the developer's way to catch up with main; the markers are resolved on
// the branch (and the CreateMR/MergeMR gates refuse a marker-carrying branch).
func (s *Service) SyncBranch(ctx context.Context, req *connect.Request[wsv1.SyncBranchRequest]) (*connect.Response[wsv1.SyncBranchResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	branch := m.GetBranch()
	if branch == "" || !roles.ValidComponent(branch) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("branch must be a simple name"))
	}
	if branch == roles.MainBranch {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("refusing to sync the default branch"))
	}
	if err := s.stagingGate(ctx, m.GetOrg(), m.GetRepo(), branch); err != nil {
		return nil, err
	}
	res, err := gitmerge.Sync(ctx, gitmerge.Options{
		RepoURL: s.git.GitURL(m.GetOrg(), m.GetRepo()),
		Branch:  branch,
		Base:    roles.MainBranch,
		User:    "root",
		Token:   s.git.Token(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("sync: %w", err))
	}
	return connect.NewResponse(&wsv1.SyncBranchResponse{
		Clean:     res.Clean,
		Conflicts: res.Conflicts,
		Commit:    res.Commit,
	}), nil
}

// unresolvedConflicts returns the changed files on `branch` (vs main) that
// still carry the conflict sentinel. Binary files are skipped (no markers).
func (s *Service) unresolvedConflicts(ctx context.Context, org, repo, branch string) ([]string, error) {
	paths, err := s.git.CompareFiles(ctx, org, repo, roles.MainBranch, branch)
	if err != nil {
		return nil, mrError(err)
	}
	seen := map[string]struct{}{}
	var carried []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		data, _, err := s.git.RawFile(ctx, org, repo, branch, p)
		if err != nil {
			// A path may be deleted on the branch; skip what we cannot read.
			continue
		}
		if gitmerge.HasMarkers(data) {
			carried = append(carried, p)
		}
	}
	return carried, nil
}

// mrError maps a Forgejo error to a connect error, surfacing conflicts as
// FailedPrecondition instead of an opaque Internal.
// BranchStatus reports a branch's staging state (placeholder present / carries
// staged changes / the sha an MR should merge).
func (s *Service) BranchStatus(ctx context.Context, req *connect.Request[wsv1.BranchStatusRequest]) (*connect.Response[wsv1.BranchStatusResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	opts, err := s.commitOpts(ctx, m.GetOrg(), m.GetRepo(), m.GetBranch())
	if err != nil {
		return nil, err
	}
	st, err := s.commits.Status(ctx, opts)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.BranchStatusResponse{
		Placeholder: st.Placeholder, Staged: st.Staged, Tip: st.Tip, MergeTip: st.MergeTip,
	}), nil
}

// CommitStaged rewinds the branch's staging placeholder to `message` and opens a
// fresh empty placeholder (the staging area).
func (s *Service) CommitStaged(ctx context.Context, req *connect.Request[wsv1.CommitStagedRequest]) (*connect.Response[wsv1.CommitStagedResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.GetMessage()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message is required"))
	}
	opts, err := s.commitOpts(ctx, m.GetOrg(), m.GetRepo(), m.GetBranch())
	if err != nil {
		return nil, err
	}
	sha, err := s.commits.Commit(ctx, opts, m.GetMessage())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&wsv1.CommitStagedResponse{Sha: sha}), nil
}

// ApplyFiles applies file operations to a branch's staging commit (amending the
// placeholder when open). It is the write path for repo-file-write/edit/delete/port.
func (s *Service) ApplyFiles(ctx context.Context, req *connect.Request[wsv1.ApplyFilesRequest]) (*connect.Response[wsv1.ApplyFilesResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if len(m.GetFiles()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("files is required"))
	}
	opts, err := s.commitOpts(ctx, m.GetOrg(), m.GetRepo(), m.GetBranch())
	if err != nil {
		return nil, err
	}
	ops := make([]gitcommit.FileOp, 0, len(m.GetFiles()))
	for _, f := range m.GetFiles() {
		content := f.GetContentBytes()
		if len(content) == 0 {
			content = []byte(f.GetContent())
		}
		ops = append(ops, gitcommit.FileOp{Path: f.GetPath(), Op: f.GetOperation(), Content: content})
	}
	sha, err := s.commits.ApplyFiles(ctx, opts, ops)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.ApplyFilesResponse{Sha: sha}), nil
}

// commitOpts builds the gitcommit options for one branch of a visible repo.
func (s *Service) commitOpts(ctx context.Context, org, repo, branch string) (gitcommit.Options, error) {
	if branch == "" || !roles.ValidComponent(branch) {
		return gitcommit.Options{}, connect.NewError(connect.CodeInvalidArgument, errors.New("branch must be a simple name"))
	}
	if branch == roles.MainBranch {
		return gitcommit.Options{}, connect.NewError(connect.CodeInvalidArgument, errors.New("refusing to rewrite the default branch"))
	}
	return gitcommit.Options{
		RepoURL: s.git.GitURL(org, repo),
		Branch:  branch,
		User:    "root",
		Token:   s.git.Token(),
	}, nil
}

// stagingGate refuses an action when the branch still has staged changes (a
// placeholder carrying a diff). A clean branch — including a fresh branch or an
// empty placeholder — passes.
func (s *Service) stagingGate(ctx context.Context, org, repo, branch string) error {
	opts, err := s.commitOpts(ctx, org, repo, branch)
	if err != nil {
		return err
	}
	st, err := s.commits.Status(ctx, opts)
	if err != nil {
		return nil // best-effort: never block on an inspection failure
	}
	if st.Placeholder && st.Staged {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(
			"branch has staged changes; run repo-commit with a message to finalize them before this action"))
	}
	return nil
}

func mrError(err error) error {
	var conflict *forgejo.ErrConflict
	if errors.As(err, &conflict) {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("merge conflict: %s", conflict.Reason))
	}
	var notFound *forgejo.ErrNotFound
	if errors.As(err, &notFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// ---- sandboxes ----
//
// The gateway owns the Kubernetes lifecycle IN-PROCESS (the worker-manager
// service was folded in): it creates Pod + Service + Secret in the managed
// namespace. Visibility is creator-scoped; the token is only ever returned by
// ResolveSandbox to the owning tenant.

func (s *Service) ListSandboxes(ctx context.Context, req *connect.Request[wsv1.ListSandboxesRequest]) (*connect.Response[wsv1.ListSandboxesResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	sbxs, err := s.sbx.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.SandboxInfo, 0, len(sbxs))
	want := req.Msg.GetSession()
	for _, sb := range sbxs {
		// Visibility: only sandboxes this tenant created.
		if sb.Creator != "" && sb.Creator != tenant {
			continue
		}
		// Optional session filter: only this session's sandboxes.
		if want != "" && sb.Session != want {
			continue
		}
		out = append(out, toSandboxInfo(sb))
	}
	return connect.NewResponse(&wsv1.ListSandboxesResponse{Sandboxes: out}), nil
}

func (s *Service) CreateSandbox(ctx context.Context, req *connect.Request[wsv1.CreateSandboxRequest]) (*connect.Response[wsv1.CreateSandboxResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if !roles.ValidComponent(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be simple"))
	}
	// Sandboxes run a PRE-BUILT, worker-bundled image from the deployment's
	// dedicated sandbox org. The gateway no longer injects the worker at launch,
	// so an image outside that org would have no worker and could never become
	// ready. Empty = the configured default sandbox image.
	image := req.Msg.GetImage()
	if image == "" {
		image = s.defaultSandboxImage
	}
	if image == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no sandbox image given and no default configured"))
	}
	if err := s.validateSandboxImage(image); err != nil {
		return nil, err
	}
	sb, _, err := s.sbx.Create(ctx, sandboxmgr.Spec{
		Name: name, Image: image, CPU: req.Msg.GetCpu(), Memory: req.Msg.GetMemory(),
		Env: req.Msg.GetEnv(), Creator: tenant, Session: req.Msg.GetSession(),
		Runtime: s.renderRuntime(req.Msg.GetKvm(), req.Msg.GetGpuCount()),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Wait up to 60s for the worker to accept connections; on timeout the
	// sandbox is left in place (the caller may status/delete it).
	if err := s.sbx.WaitReady(ctx, name, 60*time.Second); err != nil {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	if cur, ok, gerr := s.sbx.Get(ctx, name); gerr == nil && ok {
		sb = cur
	}
	return connect.NewResponse(&wsv1.CreateSandboxResponse{Sandbox: toSandboxInfo(sb)}), nil
}

// sandboxPick is one session's representative sandbox.
type sandboxPick struct {
	name    string
	phase   string
	ready   bool
	created int64
}

// pickRepresentative reports whether candidate `sb` should replace `prev`
// (prefer Running/Ready, then newest; any first entry wins).
func pickRepresentative(prev sandboxPick, sb sandboxmgr.Sandbox) bool {
	if prev.name == "" {
		return true
	}
	candUp := sb.Phase == "Running" || sb.Ready
	prevUp := prev.phase == "Running" || prev.ready
	if candUp != prevUp {
		return candUp
	}
	return sb.CreatedAt > prev.created
}

// sessionSandboxes returns every sandbox owned by `session`, REPRESENTATIVE
// first (prefer Running/Ready, else newest), then the rest newest-first. Empty
// when none. Used to fill BranchSession.sandboxes; the head is the
// representative (BranchSession.sandbox / .phase).
func sessionSandboxes(sbxs []sandboxmgr.Sandbox, session string) []*wsv1.SandboxRef {
	mine := make([]sandboxmgr.Sandbox, 0, 2)
	for _, sb := range sbxs {
		if sb.Session == session {
			mine = append(mine, sb)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	// Newest-first, then move the representative to the head — the rest keep
	// their newest-first order.
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].CreatedAt > mine[j].CreatedAt })
	rep := 0
	for i := 1; i < len(mine); i++ {
		if pickRepresentative(
			sandboxPick{name: mine[rep].Name, phase: mine[rep].Phase, ready: mine[rep].Ready, created: mine[rep].CreatedAt},
			mine[i],
		) {
			rep = i
		}
	}
	ordered := make([]sandboxmgr.Sandbox, 0, len(mine))
	ordered = append(ordered, mine[rep])
	ordered = append(ordered, mine[:rep]...)
	ordered = append(ordered, mine[rep+1:]...)
	out := make([]*wsv1.SandboxRef, 0, len(ordered))
	for _, sb := range ordered {
		out = append(out, &wsv1.SandboxRef{Name: sb.Name, Phase: sb.Phase, Ready: sb.Ready})
	}
	return out
}

// sandboxFields loads the session's sandboxes and returns the representative
// (name, phase) plus the full ordered list, for filling a BranchSession.
func (s *Service) sandboxFields(ctx context.Context, session string) (string, string, []*wsv1.SandboxRef) {
	sbxs, err := s.sbx.List(ctx)
	if err != nil {
		return "", "", nil
	}
	refs := sessionSandboxes(sbxs, session)
	if len(refs) == 0 {
		return "", "", nil
	}
	return refs[0].GetName(), refs[0].GetPhase(), refs
}

// validateSandboxImage enforces that a sandbox image comes from the
// deployment's dedicated sandbox org. The image may be a full ref
// (`<registry>/<org>/<name>:<tag>`) or a bare name; in both cases the FIRST
// path segment after an optional registry host must equal the sandbox org. The
// registry host is matched loosely (with or without scheme) because callers
// may echo back either form.
func (s *Service) validateSandboxImage(image string) error {
	if s.sandboxOrg == "" {
		// Not configured: accept as-is (the deployment opted out of the guard).
		return nil
	}
	path := image
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
	}
	// Strip the registry host (first segment containing '.' or ':' or equal to
	// localhost), then the tag/digest.
	seg := strings.SplitN(path, "/", 2)
	rest := path
	if len(seg) == 2 && (strings.ContainsAny(seg[0], ".:") || seg[0] == "localhost") {
		rest = seg[1]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[:i]
	}
	// Drop a tag only when it is in the LAST path segment (not an org:port).
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		rest = rest[:i]
	}
	org := strings.SplitN(rest, "/", 2)[0]
	if org != s.sandboxOrg {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("sandbox image must come from the %q org", s.sandboxOrg))
	}
	return nil
}

func (s *Service) GetSandbox(ctx context.Context, req *connect.Request[wsv1.GetSandboxRequest]) (*connect.Response[wsv1.GetSandboxResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	sb, ok, err := s.sbx.Get(ctx, req.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	if sb.Creator != "" && sb.Creator != tenant {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	info := toSandboxInfo(sb)
	// Best-effort worker environment (workspace root + home) so the webui can
	// anchor relative paths and show `~`. Never fails the request.
	if url, token, rerr := s.sbx.Resolve(ctx, sb.Name); rerr == nil {
		if wi, ierr := workerclient.New(url, token).Info(ctx); ierr == nil {
			info.Workspace, info.Home, info.Os, info.Arch = wi.Workspace, wi.Home, wi.OS, wi.Arch
		}
	}
	return connect.NewResponse(&wsv1.GetSandboxResponse{Sandbox: info}), nil
}

func (s *Service) DeleteSandbox(ctx context.Context, req *connect.Request[wsv1.DeleteSandboxRequest]) (*connect.Response[wsv1.DeleteSandboxResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	sb, ok, err := s.sbx.Get(ctx, req.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	if sb.Creator != "" && sb.Creator != tenant {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	deleted, err := s.sbx.Delete(ctx, req.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeleteSandboxResponse{Ok: deleted}), nil
}

// ResolveSandbox returns the worker's url + bearer token to the OWNING tenant,
// so an execution tool can talk to the worker directly.
func (s *Service) ResolveSandbox(ctx context.Context, req *connect.Request[wsv1.ResolveSandboxRequest]) (*connect.Response[wsv1.ResolveSandboxResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	sb, ok, err := s.sbx.Get(ctx, req.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	if sb.Creator != "" && sb.Creator != tenant {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	url, token, err := s.sbx.Resolve(ctx, req.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&wsv1.ResolveSandboxResponse{Name: req.Msg.GetName(), Url: url, Token: token}), nil
}

// ---- sandbox jobs (read-only observability) ----

// ownedSandbox resolves the caller + the named sandbox, enforcing tenant
// ownership. Returns the worker endpoint (url, token).
func (s *Service) ownedSandbox(ctx context.Context, hdr map[string][]string, name string) (string, string, error) {
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		return "", "", err
	}
	sb, ok, err := s.sbx.Get(ctx, name)
	if err != nil {
		return "", "", connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return "", "", connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	if sb.Creator != "" && sb.Creator != tenant {
		return "", "", connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	url, token, err := s.sbx.Resolve(ctx, name)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err)
	}
	return url, token, nil
}

// ListSandboxJobs returns a sandbox's worker job history.
func (s *Service) ListSandboxJobs(ctx context.Context, req *connect.Request[wsv1.ListSandboxJobsRequest]) (*connect.Response[wsv1.ListSandboxJobsResponse], error) {
	url, token, err := s.ownedSandbox(ctx, req.Header(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	jobs, err := workerclient.New(url, token).ListJobs(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.SandboxJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, &wsv1.SandboxJob{
			Id: j.ID, Command: j.Command, State: j.State, ExitCode: j.ExitCode,
			StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		})
	}
	return connect.NewResponse(&wsv1.ListSandboxJobsResponse{Jobs: out}), nil
}

// GetSandboxJobOutput polls a bounded window of a job's buffered output.
func (s *Service) GetSandboxJobOutput(ctx context.Context, req *connect.Request[wsv1.GetSandboxJobOutputRequest]) (*connect.Response[wsv1.GetSandboxJobOutputResponse], error) {
	url, token, err := s.ownedSandbox(ctx, req.Header(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	out, err := workerclient.New(url, token).JobOutput(ctx, m.GetJobId(), m.GetStart(), m.GetEnd(), m.GetStream())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.GetSandboxJobOutputResponse{
		Lines: out.Lines, TotalLines: out.TotalLines,
		StartLine: out.StartLine, EndLine: out.EndLine, Done: out.Done,
	}), nil
}

// WatchSandboxJob streams a job's output (history then live) until it ends.
func (s *Service) WatchSandboxJob(ctx context.Context, req *connect.Request[wsv1.WatchSandboxJobRequest], st *connect.ServerStream[wsv1.WatchSandboxJobResponse]) error {
	url, token, err := s.ownedSandbox(ctx, req.Header(), req.Msg.GetName())
	if err != nil {
		return err
	}
	events, errc := workerclient.New(url, token).WatchJob(ctx, req.Msg.GetJobId())
	for ev := range events {
		msg := &wsv1.WatchSandboxJobResponse{Output: ev.Output}
		if ev.Done {
			msg.Done = true
			msg.ExitCode = ev.ExitCode
			msg.Stdout = ev.Stdout
			msg.Stderr = ev.Stderr
		}
		if err := st.Send(msg); err != nil {
			return err
		}
	}
	if err := <-errc; err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	return nil
}

// ListSandboxFiles lists a directory (or a single file) in the sandbox. The
// path is passed to the worker VERBATIM: a relative path resolves against the
// worker's workspace, an absolute path is used as-is.
func (s *Service) ListSandboxFiles(ctx context.Context, req *connect.Request[wsv1.ListSandboxFilesRequest]) (*connect.Response[wsv1.ListSandboxFilesResponse], error) {
	url, token, err := s.ownedSandbox(ctx, req.Header(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	fl, err := workerclient.New(url, token).FileList(ctx, m.GetPath(), m.GetDepth(), m.GetLimit())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.SandboxFileEntry, 0, len(fl.Files))
	for _, f := range fl.Files {
		out = append(out, &wsv1.SandboxFileEntry{Path: f.Path, Size: f.Size, IsDir: f.IsDir})
	}
	return connect.NewResponse(&wsv1.ListSandboxFilesResponse{IsDir: fl.IsDir, Files: out}), nil
}

// ReadSandboxFile reads a (windowed) file from the sandbox.
func (s *Service) ReadSandboxFile(ctx context.Context, req *connect.Request[wsv1.ReadSandboxFileRequest]) (*connect.Response[wsv1.ReadSandboxFileResponse], error) {
	url, token, err := s.ownedSandbox(ctx, req.Header(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	fr, err := workerclient.New(url, token).FileRead(ctx, m.GetPath(), m.GetStartLine(), m.GetEndLine())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.ReadSandboxFileResponse{
		Content: fr.Content, TotalLines: fr.TotalLines,
		StartLine: fr.StartLine, EndLine: fr.EndLine,
	}), nil
}

// ---- services (long-lived Deployments) ----

// DeployService creates/updates a long-lived Deployment + Service from a user
// image (no worker injection, no sidecar). A bounded YAML manifest, when
// given, overrides the scalar fields.
func (s *Service) DeployService(ctx context.Context, req *connect.Request[wsv1.DeployServiceRequest]) (*connect.Response[wsv1.DeployServiceResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("service backend not configured"))
	}
	m := req.Msg
	name := m.GetName()
	if name == "" {
		name = servicesmgr.ServiceName(sessionFromHeaders(req.Header()))
	}
	// A service is published publicly as `<name>.<ns>.<domain>`, so its name
	// must be a DNS-1123 label (lowercase letters/digits/'-').
	if !roles.ValidServiceName(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be a DNS-1123 label (lowercase letters, digits, '-')"))
	}
	// Ownership guard: only the creator may update an existing service.
	if existing, gerr := s.services.Get(ctx, name); gerr == nil {
		if existing.Creator != "" && existing.Creator != tenant {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("service belongs to another tenant"))
		}
	}

	// Resolve the exposed ports. Empty `services` = the default single public
	// port (tcp80 -> container_port). Entries sharing a suffix form ONE Service.
	ports, err := servicePorts(m.GetServices(), m.GetContainerPort())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	vols, err := s.resolveVolumes(ctx, tenant, m.GetVolumes())
	if err != nil {
		return nil, err
	}
	spec := servicesmgr.Spec{
		Name: name, Image: m.GetImage(), Command: m.GetCommand(),
		Env: m.GetEnv(), CPU: m.GetCpu(), Memory: m.GetMemory(),
		Replicas: m.GetReplicas(), ContainerPort: m.GetContainerPort(),
		ServicePort: m.GetServicePort(), Creator: tenant,
		Session: sessionFromHeaders(req.Header()),
		Ports:   ports,
		Runtime: s.renderRuntime(m.GetKvm(), m.GetGpuCount()),
		Volumes: vols,
	}
	if spec.Image == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image is required"))
	}
	svc, err := s.services.Deploy(ctx, spec)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeployServiceResponse{Service: toServiceInfo(svc, s.servicePublicURLs(svc, req.Header()))}), nil
}

// servicePorts resolves the requested ports into servicesmgr.Port values. With
// no explicit ports it returns the default (tcp80 -> containerPort). Each
// distinct suffix becomes one k8s Service; the primary (empty suffix) must not
// repeat the public tcp80 (only one public endpoint per Service name).
func servicePorts(specs []*wsv1.ServicePortSpec, containerPort int32) ([]servicesmgr.Port, error) {
	if len(specs) == 0 {
		target := containerPort
		if target == 0 {
			target = 8080
		}
		return []servicesmgr.Port{{Port: 80, Protocol: "tcp", TargetPort: target}}, nil
	}
	out := make([]servicesmgr.Port, 0, len(specs))
	// tcp80 is the public ingress; only one per suffix (port collision).
	publicPerSuffix := map[string]int{}
	for i, sp := range specs {
		port, proto, ok := servicesmgr.PresetPorts(sp.GetPreset())
		if !ok {
			return nil, fmt.Errorf("services[%d]: preset must be tcp80|tcp443|udp443", i)
		}
		suffix := sp.GetName()
		if suffix != "" && !roles.ValidServiceName(suffix) {
			return nil, fmt.Errorf("services[%d]: name must be a DNS-1123 label", i)
		}
		if port == 80 {
			publicPerSuffix[suffix]++
			if publicPerSuffix[suffix] > 1 {
				return nil, fmt.Errorf("services[%d]: only one tcp80 per service name", i)
			}
		}
		target := sp.GetTargetPort()
		if target == 0 {
			target = containerPort
		}
		if target == 0 {
			target = 8080
		}
		out = append(out, servicesmgr.Port{Suffix: suffix, Port: port, Protocol: proto, TargetPort: target})
	}
	return out, nil
}

// resolveVolumes validates each requested PVC mount: the claim must exist and
// belong to the caller's tenant. Returns the servicesmgr volume specs.
func (s *Service) resolveVolumes(ctx context.Context, tenant string, mounts []*wsv1.VolumeMountSpec) ([]servicesmgr.VolumeMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]servicesmgr.VolumeMount, 0, len(mounts))
	for i, m := range mounts {
		if !roles.ValidServiceName(m.GetPvc()) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("volumes[%d]: pvc must be a DNS-1123 label", i))
		}
		if m.GetMountPath() == "" || !strings.HasPrefix(m.GetMountPath(), "/") {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("volumes[%d]: mount_path must be an absolute path", i))
		}
		pvc, err := s.services.GetPVC(ctx, m.GetPvc())
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("volumes[%d]: pvc %q not found", i, m.GetPvc()))
		}
		if pvc.Creator != "" && pvc.Creator != tenant {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("volumes[%d]: pvc %q not found", i, m.GetPvc()))
		}
		out = append(out, servicesmgr.VolumeMount{
			PVC: pvc.Name, MountPath: m.GetMountPath(),
			ReadOnly: m.GetReadOnly(), SubPath: m.GetSubPath(),
		})
	}
	return out, nil
}

// ---- persistent volume claims ----

// CreatePVC creates a named, tenant-owned claim with the deployment's storage
// class (self-hosted local-path). Admin action; the tenant owns the claim.
func (s *Service) CreatePVC(ctx context.Context, req *connect.Request[wsv1.CreatePVCRequest]) (*connect.Response[wsv1.CreatePVCResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("service backend not configured"))
	}
	m := req.Msg
	if !roles.ValidServiceName(m.GetName()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be a DNS-1123 label (lowercase letters, digits, '-')"))
	}
	// Only the deployment's configured class is supported today.
	if sc := m.GetStorageClass(); sc != "" && sc != s.pvcStorageClass {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("storage_class must be %q", s.pvcStorageClass))
	}
	size := m.GetSize()
	if size == "" {
		size = s.pvcDefaultSize
	}
	pvc, err := s.services.CreatePVC(ctx, m.GetName(), size, s.pvcStorageClass, tenant)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.CreatePVCResponse{Pvc: toPVCInfo(pvc)}), nil
}

// ListPVCs lists the tenant's claims.
func (s *Service) ListPVCs(ctx context.Context, req *connect.Request[wsv1.ListPVCsRequest]) (*connect.Response[wsv1.ListPVCsResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return connect.NewResponse(&wsv1.ListPVCsResponse{}), nil
	}
	pvcs, err := s.services.ListPVCs(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.PVCInfo, 0, len(pvcs))
	for _, p := range pvcs {
		if p.Creator != "" && p.Creator != tenant {
			continue
		}
		out = append(out, toPVCInfo(p))
	}
	return connect.NewResponse(&wsv1.ListPVCsResponse{Pvcs: out}), nil
}

// DeletePVC removes a claim the tenant owns. Refused while a service mounts it.
func (s *Service) DeletePVC(ctx context.Context, req *connect.Request[wsv1.DeletePVCRequest]) (*connect.Response[wsv1.DeletePVCResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return connect.NewResponse(&wsv1.DeletePVCResponse{Ok: false}), nil
	}
	cur, gerr := s.services.GetPVC(ctx, req.Msg.GetName())
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("pvc not found"))
	}
	if cur.Creator != "" && cur.Creator != tenant {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("pvc not found"))
	}
	ok, err := s.services.DeletePVC(ctx, req.Msg.GetName())
	if err != nil {
		// Mounted-by is the common refusal: surface it as FailedPrecondition.
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&wsv1.DeletePVCResponse{Ok: ok}), nil
}

func toPVCInfo(p servicesmgr.PVC) *wsv1.PVCInfo {
	return &wsv1.PVCInfo{
		Name: p.Name, Size: p.Size, StorageClass: p.StorageClass, Phase: p.Phase,
		Creator: p.Creator, CreatedAt: p.CreatedAt, MountedBy: p.MountedBy,
	}
}

// ListServices lists the tenant's services.
func (s *Service) ListServices(ctx context.Context, req *connect.Request[wsv1.ListServicesRequest]) (*connect.Response[wsv1.ListServicesResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return connect.NewResponse(&wsv1.ListServicesResponse{}), nil
	}
	svcs, err := s.services.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.ServiceInfo, 0, len(svcs))
	for _, svc := range svcs {
		if svc.Creator != "" && svc.Creator != tenant {
			continue
		}
		out = append(out, toServiceInfo(svc, s.servicePublicURLs(svc, req.Header())))
	}
	return connect.NewResponse(&wsv1.ListServicesResponse{Services: out}), nil
}

// DeleteService removes a service the tenant owns.
func (s *Service) DeleteService(ctx context.Context, req *connect.Request[wsv1.DeleteServiceRequest]) (*connect.Response[wsv1.DeleteServiceResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return connect.NewResponse(&wsv1.DeleteServiceResponse{Ok: false}), nil
	}
	svc, gerr := s.services.Get(ctx, req.Msg.GetName())
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("service not found"))
	}
	if svc.Creator != "" && svc.Creator != tenant {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("service not found"))
	}
	ok, err := s.services.Delete(ctx, req.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeleteServiceResponse{Ok: ok}), nil
}

// PauseService scales a service to zero replicas without deleting it. The
// replica count is remembered so ResumeService can restore it.
func (s *Service) PauseService(ctx context.Context, req *connect.Request[wsv1.PauseServiceRequest]) (*connect.Response[wsv1.PauseServiceResponse], error) {
	svc, err := s.pauseResume(ctx, req.Header(), req.Msg.GetName(), true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wsv1.PauseServiceResponse{Service: svc}), nil
}

// ResumeService restores a paused service to its pre-pause replica count.
func (s *Service) ResumeService(ctx context.Context, req *connect.Request[wsv1.ResumeServiceRequest]) (*connect.Response[wsv1.ResumeServiceResponse], error) {
	svc, err := s.pauseResume(ctx, req.Header(), req.Msg.GetName(), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wsv1.ResumeServiceResponse{Service: svc}), nil
}

// pauseResume is the shared authorization + dispatch for Pause/Resume. Pausing
// is only meaningful for a running service; resuming only for a paused one.
func (s *Service) pauseResume(ctx context.Context, hdr map[string][]string, name string, pause bool) (*wsv1.ServiceInfo, error) {
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("service backend not configured"))
	}
	cur, gerr := s.services.Get(ctx, name)
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("service not found"))
	}
	if cur.Creator != "" && cur.Creator != tenant {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("service not found"))
	}
	var out servicesmgr.Service
	if pause {
		out, err = s.services.Pause(ctx, name)
	} else {
		out, err = s.services.Resume(ctx, name)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return toServiceInfo(out, s.servicePublicURLs(out, hdr)), nil
}

// PreviewService deploys a session-bound, cluster-only PREVIEW service for
// developer verification. The name is prefixed with the session slug, no public
// URL is produced, and the service carries a TTL for reclamation.
func (s *Service) PreviewService(ctx context.Context, req *connect.Request[wsv1.PreviewServiceRequest]) (*connect.Response[wsv1.PreviewServiceResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("service backend not configured"))
	}
	m := req.Msg
	if m.GetImage() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image is required"))
	}
	session := sessionFromHeaders(req.Header())
	base := m.GetName()
	if base == "" {
		base = "app"
	}
	if !roles.ValidServiceName(base) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be a DNS-1123 label"))
	}
	// Prefix with the session slug so a preview never collides with a release
	// service, and stays under 63 chars (DNS-1123 label).
	name := previewName(session, base)
	// Ports: previews are CLUSTER-ONLY. Even a tcp80 entry maps to a Service
	// with no public URL (the gateway never publishes it).
	ports, err := servicePorts(m.GetServices(), m.GetContainerPort())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttl := time.Duration(m.GetTtlSeconds()) * time.Second
	if ttl <= 0 {
		ttl = s.previewTTL
	}
	expires := int64(0)
	if ttl > 0 {
		expires = time.Now().Add(ttl).UnixMilli()
	}
	vols, err := s.resolveVolumes(ctx, tenant, m.GetVolumes())
	if err != nil {
		return nil, err
	}
	svc, err := s.services.Deploy(ctx, servicesmgr.Spec{
		Name: name, Image: m.GetImage(), Command: m.GetCommand(),
		Env: m.GetEnv(), CPU: m.GetCpu(), Memory: m.GetMemory(),
		Replicas: 1, ContainerPort: m.GetContainerPort(),
		Creator: tenant, Session: session, Ports: ports,
		Stage:     servicesmgr.StagePreview,
		ExpiresAt: expires,
		Runtime:   s.renderRuntime(m.GetKvm(), m.GetGpuCount()),
		Volumes:   vols,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// No public URLs: a preview is reachable only in-cluster.
	return connect.NewResponse(&wsv1.PreviewServiceResponse{Service: toServiceInfo(svc, nil)}), nil
}

// previewName prefixes a base name with the session slug, keeping it a legal
// DNS-1123 label. A free session (no branch) still gets a stable slug.
func previewName(session, base string) string {
	slug := servicesmgr.ServiceName(session)
	// Leave room for "-" + base, capped at 63 chars total.
	maxSlug := 63 - 1 - len(base)
	if maxSlug < 1 {
		maxSlug = 1
	}
	if len(slug) > maxSlug {
		slug = slug[:maxSlug]
	}
	slug = strings.Trim(slug, "-")
	name := slug + "-" + base
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// ServiceLogs reads a bounded window of a service's container log.
func (s *Service) ServiceLogs(ctx context.Context, req *connect.Request[wsv1.ServiceLogsRequest]) (*connect.Response[wsv1.ServiceLogsResponse], error) {
	if _, err := s.ownedService(ctx, req.Header(), req.Msg.GetName()); err != nil {
		return nil, err
	}
	tail := req.Msg.GetTailLines()
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	lines, err := s.services.LogSource().Tail(ctx, req.Msg.GetName(), servicesmgr.LogOptions{
		TailLines: tail, Previous: req.Msg.GetPrevious(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.ServiceLogsResponse{Lines: lines}), nil
}

// WatchServiceLogs streams a service's container log until the stream ends or
// the client disconnects.
func (s *Service) WatchServiceLogs(ctx context.Context, req *connect.Request[wsv1.WatchServiceLogsRequest], st *connect.ServerStream[wsv1.WatchServiceLogsResponse]) error {
	if _, err := s.ownedService(ctx, req.Header(), req.Msg.GetName()); err != nil {
		return err
	}
	lines, errc := s.services.LogSource().Follow(ctx, req.Msg.GetName(), servicesmgr.LogOptions{
		Previous: req.Msg.GetPrevious(),
	})
	for l := range lines {
		if err := st.Send(&wsv1.WatchServiceLogsResponse{Output: l + "\n"}); err != nil {
			return err
		}
	}
	if err := <-errc; err != nil {
		return st.Send(&wsv1.WatchServiceLogsResponse{Done: true, Error: err.Error()})
	}
	return st.Send(&wsv1.WatchServiceLogsResponse{Done: true})
}

// ownedService resolves a service and enforces tenant/creator ownership.
func (s *Service) ownedService(ctx context.Context, hdr map[string][]string, name string) (servicesmgr.Service, error) {
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		return servicesmgr.Service{}, err
	}
	if s.services == nil {
		return servicesmgr.Service{}, connect.NewError(connect.CodeUnavailable, errors.New("service backend not configured"))
	}
	svc, gerr := s.services.Get(ctx, name)
	if gerr != nil {
		return servicesmgr.Service{}, connect.NewError(connect.CodeNotFound, errors.New("service not found"))
	}
	if svc.Creator != "" && svc.Creator != tenant {
		return servicesmgr.Service{}, connect.NewError(connect.CodePermissionDenied, errors.New("service belongs to another tenant"))
	}
	return svc, nil
}

// BuildPreviewImage builds a preview image: the image NAME is forced to the
// repo, and the TAG is forced to `preview-<branch>-<sha>` (+ optional suffix),
// so a preview build can never overwrite a release tag. Admin/maintainer/
// developer may build; the destination is always under the source repo's org.
func (s *Service) BuildPreviewImage(ctx context.Context, req *connect.Request[wsv1.BuildPreviewImageRequest]) (*connect.Response[wsv1.BuildPreviewImageResponse], error) {
	if _, err := s.sandboxAuth(ctx, req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	org, repo, ref := m.GetOrg(), m.GetRepo(), m.GetRef()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo must be simple names"))
	}
	if ref == "" {
		ref = roles.MainBranch
	}
	if !roles.ValidComponent(ref) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref must be a simple name"))
	}
	if s.builder == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("image builder not configured"))
	}
	// The image NAME is the repo (not caller-chosen): a preview never writes to
	// another image path.
	if !imageNameRe.MatchString(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("repo must be a single simple name to derive the image"))
	}
	// The tag is FORCED to a preview prefix; a short ref sha keeps builds
	// distinct. The caller may append a suffix.
	sha, err := s.git.BranchTip(ctx, org, repo, ref)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	tag := "preview-" + sanitizeTag(ref) + "-" + short
	if sfx := m.GetTagSuffix(); sfx != "" {
		if !roles.ValidComponent(sfx) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tag_suffix must be simple"))
		}
		tag += "-" + sanitizeTag(sfx)
	}

	archive, err := s.git.ArchiveTarGz(ctx, org, repo, ref)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fetch repo archive: %w", err))
	}
	dir, cleanup, err := imagebuild.Extract(archive, 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer cleanup()
	res, err := s.builder.Build(ctx, imagebuild.Request{
		ContextDir: dir,
		Dockerfile: m.GetDockerfile(),
		Context:    m.GetContext(),
		Repo:       org + "/" + repo,
		Tag:        tag,
		BuildArgs:  m.GetBuildArgs(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.BuildPreviewImageResponse{ImageRef: res.ImageRef, Log: res.Log, Tag: tag}), nil
}

// sanitizeTag lowercases and replaces tag-illegal chars with '-'.
func sanitizeTag(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "x"
	}
	return out
}

// Blame returns per-line authorship of one file at a ref (go-git over a bare
// clone; Forgejo has no blame API).
func (s *Service) Blame(ctx context.Context, req *connect.Request[wsv1.BlameRequest]) (*connect.Response[wsv1.BlameResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if m.GetPath() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("path is required"))
	}
	ref := m.GetRef()
	if ref == "" {
		ref = roles.MainBranch
	}
	lines, err := gitcommit.Blame(ctx, s.git.GitURL(m.GetOrg(), m.GetRepo()), "root", s.git.Token(), ref, m.GetPath(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*wsv1.BlameLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, &wsv1.BlameLine{
			Line: int32(l.Line), Sha: l.SHA, Author: l.Author,
			AuthorEmail: l.AuthorEmail, Date: l.Date, Content: l.Content,
		})
	}
	return connect.NewResponse(&wsv1.BlameResponse{Lines: out}), nil
}

// FileDiff returns the unified diff of one file between two refs (go-git;
// Forgejo's compare `patch` is empty on 1.22).
func (s *Service) FileDiff(ctx context.Context, req *connect.Request[wsv1.FileDiffRequest]) (*connect.Response[wsv1.FileDiffResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if m.GetPath() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("path is required"))
	}
	diff, err := gitcommit.FileDiff(ctx, s.git.GitURL(m.GetOrg(), m.GetRepo()), "root", s.git.Token(), m.GetBase(), m.GetHead(), m.GetPath(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.FileDiffResponse{Diff: diff}), nil
}

// sessionFromHeaders extracts the caller's session name (used to derive a
// default service name). The extension passes it via X-Session-Name.
func sessionFromHeaders(hdr map[string][]string) string {
	for k, vs := range hdr {
		if strings.EqualFold(k, "X-Session-Name") && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

func toServiceInfo(svc servicesmgr.Service, publicURLs map[string]string) *wsv1.ServiceInfo {
	ports := make([]*wsv1.ServicePortInfo, 0, len(svc.Ports))
	primary := ""
	for _, p := range svc.Ports {
		key := presetKey(p.Port, p.Protocol)
		// Only a tcp80 ingress has a public URL.
		publicURL := ""
		if key == "tcp80" {
			publicURL = publicURLs[p.Suffix]
		}
		info := &wsv1.ServicePortInfo{
			Name: p.Suffix, Preset: key, Port: p.Port, Protocol: p.Protocol,
			TargetPort: p.TargetPort, PublicUrl: publicURL,
		}
		ports = append(ports, info)
		if p.Suffix == "" && key == "tcp80" {
			primary = publicURL
		}
	}
	return &wsv1.ServiceInfo{
		Name: svc.Name, Image: svc.Image, Phase: svc.Phase, Ready: svc.Ready,
		Replicas: svc.Replicas, Url: svc.URL, Creator: svc.Creator, Session: svc.Session,
		PublicUrl: primary, Ports: ports,
		Stage: svc.Stage, PodPhase: svc.PodPhase, Restarts: svc.Restarts,
		Message: svc.Message, ExpiresAt: svc.ExpiresAt, Paused: svc.Paused,
	}
}

// presetKey maps a (port, protocol) back to its preset name.
func presetKey(port int32, proto string) string {
	switch {
	case port == 80 && proto == "tcp":
		return "tcp80"
	case port == 443 && proto == "tcp":
		return "tcp443"
	case port == 443 && proto == "udp":
		return "udp443"
	}
	return fmt.Sprintf("%s%d", proto, port)
}

// servicePublicURLs returns, per port-suffix, the anonymous public URL for
// tcp80 ingress ports (`https://<name>[-<suffix>].<ns>.<domain>`). Only tcp80 is
// publicly reachable (the edge maps hosts to a service's port 80).
func (s *Service) servicePublicURLs(svc servicesmgr.Service, hdr map[string][]string) map[string]string {
	domain := s.publicDomainFor(hdr)
	if domain == "" {
		return nil
	}
	ns := s.sandboxNS
	if ns == "" {
		ns = "worker"
	}
	out := map[string]string{}
	for _, p := range svc.Ports {
		if p.Port == 80 && p.Protocol == "tcp" {
			out[p.Suffix] = "https://" + siblingName(svc.Name, p.Suffix) + "." + ns + "." + domain
		}
	}
	return out
}

// siblingName mirrors servicesmgr's service naming (primary vs `<name>-<suffix>`).
func siblingName(name, suffix string) string {
	if suffix == "" {
		return name
	}
	return name + "-" + suffix
}

// publicDomainFor returns the domain a service is published under, so its
// public URL is `https://<name>.<ns>.<domain>`. An explicit configured domain
// wins; otherwise it is INFERRED from the request's forwarded host by dropping
// the first two labels (the edge convention is `<svc>.<ns>.<domain>`), e.g.
// `workspace.agent.10.199.64.20.nip.io` -> `10.199.64.20.nip.io`. Empty when it
// cannot be determined (e.g. an in-cluster caller with no public host).
func (s *Service) publicDomainFor(hdr map[string][]string) string {
	if s.publicServiceDomain != "" {
		return s.publicServiceDomain
	}
	host := forwardedHost(hdr)
	if host == "" {
		return ""
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	// An in-cluster caller's Host is a Service DNS name (`*.svc.cluster.local`)
	// or a bare address; those never carry a public domain. A bare IP host
	// (e.g. a ClusterIP) has no domain to derive either.
	if host == "localhost" || strings.HasSuffix(host, ".svc.cluster.local") || strings.HasSuffix(host, ".svc") {
		return ""
	}
	if net.ParseIP(host) != nil {
		return ""
	}
	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[2:], ".")
}

// ---- sandbox images ----

// ListOCIImages browses container images in the registry. `owner` selects the
// namespace (default: the deployment toolchain org); `name` narrows to one
// image, listing its tags.
func (s *Service) ListOCIImages(ctx context.Context, req *connect.Request[wsv1.ListOCIImagesRequest]) (*connect.Response[wsv1.ListOCIImagesResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	owner := req.Msg.GetOwner()
	if owner == "" {
		owner = s.toolchainOrg
	}
	if !roles.ValidComponent(owner) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("owner must be a simple name"))
	}
	// A tenant may browse the SHARED catalog namespaces (the toolchain org, the
	// sandbox org and the system `root`), but any other namespace must be one it
	// owns.
	if owner != s.toolchainOrg && owner != s.sandboxOrg && owner != "root" {
		owned, err := s.members.OwnsOrg(tenant, owner)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if !owned {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("namespace not found"))
		}
	}
	name := req.Msg.GetName()
	pkgs, err := s.git.ListContainerPackages(ctx, owner, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	host := ""
	if s.builder != nil {
		host = s.builder.RegistryHost
	}
	out := make([]*wsv1.OCIImage, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, &wsv1.OCIImage{
			Owner: p.Owner, Name: p.Name, Tag: p.Tag,
			Ref: host + "/" + p.Owner + "/" + p.Name + ":" + p.Tag,
		})
	}
	return connect.NewResponse(&wsv1.ListOCIImagesResponse{Images: out}), nil
}

// BuildSandboxImage builds an image from a repository Dockerfile (context = a
// repo subdirectory) and pushes it under the deployment registry. The result
// is NOT auto-registered in the catalog (the catalog is deployment-curated).
func (s *Service) BuildSandboxImage(ctx context.Context, req *connect.Request[wsv1.BuildSandboxImageRequest]) (*connect.Response[wsv1.BuildSandboxImageResponse], error) {
	if _, err := s.sandboxAuth(ctx, req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	org, repo, ref := m.GetOrg(), m.GetRepo(), m.GetRef()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo must be simple names"))
	}
	if ref == "" {
		ref = roles.MainBranch
	}
	if !roles.ValidComponent(ref) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref must be a simple name"))
	}
	if !imageNameRe.MatchString(m.GetImage()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image must be a single simple name"))
	}
	if !roles.ValidComponent(m.GetTag()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tag must be a simple name"))
	}
	if s.builder == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("image builder not configured"))
	}

	archive, err := s.git.ArchiveTarGz(ctx, org, repo, ref)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fetch repo archive: %w", err))
	}
	dir, cleanup, err := imagebuild.Extract(archive, 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer cleanup()

	// The image is pushed under the SOURCE REPO's org namespace
	// (<registry>/<org>/<image>:<tag>), matching Forgejo's OCI semantics.
	res, err := s.builder.Build(ctx, imagebuild.Request{
		ContextDir: dir,
		Dockerfile: m.GetDockerfile(),
		Context:    m.GetContext(),
		Repo:       org + "/" + m.GetImage(),
		Tag:        m.GetTag(),
		BuildArgs:  m.GetBuildArgs(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.BuildSandboxImageResponse{ImageRef: res.ImageRef, Log: res.Log}), nil
}

// imageNameRe: a single path segment (no '/', no ':').
var imageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ImportImage mirrors an upstream image (public or private) into the registry
// under an org the caller owns, with a no-op `FROM <source>` rebuild. It is the
// OCI analogue of repo-import: the caller names the destination org and the new
// image is recorded under that org (which must belong to the tenant).
func (s *Service) ImportImage(ctx context.Context, req *connect.Request[wsv1.ImportImageRequest]) (*connect.Response[wsv1.ImportImageResponse], error) {
	tenant, err := s.sandboxAuth(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	org, name, tag, source := m.GetOrg(), m.GetName(), m.GetTag(), m.GetSource()
	if !roles.ValidComponent(org) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org must be a simple name"))
	}
	if !imageNameRe.MatchString(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be a single simple name"))
	}
	if !roles.ValidComponent(tag) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tag must be a simple name"))
	}
	if source == "" || strings.ContainsAny(source, " \t\n") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("source must be an image ref"))
	}
	if s.builder == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("image builder not configured"))
	}
	// The destination org must belong to the caller (creating it if needed).
	if err := s.claimOrg(ctx, tenant, org); err != nil {
		return nil, err
	}
	res, err := s.builder.Import(ctx, imagebuild.ImportRequest{
		Source:    source,
		Repo:      org + "/" + name,
		Tag:       tag,
		AuthUser:  m.GetAuthUser(),
		AuthToken: m.GetAuthToken(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.ImportImageResponse{ImageRef: res.ImageRef, Log: res.Log}), nil
}

func toSandboxInfo(sb sandboxmgr.Sandbox) *wsv1.SandboxInfo {
	return &wsv1.SandboxInfo{
		Name: sb.Name, Image: sb.Image, Phase: sb.Phase, Ready: sb.Ready,
		Url: sb.URL, Creator: sb.Creator, CreatedAt: sb.CreatedAt,
		Session: sb.Session,
	}
}

// toMRInfo projects a Forgejo MR onto the wire type.
func toMRInfo(mr forgejo.MRInfo) *wsv1.MRInfo {
	return &wsv1.MRInfo{
		Index: mr.Index, Title: mr.Title, State: mr.State, Head: mr.Head, Base: mr.Base,
		Body: mr.Body, Author: mr.Author, CreatedAt: mr.CreatedAt, UpdatedAt: mr.UpdatedAt,
		Merged: mr.Merged, Mergeable: mr.Mergeable,
		Additions: mr.Additions, Deletions: mr.Deletions, ChangedFiles: mr.ChangedFiles,
		HtmlUrl: mr.HTMLURL,
	}
}

// mimeOfPath guesses a MIME from a file path's extension (empty when unknown).
// Used only as a hint for the client's viewer picker.
func mimeOfPath(path string) string {
	ext := strings.ToLower(path)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i+1:]
	} else {
		return ""
	}
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "bmp":
		return "image/bmp"
	case "ico":
		return "image/x-icon"
	case "pdf":
		return "application/pdf"
	case "mp4":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mov":
		return "video/quicktime"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "json":
		return "application/json"
	case "yaml", "yml":
		return "application/x-yaml"
	case "xml":
		return "application/xml"
	case "md", "markdown":
		return "text/markdown"
	case "csv":
		return "text/csv"
	case "tsv":
		return "text/tab-separated-values"
	case "html", "htm":
		return "text/html"
	case "css":
		return "text/css"
	case "js", "mjs", "cjs":
		return "text/javascript"
	case "ts":
		return "text/x-typescript"
	case "sh", "bash":
		return "text/x-shellscript"
	case "txt", "log":
		return "text/plain"
	}
	return ""
}

// ---- helpers ----

// hopHeaders are protocol-managed by connect-go on the OUTGOING request. The
// connect client sets them itself, so copying the caller's values on top makes
// them duplicate (e.g. `Content-Type: application/connect+proto
// application/connect+proto`), which the agent's connect server rejects with
// 415 Unsupported Media Type. Never forward them.
var hopHeaders = map[string]bool{
	"Content-Type":             true,
	"Content-Length":           true,
	"Connect-Protocol-Version": true,
	"Connect-Timeout-Ms":       true,
	"Connect-Accept-Encoding":  true,
	"Connect-Content-Encoding": true,
	"Accept-Encoding":          true,
	"Host":                     true,
}

// copyHeaders copies the caller's headers (Authorization in particular) so the
// agent sees the real tenant token. Protocol/transport headers are skipped:
// the connect client sets them on the outbound request.
func copyHeaders[T any](dst *connect.Request[T], hdr map[string][]string) {
	for k, vs := range hdr {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Header().Add(k, v)
		}
	}
}

var _ wsv1connect.BranchSessionServiceHandler = (*Service)(nil)
