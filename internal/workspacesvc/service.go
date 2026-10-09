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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"

	"github.com/abcp-sdk/abc-protocol-go/v2/bus"
	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
	"github.com/abcp-sdk/workspace-gateway/internal/gitcommit"
	"github.com/abcp-sdk/workspace-gateway/internal/gitimport"
	"github.com/abcp-sdk/workspace-gateway/internal/helmmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/imagebuild"
	"github.com/abcp-sdk/workspace-gateway/internal/k8swatch"
	"github.com/abcp-sdk/workspace-gateway/internal/members"
	"github.com/abcp-sdk/workspace-gateway/internal/registry"
	"github.com/abcp-sdk/workspace-gateway/internal/roles"
	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/servicesmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/workerclient"
)

// serviceReadyTimeout bounds how long DeployService waits for a freshly-
// deployed pod to become ready. A deterministic failure returns immediately; a
// genuine slow start is reported as not-ready (non-fatal).
const serviceReadyTimeout = 60 * time.Second

// Service implements wsv1connect.BranchSessionServiceHandler.
type Service struct {
	agent    agentv1connect.AgentServiceClient
	members  *members.Store
	git      *forgejo.Client
	sbx      *sandboxmgr.Client
	services *servicesmgr.Client
	helm     *helmmgr.Client
	builder  *imagebuild.Builder
	// registry is the OCI catalog client (standard registry v2 API). Nil falls
	// back to the Forgejo packages API (legacy).
	registry *registry.Client
	runtime  runtimeprofiles.Settings
	// sandboxOrg is the ONLY registry org a sandbox image may come from. The
	// deployment pre-imports worker-bundled images there (see sandbox-images/),
	// so a sandbox can never run an arbitrary upstream image.
	sandboxOrg string
	// defaultSandboxImage is used when CreateSandbox omits an image.
	defaultSandboxImage string
	// sandboxImageRegistryHost, when set, is the ONLY registry a sandbox image
	// may come from (e.g. artifact.worker.svc.cluster.local). A sandbox request
	// naming another registry is refused. Empty = no registry-host guard (only
	// the org guard applies). Distinct from builder.RegistryHost, which is the
	// BUILD/PUSH + catalog target (Forgejo) and must stay unchanged.
	sandboxImageRegistryHost string
	toolchainOrg             string // default owner for ListOCIImages
	svcToken                 string // shared service token (sandbox-only service-to-service)
	svcTenant                string // tenant the service token resolves to (sandbox ownership)
	commits                  *gitcommit.Manager
	sandboxNS                string // namespace services/sandboxes live in (public-host inference)
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
	// goldenDiskCachePVC / goldenDiskCacheSize configure the shared golden-disk
	// cache PVC mounted by VM (Windows/macOS) sandboxes.
	goldenDiskCachePVC  string
	goldenDiskCacheSize string
	// vmProxy, when set, is exported to a VM sandbox (macos/windows) as
	// SANDBOX_PROXY. The VM guest has NO direct egress, so the worker's start.sh
	// persists it (/run/shm/proxy + nginx /proxy) and the worker exports
	// HTTP(S)_PROXY for its jobs — letting guest tools (winget/brew/apt…) reach
	// the network through the cluster proxy. Empty = no injection.
	vmProxy string
	// vmImageTag is the tag applied to VM/device sandbox images (macos/windows/
	// android). Deployment-pinned so the mapping never depends on a mutable
	// alias like `base` (which a registry cleanup can prune). Empty = "base".
	vmImageTag string
	// bootstrap is the rendered package-source bootstrap applied to every
	// sandbox (nil = none). It is derived from SANDBOX_PACKAGE_UPSTREAM, so a
	// registry URL change needs no image rebuild.
	bootstrap *sandboxmgr.Bootstrap
	// hub fans out a single k8s-change signal to WatchWorkspace subscribers.
	hub *workspaceHub
	// bus publishes a mailbox `trigger` to wake a session's turn (the
	// MR-submitted notification). Nil disables it (best-effort).
	bus bus.Bus
}

// Deps configures the service.
type Deps struct {
	Agent   agentv1connect.AgentServiceClient
	Members *members.Store
	Forgejo *forgejo.Client
	Sandbox *sandboxmgr.Client
	// Services manages long-lived Deployments.
	Services *servicesmgr.Client
	// Helm manages chart releases (templating + apply). Nil disables the
	// helm-* RPCs.
	Helm *helmmgr.Client
	// Builder builds/derives images (repo Dockerfile / base image -> registry).
	Builder *imagebuild.Builder
	// Registry is the OCI catalog client (standard registry v2 API, e.g.
	// artifact). When set, ListOCIImages browses it instead of Forgejo.
	Registry *registry.Client
	// Runtime holds the deployment's runtime knobs (KVM/GPU devices).
	Runtime runtimeprofiles.Settings
	// SandboxOrg is the registry org sandbox images MUST come from. A sandbox
	// request naming an image outside it is refused.
	SandboxOrg string
	// DefaultSandboxImage is used when CreateSandbox omits an image.
	DefaultSandboxImage string
	// SandboxImageRegistryHost, when set, is the ONLY registry a sandbox image
	// may come from (artifact). Distinct from the builder's registry host (the
	// build/push + catalog target). Empty = no registry-host guard.
	SandboxImageRegistryHost string
	// SandboxBootstrap, when set, is applied to every sandbox (package-source
	// init container + config volumes). Built from SANDBOX_PACKAGE_UPSTREAM.
	SandboxBootstrap *sandboxmgr.Bootstrap
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
	// ServiceLogTail is the default number of service-log lines returned.
	ServiceLogTail int64
	// PVCStorageClass is the StorageClass CreatePVC uses ("" = cluster default).
	// Only the self-hosted local-path class is supported.
	PVCStorageClass string
	// PVCDefaultSize is used when CreatePVC omits a size (e.g. "1Gi").
	PVCDefaultSize string
	// GoldenDiskCachePVC / GoldenDiskCacheSize configure the shared golden-disk
	// cache PVC mounted by VM (Windows/macOS) sandboxes.
	GoldenDiskCachePVC  string
	GoldenDiskCacheSize string
	// VMProxy is exported to VM sandboxes as SANDBOX_PROXY (empty = none).
	VMProxy string
	// VMImageTag is the tag for VM/device sandbox images (empty = "base").
	VMImageTag string
	// Bus publishes mailbox triggers (e.g. the MR-submitted notification). Nil
	// disables the notification; it is best-effort and never fails the RPC.
	Bus bus.Bus
}

// New builds the service.
func New(d Deps) *Service {
	commits := gitcommit.NewManager()
	commits.SetBaseDir(d.GitCloneDir)
	var sources []k8swatch.Source
	if d.Sandbox != nil {
		sources = append(sources, d.Sandbox.Watch)
	}
	if d.Services != nil {
		sources = append(sources, d.Services.WatchDeployments, d.Services.WatchPVCs)
	}
	if d.Helm != nil {
		sources = append(sources, d.Helm.Watch)
	}
	return &Service{
		agent: d.Agent, members: d.Members, git: d.Forgejo, sbx: d.Sandbox,
		services: d.Services,
		helm:     d.Helm,
		builder:  d.Builder, runtime: d.Runtime,
		registry:   d.Registry,
		sandboxOrg: d.SandboxOrg, defaultSandboxImage: d.DefaultSandboxImage,
		sandboxImageRegistryHost: d.SandboxImageRegistryHost,
		bootstrap:                d.SandboxBootstrap,
		toolchainOrg:             d.ToolchainOrg,
		svcToken:                 d.ServiceToken, svcTenant: d.ServiceTenant,
		commits:             commits,
		sandboxNS:           d.SandboxNamespace,
		publicServiceDomain: d.PublicServiceDomain,
		serviceLogTail:      d.ServiceLogTail,
		pvcStorageClass:     d.PVCStorageClass,
		pvcDefaultSize:      d.PVCDefaultSize,
		goldenDiskCachePVC:  d.GoldenDiskCachePVC,
		goldenDiskCacheSize: d.GoldenDiskCacheSize,
		vmProxy:             d.VMProxy,
		vmImageTag:          d.VMImageTag,
		hub:                 newWorkspaceHub(sources...),
		bus:                 d.Bus,
	}
}

// renderRuntime maps the caller's (kvm, gpuCount) to pod-level settings using
// the deployment's runtime knobs.
func (s *Service) renderRuntime(kvm bool, gpuCount int32) runtimeprofiles.Rendered {
	return s.runtime.Render(kvm, int(gpuCount))
}

// buildArgs merges the caller's build-args with the deployment's package-source
// defaults (from SANDBOX_PACKAGE_UPSTREAM). A Dockerfile that honors these ARGs
// (GOPROXY / NPM_CONFIG_REGISTRY / PIP_INDEX_URL / …) then fetches packages
// from artifact at BUILD time, same source as at runtime. Caller values win.
func (s *Service) buildArgs(caller map[string]string) map[string]string {
	merged := map[string]string{}
	for k, v := range runtimeprofiles.PackageEnv(s.runtime.PackageUpstream) {
		merged[k] = v
	}
	for k, v := range caller {
		merged[k] = v
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// sandboxEnv merges the caller's env with the deployment's package-source env
// (from SANDBOX_PACKAGE_UPSTREAM). This is applied ONLY to sandboxes — never to
// user services (which use renderRuntime directly). Caller values win.
func (s *Service) sandboxEnv(caller map[string]string) map[string]string {
	if s.runtime.PackageUpstream == "" {
		return caller
	}
	merged := map[string]string{}
	for k, v := range runtimeprofiles.PackageEnv(s.runtime.PackageUpstream) {
		merged[k] = v
	}
	// The registry root, also read by the bootstrap init container.
	merged["SANDBOX_PACKAGE_UPSTREAM"] = s.runtime.PackageUpstream
	for k, v := range caller {
		merged[k] = v
	}
	return merged
}

// sandboxAuth resolves the caller's tenant for a sandbox RPC. A request bearing
// the shared service token resolves to the configured service tenant (unless it
// names a real tenant via X-Abc-Tenant); every other request resolves through
// the agent identity as usual.
func (s *Service) sandboxAuth(ctx context.Context, hdr *connect.Header) (string, error) {
	if t, ok := s.serviceTenant(hdr); ok {
		return t, nil
	}
	return s.resolveTenant(ctx, hdr)
}

// serviceTenant resolves a SERVICE-TOKEN caller's tenant: the `X-Abc-Tenant`
// header when present and valid (the workspace extension acts on behalf of the
// real caller), else the configured synthetic service tenant. ok=false when the
// request does not carry the shared service token.
func (s *Service) serviceTenant(hdr *connect.Header) (string, bool) {
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

func headerValue(hdr *connect.Header, name string) string {
	if hdr == nil {
		return ""
	}
	return strings.TrimSpace(hdr.Get(name))
}

// forwardedHost returns the public host the client used: X-Forwarded-Host
// (set by the edge) when present, else the Host header.
func forwardedHost(hdr *connect.Header) string {
	if h := headerValue(hdr, "X-Forwarded-Host"); h != "" {
		return h
	}
	return headerValue(hdr, "Host")
}

func bearerToken(hdr *connect.Header) string {
	if hdr == nil {
		return ""
	}
	for _, v := range hdr.Values("Authorization") {
		if strings.HasPrefix(v, "Bearer ") {
			return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		}
	}
	return ""
}

// hdrFrom returns the inbound request headers for a handler context, or nil when
// there is no CallInfo (e.g. a direct unit-test call).
func hdrFrom(ctx context.Context) *connect.Header {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		return info.RequestHeader()
	}
	// Test seam: unit tests calling a handler directly have no server CallInfo,
	// so they may seed the headers on the context.
	if h, ok := ctx.Value(testHdrKey{}).(*connect.Header); ok {
		return h
	}
	return nil
}

// testHdrKey carries headers for tests that call handlers without a transport.
type testHdrKey struct{}

// WithTestHeaders seeds request headers on ctx for a direct handler call in a
// test (connect v2 exposes no exported server-CallInfo constructor).
func WithTestHeaders(ctx context.Context, h *connect.Header) context.Context {
	return context.WithValue(ctx, testHdrKey{}, h)
}

// ---- tenant ----

// resolveTenant asks the agent who the caller is (forwarding their token). A
// service-token caller is resolved from X-Abc-Tenant / the service tenant so the
// extension can act for the real tenant without an agent identity round-trip.
func (s *Service) resolveTenant(ctx context.Context, hdr *connect.Header) (string, error) {
	if t, ok := s.serviceTenant(hdr); ok {
		return t, nil
	}
	req := &agentv1.GetIdentityRequest{}
	copyHeaders(req, hdr)
	res, err := s.agent.GetIdentity(ctx, req)
	if err != nil {
		return "", err
	}
	t := res.GetTenant()
	if t == "" {
		return "", connect.NewError(connect.CodeUnauthenticated, "no tenant for credential")
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
func (s *Service) EnsureBranchSession(ctx context.Context, req *wsv1.EnsureBranchSessionRequest) (*wsv1.EnsureBranchSessionResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	org, repo, branch := req.GetOrg(), req.GetRepo(), req.GetBranch()
	if branch == "" {
		branch = roles.MainBranch
	}
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) || !roles.ValidComponent(branch) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org/repo/branch must be simple names")
	}
	if err := s.claimRepo(ctx, tenant, org, repo); err != nil {
		return nil, err
	}

	session := roles.SessionName(org, repo, branch)
	role := roles.RoleForBranch(branch)
	// Idempotent: create only when absent. The agent refuses a duplicate, so we
	// probe first (GetSession -> NotFound means absent).
	if !s.sessionExists(ctx, hdrFrom(ctx), session) {
		if err := s.createSession(ctx, hdrFrom(ctx), session, roles.PresetFor(role), req.GetModel(), req.GetLocale()); err != nil {
			return nil, err
		}
	}
	sbName, sbPhase, refs := s.sandboxFields(ctx, session)
	return &wsv1.EnsureBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Org: org, Repo: repo, Branch: branch,
		Role: string(role), Preset: roles.PresetFor(role),
		Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
	}}, nil
}

// sessionExists reports whether the agent already holds a session with this id.
func (s *Service) sessionExists(ctx context.Context, hdr *connect.Header, session string) bool {
	r := &agentv1.GetSessionRequest{Id: session}
	copyHeaders(r, hdr)
	res, err := s.agent.GetSession(ctx, r)
	return err == nil && res.GetSession() != nil
}

// claimOrg ensures `org` exists and is owned by tenant. A pre-existing org not
// owned by the tenant is refused (import must not land in someone else's org).
func (s *Service) claimOrg(ctx context.Context, tenant, org string) error {
	owned, err := s.members.OwnsOrg(tenant, org)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if owned {
		return nil
	}
	// An org is GLOBALLY unique: if another tenant already owns it (or it exists
	// in Forgejo under a different owner), refuse rather than double-book it.
	if other, err := s.members.OrgOwner(org); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	} else if other != "" && other != tenant {
		return connect.NewError(connect.CodePermissionDenied, "organization is owned by another tenant")
	}
	if err := s.git.EnsureOrg(ctx, org); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("ensure org: %w", err).Error())
	}
	if err := s.members.AddOrg(tenant, org); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return nil
}

// claimRepo ensures org/repo exists and is owned by tenant. A pre-existing repo
// owned by another tenant is refused.
func (s *Service) claimRepo(ctx context.Context, tenant, org, repo string) error {
	exists, err := s.git.RepoExists(ctx, org, repo)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if exists {
		owned, err := s.members.OwnsRepo(tenant, org, repo)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		if !owned {
			return connect.NewError(connect.CodePermissionDenied, "repository is owned by another tenant")
		}
		return nil
	}
	if _, err := s.git.EnsureRepo(ctx, org, repo); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("ensure repo: %w", err).Error())
	}
	if err := s.members.AddRepo(tenant, org, repo); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
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
func (s *Service) ForkBranchSession(ctx context.Context, req *wsv1.ForkBranchSessionRequest) (*wsv1.ForkBranchSessionResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	org, repo, parentBranch, ok := roles.ParseSession(req.GetSession())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "session must be org:repo:branch")
	}
	branch := req.GetBranch()
	if branch == "" || !roles.ValidComponent(branch) || branch == roles.MainBranch {
		return nil, connect.NewError(connect.CodeInvalidArgument, "branch must be a legal, non-main name")
	}
	// Only a MAIN session may create branches (manual fork). A feature-branch
	// session is bound to exactly one branch and must not spawn more.
	if parentBranch != roles.MainBranch {
		return nil, connect.NewError(connect.CodePermissionDenied,
			"only a main session can create branches; a feature branch cannot fork new branches")
	}
	// An agent session may only fork a branch in its OWN repository; the parent
	// must belong to the caller's repo (a human webui caller sends no session
	// header and keeps full tenant access).
	if !branchTargetAllowed(sessionFromHeaders(hdrFrom(ctx)), org, repo) {
		return nil, connect.NewError(connect.CodePermissionDenied,
			"a session may only create branches in its own repository")
	}
	owned, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, "branch session not found")
	}
	if !s.sessionExists(ctx, hdrFrom(ctx), req.GetSession()) {
		return nil, connect.NewError(connect.CodeNotFound, "parent session not found")
	}

	session := roles.SessionName(org, repo, branch)
	role := roles.Developer
	fr := &agentv1.ForkRequest{
		Id:        req.GetSession(),
		Name:      session,
		MessageId: req.GetMessageId(),
		Preset:    roles.PresetFor(role),
	}
	copyHeaders(fr, hdrFrom(ctx))
	if _, err := s.agent.Fork(ctx, fr); err != nil {
		return nil, err
	}
	sbName, sbPhase, refs := s.sandboxFields(ctx, session)
	return &wsv1.ForkBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Org: org, Repo: repo, Branch: branch,
		Role: string(role), Preset: roles.PresetFor(role),
		Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
	}}, nil
}

// CreateFreeSession creates a standalone (non-repo-bound) session. Free
// sessions carry a tenant-scoped role: admin (manage org/repo) or explorer
// (read-only). Any number of each may exist; the role decides what the session
// may DO, never what it may SEE (visibility is the tenant).
func (s *Service) CreateFreeSession(ctx context.Context, req *wsv1.CreateFreeSessionRequest) (*wsv1.CreateFreeSessionResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	role := roles.Role(req.GetRole())
	if role != roles.Admin && role != roles.Explorer {
		return nil, connect.NewError(connect.CodeInvalidArgument, "role must be admin|explorer")
	}
	name := req.GetName()
	if name == "" || !roles.ValidComponent(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name required (simple)")
	}
	if err := s.members.AddFreeSession(tenant, name, string(role)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if err := s.createSession(ctx, hdrFrom(ctx), name, roles.PresetFor(role), req.GetModel(), req.GetLocale()); err != nil {
		return nil, err
	}
	return &wsv1.CreateFreeSessionResponse{Session: name, Preset: roles.PresetFor(role)}, nil
}

// createSession forwards a trusted CreateSession to the agent. `locale`, when
// non-empty ("zh"/"en"), pins the session's agent language for its lifetime.
//
// An EMPTY locale means "follow the tenant default". We resolve the tenant's
// configured `locale` HERE and pass it explicitly, because the agent stores a
// pinned locale and would otherwise pin an empty request to English (the
// agent's normalizeLocale(”) == 'en'), ignoring the tenant's zh config.
func (s *Service) createSession(ctx context.Context, hdr *connect.Header, name, preset, model, locale string) error {
	if strings.TrimSpace(locale) == "" {
		locale = s.tenantLocale(ctx, hdr)
	}
	msg := &agentv1.CreateSessionRequest{Name: name, Preset: preset, Model: model, Locale: normalizeLocale(locale)}
	r := msg
	copyHeaders(r, hdr)
	_, err := s.agent.CreateSession(ctx, r)
	return err
}

// tenantLocale reads the tenant's configured agent language (`locale` KV),
// forwarding the caller's credential. Empty on any error (the agent then falls
// back to its own default).
func (s *Service) tenantLocale(ctx context.Context, hdr *connect.Header) string {
	r := &agentv1.GetConfigRequest{Key: "locale"}
	copyHeaders(r, hdr)
	res, err := s.agent.GetConfig(ctx, r)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.GetValue())
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
func (s *Service) ListBranchSessions(ctx context.Context, req *wsv1.ListBranchSessionsRequest) (*wsv1.ListBranchSessionsResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	lr := &agentv1.ListSessionsRequest{}
	copyHeaders(lr, hdrFrom(ctx))
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
	for _, sess := range res.GetSessions() {
		name := sess.GetName()
		refs := refsFor(name)
		sbName, sbPhase := repOf(refs)
		if org, repo, branch, ok := roles.ParseSession(name); ok {
			owned, err := s.members.OwnsRepo(tenant, org, repo)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
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
	return &wsv1.ListBranchSessionsResponse{BranchSessions: out}, nil
}

// GetBranchSession returns one branch session (or free session).
func (s *Service) GetBranchSession(ctx context.Context, req *wsv1.GetBranchSessionRequest) (*wsv1.GetBranchSessionResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	session := req.GetSession()
	if org, repo, branch, ok := roles.ParseSession(session); ok {
		owned, err := s.members.OwnsRepo(tenant, org, repo)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		if !owned {
			return nil, connect.NewError(connect.CodeNotFound, "branch session not found")
		}
		role := roles.RoleForBranch(branch)
		sbName, sbPhase, refs := s.sandboxFields(ctx, session)
		return &wsv1.GetBranchSessionResponse{BranchSession: &wsv1.BranchSession{
			Session: session, Org: org, Repo: repo, Branch: branch,
			Role: string(role), Preset: roles.PresetFor(role),
			Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
		}}, nil
	}
	role, ok, _ := s.members.FreeRole(tenant, session)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "branch session not found")
	}
	sbName, sbPhase, refs := s.sandboxFields(ctx, session)
	return &wsv1.GetBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Role: role, Preset: role,
		Sandbox: sbName, Phase: sbPhase, Sandboxes: refs,
	}}, nil
}

// DeleteBranchSession deletes a branch session (its sandbox + agent session)
// or a free session. For a NON-MAIN branch session it ALSO deletes the
// underlying git branch: repo:branch <-> session is 1:1, so leaving the branch
// behind would strand an orphan branch with no session. `main` is protected —
// deleting the main session never deletes the default branch.
func (s *Service) DeleteBranchSession(ctx context.Context, req *wsv1.DeleteBranchSessionRequest) (*wsv1.DeleteBranchSessionResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	session := req.GetSession()
	org, repo, branch, isBranch := roles.ParseSession(session)
	if isBranch {
		if owned, _ := s.members.OwnsRepo(tenant, org, repo); !owned {
			return nil, connect.NewError(connect.CodeNotFound, "branch session not found")
		}
	} else if _, ok, _ := s.members.FreeRole(tenant, session); !ok {
		return nil, connect.NewError(connect.CodeNotFound, "branch session not found")
	}
	// Best-effort sandbox cleanup + session delete.
	s.deleteSessionCascade(ctx, hdrFrom(ctx), tenant, session)
	// A branch session owns its branch 1:1: delete it too (except main, which is
	// protected). The git deletion is idempotent.
	if isBranch && branch != roles.MainBranch {
		if err := s.git.DeleteBranch(ctx, org, repo, branch); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
	}
	return &wsv1.DeleteBranchSessionResponse{Ok: true}, nil
}

// DeleteBranch removes a repo branch AND its branch session (session + sandboxes
// cascade). Only the owning tenant may delete; `main` is refused (the default
// branch is protected). The git deletion is idempotent (an already-absent
// branch is fine); the branch session is deleted even if the branch was gone.
func (s *Service) DeleteBranch(ctx context.Context, req *wsv1.DeleteBranchRequest) (*wsv1.DeleteBranchResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	org, repo, branch := req.GetOrg(), req.GetRepo(), req.GetBranch()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) || !roles.ValidComponent(branch) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org/repo/branch must be simple names")
	}
	if branch == roles.MainBranch {
		return nil, connect.NewError(connect.CodeInvalidArgument, "refusing to delete the default branch")
	}
	owned, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, "repository not found")
	}
	// Delete the branch session (session + sandboxes) first, then the git branch.
	session := roles.SessionName(org, repo, branch)
	s.deleteSessionCascade(ctx, hdrFrom(ctx), tenant, session)
	if err := s.git.DeleteBranch(ctx, org, repo, branch); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeleteBranchResponse{Ok: true}, nil
}

// DeleteRepo removes a repository AND every one of its branch sessions (each
// cascading to its sandboxes), then drops the git repo + ownership row. Only
// the owning tenant may delete. The org is left in place. Admin action.
func (s *Service) DeleteRepo(ctx context.Context, req *wsv1.DeleteRepoRequest) (*wsv1.DeleteRepoResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	org, repo := req.GetOrg(), req.GetRepo()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org/repo must be simple names")
	}
	owned, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, "repository not found")
	}
	// Cascade every branch session of this repo (the agent is the source of
	// truth for which sessions exist). Best-effort per session.
	prefix := org + ":" + repo + ":"
	lr := &agentv1.ListSessionsRequest{}
	copyHeaders(lr, hdrFrom(ctx))
	if res, err := s.agent.ListSessions(ctx, lr); err == nil {
		for _, sess := range res.GetSessions() {
			if name := sess.GetName(); strings.HasPrefix(name, prefix) {
				s.deleteSessionCascade(ctx, hdrFrom(ctx), tenant, name)
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
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if err := s.members.RemoveRepo(tenant, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeleteRepoResponse{Ok: true}, nil
}

// deleteSessionCascade is the SINGLE session-deletion path: it reclaims the
// session's sandboxes, deletes the agent session, and clears a free-session row.
// Every delete entry point (DeleteBranchSession / DeleteBranch / DeleteRepo)
// funnels through here so the semantics never drift.
//
// Sandboxes are matched by their `worker-manager/session` annotation (the ONLY
// reliable key: the creator annotation holds just the tenant). A sandbox with
// no session annotation falls back to a name match for legacy sandboxes.
func (s *Service) deleteSessionCascade(ctx context.Context, hdr *connect.Header, tenant, session string) {
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
	r := &agentv1.DeleteSessionRequest{Id: session}
	copyHeaders(r, hdr)
	_, _ = s.agent.DeleteSession(ctx, r)
	_ = s.members.DeleteFreeSession(tenant, session)
}

// ---- git browse ----

func (s *Service) ensureVisible(ctx context.Context, hdr *connect.Header, org, repo string) error {
	tenant, err := s.resolveTenant(ctx, hdr)
	if err != nil {
		return err
	}
	ok, err := s.members.OwnsRepo(tenant, org, repo)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return connect.NewError(connect.CodeNotFound, "repository not found")
	}
	return nil
}

// ListRepos lists the tenant's visible repos.
func (s *Service) ListRepos(ctx context.Context, req *wsv1.ListReposRequest) (*wsv1.ListReposResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	repos, err := s.members.ListRepos(tenant)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
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
	return &wsv1.ListReposResponse{Repos: out}, nil
}

func (s *Service) Tree(ctx context.Context, req *wsv1.TreeRequest) (*wsv1.TreeResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	entries, truncated, err := s.git.Tree(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.TreeEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &wsv1.TreeEntry{Path: e.Path, Type: e.Type, Size: e.Size})
	}
	return &wsv1.TreeResponse{Entries: out, Truncated: truncated}, nil
}

func (s *Service) ReadBlob(ctx context.Context, req *wsv1.ReadBlobRequest) (*wsv1.ReadBlobResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	content, sha, err := s.git.ReadBlob(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.ReadBlobResponse{Content: content, Sha: sha}, nil
}

func (s *Service) Log(ctx context.Context, req *wsv1.LogRequest) (*wsv1.LogResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	commits, hasMore, err := s.git.Log(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath(), int(m.GetLimit()), int(m.GetOffset()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.CommitInfo, 0, len(commits))
	for _, c := range commits {
		out = append(out, &wsv1.CommitInfo{Sha: c.SHA, Message: c.Message, Author: c.Author, Date: c.Date})
	}
	return &wsv1.LogResponse{Commits: out, HasMore: hasMore}, nil
}

func (s *Service) Branches(ctx context.Context, req *wsv1.BranchesRequest) (*wsv1.BranchesResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	branches, err := s.git.Branches(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.BranchInfo, 0, len(branches))
	for _, b := range branches {
		out = append(out, &wsv1.BranchInfo{Name: b.Name, Sha: b.SHA})
	}
	return &wsv1.BranchesResponse{Branches: out}, nil
}

// ReadRaw returns a file's raw bytes (any content type) so the webui can
// preview binary files (images / PDF / office) with the chat's viewers.
//
// It also classifies the content: `is_text` is true only for valid UTF-8
// without NUL bytes AND at most maxTextBytes (larger files are returned as
// non-text so the client offers a download instead of highlighting).
func (s *Service) ReadRaw(ctx context.Context, req *wsv1.ReadRawRequest) (*wsv1.ReadRawResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	data, sha, err := s.git.RawFile(ctx, m.GetOrg(), m.GetRepo(), m.GetRef(), m.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &wsv1.ReadRawResponse{
		Data: data, Sha: sha, Mime: mimeOfPath(m.GetPath()),
		IsText: isTextContent(data),
	}, nil
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
func (s *Service) Tags(ctx context.Context, req *wsv1.TagsRequest) (*wsv1.TagsResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	tags, err := s.git.Tags(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.TagInfo, 0, len(tags))
	for _, t := range tags {
		out = append(out, &wsv1.TagInfo{Name: t.Name, Sha: t.SHA})
	}
	return &wsv1.TagsResponse{Tags: out}, nil
}

// ListReleases lists a repository's releases (read-only).
func (s *Service) ListReleases(ctx context.Context, req *wsv1.ReleasesRequest) (*wsv1.ReleasesResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	rels, err := s.git.ListReleases(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
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
	return &wsv1.ReleasesResponse{Releases: out}, nil
}

// GetReleaseAsset proxies one release asset's bytes (the browser may not reach
// the git host directly).
func (s *Service) GetReleaseAsset(ctx context.Context, req *wsv1.ReleaseAssetRequest) (*wsv1.ReleaseAssetResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	data, name, mime, err := s.git.GetReleaseAsset(ctx, m.GetOrg(), m.GetRepo(), m.GetReleaseId(), m.GetAssetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &wsv1.ReleaseAssetResponse{Data: data, Name: name, Mime: mime}, nil
}

// GetCommit returns one commit's metadata.
func (s *Service) GetCommit(ctx context.Context, req *wsv1.GetCommitRequest) (*wsv1.GetCommitResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	c, err := s.git.GetCommit(ctx, m.GetOrg(), m.GetRepo(), m.GetSha())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &wsv1.GetCommitResponse{Commit: &wsv1.CommitDetail{
		Sha: c.SHA, Message: c.Message, Author: c.Author, AuthorEmail: c.AuthorEmail,
		Date: c.Date, Parents: c.Parents, HtmlUrl: c.HTMLURL,
	}}, nil
}

// CommitDiff returns the unified diff of one commit.
func (s *Service) CommitDiff(ctx context.Context, req *wsv1.CommitDiffRequest) (*wsv1.DiffResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	diff, err := s.git.CommitDiff(ctx, m.GetOrg(), m.GetRepo(), m.GetSha())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &wsv1.DiffResponse{Diff: diff}, nil
}

// GetMR returns one change request's full detail.
func (s *Service) GetMR(ctx context.Context, req *wsv1.GetMRRequest) (*wsv1.GetMRResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mr, err := s.git.GetMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	info := toMRInfo(mr)
	// Attach the submitting session (best-effort; the head is an anonymous
	// `mr/...` branch, so this is the only way to find the origin).
	if sub, ok, serr := s.members.MRSubmission(s.tenantOf(ctx, hdrFrom(ctx)), m.GetOrg(), m.GetRepo(), m.GetIndex()); serr == nil && ok {
		info.OriginSession = sub.OriginSession
	}
	return &wsv1.GetMRResponse{Mr: info}, nil
}

// MRDiff returns the unified diff of one change request.
func (s *Service) MRDiff(ctx context.Context, req *wsv1.MRDiffRequest) (*wsv1.MRDiffResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	diff, err := s.git.MRDiff(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &wsv1.MRDiffResponse{Diff: diff}, nil
}

// ListMRComments lists the (read-only) MR conversation.
func (s *Service) ListMRComments(ctx context.Context, req *wsv1.ListMRCommentsRequest) (*wsv1.ListMRCommentsResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	comments, err := s.git.MRComments(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.MRCommentInfo, 0, len(comments))
	for _, c := range comments {
		out = append(out, &wsv1.MRCommentInfo{
			Id: c.ID, Author: c.Author, Body: c.Body,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	return &wsv1.ListMRCommentsResponse{Comments: out}, nil
}

// EnsureRepo creates the org/repo (protecting main) and records ownership.
// Admin-only: creating org/repo is a gateway-verified admin action.
func (s *Service) EnsureRepo(ctx context.Context, req *wsv1.EnsureRepoRequest) (*wsv1.EnsureRepoResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := s.requireAdmin(tenant); err != nil {
		return nil, err
	}
	org, repo := req.GetOrg(), req.GetRepo()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org/repo must be simple names")
	}
	created, err := s.git.EnsureRepo(ctx, org, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if err := s.members.AddRepo(tenant, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	// Auto-create the `org:repo:main` branch session (repo:branch <-> session,
	// 1:1). Idempotent: a re-ensure leaves an existing session untouched.
	s.ensureMainSession(ctx, hdrFrom(ctx), org, repo)
	return &wsv1.EnsureRepoResponse{Created: created}, nil
}

// CreateOrg creates an organization owned by the caller's tenant (idempotent:
// an org already owned is returned unchanged). Admin action.
func (s *Service) CreateOrg(ctx context.Context, req *wsv1.CreateOrgRequest) (*wsv1.CreateOrgResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := s.requireAdmin(tenant); err != nil {
		return nil, err
	}
	org := req.GetOrg()
	if !roles.ValidComponent(org) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org must be a simple name")
	}
	if err := s.claimOrg(ctx, tenant, org); err != nil {
		return nil, err
	}
	return &wsv1.CreateOrgResponse{Org: org}, nil
}

// ListOrgs lists the caller's tenant-owned orgs (including empty ones, which
// ListRepos cannot surface).
func (s *Service) ListOrgs(ctx context.Context, req *wsv1.ListOrgsRequest) (*wsv1.ListOrgsResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	orgs, err := s.members.ListOrgs(tenant)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.ListOrgsResponse{Orgs: orgs}, nil
}

// ---- tenant-scoped repo operations (the extension's ONLY path to Forgejo) ----
//
// Every one of these first calls ensureVisible, so a tenant can only touch a
// repo it owns. This replaces the extension's former direct Forgejo calls made
// with a SHARED admin token (which exposed every tenant's repos).

// RepoMeta returns one repository's metadata (ownership-checked).
func (s *Service) RepoMeta(ctx context.Context, req *wsv1.RepoMetaRequest) (*wsv1.RepoMetaResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	info, err := s.git.GetRepo(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, mrError(err)
	}
	return &wsv1.RepoMetaResponse{
		Org: info.Org, Repo: info.Repo, DefaultBranch: info.DefaultBranch, Private: info.Private, Empty: info.Empty,
	}, nil
}

// Contents reads a file (text) or lists a directory at ref/path.
func (s *Service) Contents(ctx context.Context, req *wsv1.ContentsRequest) (*wsv1.ContentsResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
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
	return res, nil
}

// Compare returns per-file patches between two refs.
func (s *Service) Compare(ctx context.Context, req *wsv1.CompareRequest) (*wsv1.CompareResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	// Compute the compare with go-git: Forgejo's compare API returns an EMPTY
	// `patch` field on 1.22, so the gateway produces the full per-file diff.
	files, err := gitcommit.Compare(ctx, s.git.GitURL(m.GetOrg(), m.GetRepo()), "root", s.git.Token(), m.GetBase(), m.GetHead(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.CompareFile, 0, len(files))
	for _, f := range files {
		out = append(out, &wsv1.CompareFile{
			Path: f.Path, Status: f.Status,
			Additions: int32(f.Additions), Deletions: int32(f.Deletions), Patch: f.Patch,
		})
	}
	return &wsv1.CompareResponse{Files: out}, nil
}

// CreateTag creates a tag at a target ref.
func (s *Service) CreateTag(ctx context.Context, req *wsv1.CreateTagRequest) (*wsv1.CreateTagResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.git.CreateTagAt(ctx, m.GetOrg(), m.GetRepo(), m.GetName(), m.GetTarget()); err != nil {
		return nil, mrError(err)
	}
	return &wsv1.CreateTagResponse{Ok: true}, nil
}

// CreateBranch creates a branch from an existing ref.
func (s *Service) CreateBranch(ctx context.Context, req *wsv1.CreateBranchRequest) (*wsv1.CreateBranchResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	// An agent session may only create branches in its OWN repository (a human
	// webui caller sends no session header and keeps full tenant access).
	if !branchTargetAllowed(sessionFromHeaders(hdrFrom(ctx)), m.GetOrg(), m.GetRepo()) {
		return nil, connect.NewError(connect.CodePermissionDenied,
			"a session may only create branches in its own repository")
	}
	if err := s.git.CreateBranch(ctx, m.GetOrg(), m.GetRepo(), m.GetName(), m.GetFrom()); err != nil {
		return nil, mrError(err)
	}
	return &wsv1.CreateBranchResponse{Ok: true}, nil
}

// branchTargetAllowed reports whether `callerSession` may act on the repo
// `org/repo` for a branch-scoped mutation. An EMPTY caller (human webui, no
// X-Session-Name) is unrestricted (the tenant console); a branch session is
// confined to its own org/repo.
func branchTargetAllowed(callerSession, org, repo string) bool {
	if callerSession == "" {
		return true
	}
	sOrg, sRepo, _, ok := roles.ParseSession(callerSession)
	if !ok {
		return true // non-branch session (free): no repo binding to compare
	}
	return sOrg == org && sRepo == repo
}

// Archive returns a repo tree at a ref as a tar.gz (sandbox checkout).
func (s *Service) Archive(ctx context.Context, req *wsv1.ArchiveRequest) (*wsv1.ArchiveResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	data, err := s.git.ArchiveTarGz(ctx, m.GetOrg(), m.GetRepo(), m.GetRef())
	if err != nil {
		return nil, mrError(err)
	}
	// Forgejo wraps the archive in one `<repo>/` directory; strip it so the
	// caller can unpack the repo tree directly into the directory it names
	// (`dest` = the exact landing directory, supporting rename/nesting).
	data, err = imagebuild.StripTop(data)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.ArchiveResponse{Data: data}, nil
}

// ensureMainSession idempotently creates the `org:repo:main` session. Best-effort:
// a repo is still usable if this fails.
func (s *Service) ensureMainSession(ctx context.Context, hdr *connect.Header, org, repo string) {
	session := roles.SessionName(org, repo, roles.MainBranch)
	if s.sessionExists(ctx, hdr, session) {
		return
	}
	if err := s.createSession(ctx, hdr, session, roles.PresetFor(roles.Developer), "", ""); err != nil {
		log.Printf("warn: ensure main session %s: %v", session, err)
	}
}

// ImportRepo migrates an EXTERNAL git repository into `org` (which must belong
// to the caller's tenant). `ref` selects the SOURCE ref to import — a branch,
// a tag, or any revision — and ALWAYS lands on the new repo's `main`; an empty
// `ref` imports the source's HEAD branch. Only `main` is created (no other
// branches/tags are carried over). The imported repo is PUBLIC and its main
// branch session is ensured. An existing repo is refused (never overwritten).
func (s *Service) ImportRepo(ctx context.Context, req *wsv1.ImportRepoRequest) (*wsv1.ImportRepoResponse, error) {
	tenant, err := s.resolveTenant(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	m := req
	org, url := m.GetOrg(), m.GetUrl()
	if !roles.ValidComponent(org) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org must be a simple name")
	}
	if url == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "url is required")
	}
	repo := m.GetRepo()
	if repo == "" {
		repo = deriveRepoName(url)
	}
	if !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "repo must be a simple name")
	}
	// The org must belong to the tenant (an unknown org is created for it).
	if err := s.claimOrg(ctx, tenant, org); err != nil {
		return nil, err
	}
	// Refuse to overwrite.
	if ok, err := s.git.RepoExists(ctx, org, repo); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	} else if ok {
		return nil, connect.NewError(connect.CodeAlreadyExists, "repository already exists")
	}

	// Create an EMPTY, PUBLIC destination repo (no auto-init): the import push
	// will establish `main`. Never use Forgejo's mirror-migrate here — it
	// fetches every `refs/pull/*`, which is multi-GB/multi-minute on a popular
	// upstream (e.g. octocat/Spoon-Knife with 63k pull refs). Every repo this
	// deployment creates is public.
	if _, err := s.git.CreateEmptyRepo(ctx, org, repo, m.GetDescription()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err).Error())
	}
	// From here a failure must not leave an empty orphan behind.
	cleanup := func(cause error) error {
		if derr := s.git.DeleteRepo(context.WithoutCancel(ctx), org, repo); derr != nil {
			log.Printf("warn: cleanup half-imported %s/%s: %v", org, repo, derr)
		}
		return connect.NewError(connect.CodeInternal, cause.Error()).WithCause(cause)
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
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if branch == roles.MainBranch {
		if err := s.git.ProtectMain(ctx, org, repo); err != nil {
			log.Printf("warn: protect main %s/%s: %v", org, repo, err)
		}
	}
	if !s.sessionExists(ctx, hdrFrom(ctx), roles.SessionName(org, repo, branch)) {
		if err := s.createSession(ctx, hdrFrom(ctx), roles.SessionName(org, repo, branch), roles.PresetFor(roles.RoleForBranch(branch)), "", ""); err != nil {
			log.Printf("warn: ensure imported session %s/%s:%s: %v", org, repo, branch, err)
		}
	}
	return &wsv1.ImportRepoResponse{Repo: &wsv1.RepoInfo{
		Org: org, Repo: repo, DefaultBranch: branch, Private: false,
	}}, nil
}

// ---- push mirrors (admin) ----

// SetPushMirror registers a push mirror on a repo the caller's tenant owns.
// HTTPS only. A repo may hold multiple mirrors (Forgejo assigns each a
// remote_name, returned to the caller). Admin action.
func (s *Service) SetPushMirror(ctx context.Context, req *wsv1.SetPushMirrorRequest) (*wsv1.SetPushMirrorResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	addr := strings.TrimSpace(m.GetRemoteAddress())
	if !validPushMirrorAddress(addr) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "remote_address must be an http(s) git URL")
	}
	if iv := strings.TrimSpace(m.GetInterval()); iv != "" && !validInterval(iv) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "interval must be a duration like 8h or 30m")
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
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.SetPushMirrorResponse{Mirror: toPushMirrorInfo(mirror)}, nil
}

// ListPushMirrors lists the push mirrors of a repo the caller's tenant owns.
func (s *Service) ListPushMirrors(ctx context.Context, req *wsv1.ListPushMirrorsRequest) (*wsv1.ListPushMirrorsResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mirrors, err := s.git.ListPushMirrors(ctx, m.GetOrg(), m.GetRepo())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.PushMirrorInfo, 0, len(mirrors))
	for _, pm := range mirrors {
		out = append(out, toPushMirrorInfo(pm))
	}
	return &wsv1.ListPushMirrorsResponse{Mirrors: out}, nil
}

// DeletePushMirror removes a push mirror (by remote_name) from a repo the
// caller's tenant owns. Admin action.
func (s *Service) DeletePushMirror(ctx context.Context, req *wsv1.DeletePushMirrorRequest) (*wsv1.DeletePushMirrorResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(m.GetRemoteName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "remote_name is required")
	}
	if err := s.git.DeletePushMirror(ctx, m.GetOrg(), m.GetRepo(), name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeletePushMirrorResponse{Ok: true}, nil
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

func (s *Service) ListMRs(ctx context.Context, req *wsv1.ListMRsRequest) (*wsv1.ListMRsResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mrs, err := s.git.ListMRs(ctx, m.GetOrg(), m.GetRepo(), m.GetState())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.MRInfo, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, toMRInfo(mr))
	}
	return &wsv1.ListMRsResponse{Mrs: out}, nil
}

// SubmitMR is the ONLY way branch content changes. It materializes the caller's
// sandbox diff (computed by the extension) onto a NEW, immutable `mr/...` head
// branch and opens an MR into `base`. No tool/RPC can write an existing branch.
//
// The head branch name is chosen by the gateway (never the caller), is written
// exactly once (a plain, non-force push), and is deleted on merge/close.
func (s *Service) SubmitMR(ctx context.Context, req *wsv1.SubmitMRRequest) (*wsv1.SubmitMRResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	base := m.GetBase()
	if base == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "base is required")
	}
	if !roles.ValidComponent(base) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "base must be a simple branch name")
	}
	if isMRBranch(base) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "base cannot be an mr/... branch")
	}
	if ok, berr := s.git.BranchExists(ctx, m.GetOrg(), m.GetRepo(), base); berr != nil {
		return nil, connect.NewError(connect.CodeInternal, berr.Error()).WithCause(berr)
	} else if !ok {
		return nil, connect.Errorf(connect.CodeNotFound, "base branch %q does not exist", base)
	}
	if len(m.GetFiles()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "files is required")
	}
	ops := make([]gitcommit.FileOp, 0, len(m.GetFiles()))
	for _, f := range m.GetFiles() {
		content := f.GetContentBytes()
		if len(content) == 0 {
			content = []byte(f.GetContent())
		}
		ops = append(ops, gitcommit.FileOp{Path: f.GetPath(), Op: f.GetOperation(), Content: content})
	}

	caller := sessionFromHeaders(hdrFrom(ctx))
	origin := caller
	if origin == "" {
		origin = roles.SessionName(m.GetOrg(), m.GetRepo(), base)
	}
	head := mrBranchName(origin)
	title := m.GetTitle()
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("MR from %s into %s", origin, base)
	}

	// The `mr/...` branch is created from `base` and written exactly once.
	opts := gitcommit.Options{
		RepoURL: s.git.GitURL(m.GetOrg(), m.GetRepo()),
		Branch:  base,
		User:    "root",
		Token:   s.git.Token(),
	}
	if _, err := s.commits.Submit(ctx, opts, base, head, title, ops); err != nil {
		if errors.Is(err, gitcommit.ErrNoChanges) || errors.Is(err, gitcommit.ErrIgnoredPath) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	index, url, err := s.git.CreateMR(ctx, m.GetOrg(), m.GetRepo(), title, head, base, m.GetBody())
	if err != nil {
		// Best-effort cleanup: a failed MR must not strand the mr/ branch.
		_ = s.git.DeleteBranch(ctx, m.GetOrg(), m.GetRepo(), head)
		return nil, mrError(err)
	}
	// Record the origin session so review notifications can find the submitter.
	if err := s.members.AddMRSubmission(s.tenantOf(ctx, hdrFrom(ctx)), m.GetOrg(), m.GetRepo(), index, base, head, origin); err != nil {
		log.Printf("warn: record MR submission %s/%s#%d: %v", m.GetOrg(), m.GetRepo(), index, err)
	}
	// Wake the TARGET branch's session so the reviewer learns about the MR
	// (best-effort; mirrors the extension's repo-mr-create notification).
	s.notifyMRSubmitted(ctx, hdrFrom(ctx), s.tenantOf(ctx, hdrFrom(ctx)), m.GetOrg(), m.GetRepo(), base, index, title, url)
	return &wsv1.SubmitMRResponse{Index: index, Url: url, Head: head}, nil
}

// isMRBranch reports whether `ref` is a gateway-managed MR head branch.
func isMRBranch(ref string) bool { return strings.HasPrefix(ref, "mr/") }

// mrBranchName builds a fresh, non-colliding MR head branch for an origin
// session. The `mr/` prefix is reserved (never a user branch, never a base).
func mrBranchName(origin string) string {
	slug := strings.NewReplacer(":", "-", "/", "-").Replace(origin)
	return fmt.Sprintf("mr/%s-%d", slug, time.Now().UnixNano())
}

func (s *Service) CommentMR(ctx context.Context, req *wsv1.CommentMRRequest) (*wsv1.CommentMRResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.git.CommentMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex(), m.GetBody()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.CommentMRResponse{Ok: true}, nil
}

// MergeMR merges an MR. It is allowed ONLY when the caller's session equals
// `org:repo:<base>` (self-merge of an MR targeting its own branch); a human
// webui caller (no session header) is unrestricted. The `mr/...` head branch is
// deleted by Forgejo on merge.
func (s *Service) MergeMR(ctx context.Context, req *wsv1.MergeMRRequest) (*wsv1.MergeMRResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mr, err := s.git.GetMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, mrError(err)
	}
	if err := s.authorizeMRTarget(hdrFrom(ctx), m.GetOrg(), m.GetRepo(), mr.Base); err != nil {
		return nil, err
	}
	if !mr.Mergeable {
		return nil, connect.Errorf(connect.CodeFailedPrecondition,
			"change request #%d is not mergeable (the base has diverged)", m.GetIndex())
	}
	if err := s.git.MergeMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex(), ""); err != nil {
		return nil, mrError(err)
	}
	// Drop the submission record; the head branch is gone (merged).
	_ = s.members.DeleteMRSubmission(s.tenantOf(ctx, hdrFrom(ctx)), m.GetOrg(), m.GetRepo(), m.GetIndex())
	return &wsv1.MergeMRResponse{Ok: true}, nil
}

// CloseMR closes an MR (without merging) and deletes its `mr/...` head branch.
// Same authorization as MergeMR.
func (s *Service) CloseMR(ctx context.Context, req *wsv1.CloseMRRequest) (*wsv1.CloseMRResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	mr, err := s.git.GetMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex())
	if err != nil {
		return nil, mrError(err)
	}
	if err := s.authorizeMRTarget(hdrFrom(ctx), m.GetOrg(), m.GetRepo(), mr.Base); err != nil {
		return nil, err
	}
	if err := s.git.CloseMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex()); err != nil {
		return nil, mrError(err)
	}
	if isMRBranch(mr.Head) {
		_ = s.git.DeleteBranch(ctx, m.GetOrg(), m.GetRepo(), mr.Head)
	}
	_ = s.members.DeleteMRSubmission(s.tenantOf(ctx, hdrFrom(ctx)), m.GetOrg(), m.GetRepo(), m.GetIndex())
	return &wsv1.CloseMRResponse{Ok: true}, nil
}

// authorizeMRTarget enforces "an agent session may only merge/close an MR whose
// base is its OWN branch". A human webui caller (no session header) is
// unrestricted; a non-branch (free) session has no repo binding to compare, so
// it is also refused for agent callers.
func (s *Service) authorizeMRTarget(hdr *connect.Header, org, repo, base string) error {
	caller := sessionFromHeaders(hdr)
	if caller == "" {
		return nil // human webui console
	}
	want := roles.SessionName(org, repo, base)
	if caller != want {
		return connect.Errorf(connect.CodePermissionDenied,
			"only the %s session may merge or close this change request", want)
	}
	return nil
}

// tenantOf resolves the caller's tenant for a record write (best-effort: ""
// on failure, which the store tolerates).
func (s *Service) tenantOf(ctx context.Context, hdr *connect.Header) string {
	t, err := s.resolveTenant(ctx, hdr)
	if err != nil {
		return ""
	}
	return t
}

// mrError maps a Forgejo error to a connect error, surfacing conflicts as
// FailedPrecondition instead of an opaque Internal.
func mrError(err error) error {
	var conflict *forgejo.ErrConflict
	if errors.As(err, &conflict) {
		return connect.Errorf(connect.CodeFailedPrecondition, "merge conflict: %s", conflict.Reason)
	}
	var notFound *forgejo.ErrNotFound
	if errors.As(err, &notFound) {
		return connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
}

// ---- sandboxes ----
//
// The gateway owns the Kubernetes lifecycle IN-PROCESS (the worker-manager
// service was folded in): it creates Pod + Service + Secret in the managed
// namespace. Visibility is creator-scoped; the token is only ever returned by
// ResolveSandbox to the owning tenant.

func (s *Service) ListSandboxes(ctx context.Context, req *wsv1.ListSandboxesRequest) (*wsv1.ListSandboxesResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	out, err := s.listSandboxes(ctx, tenant, req.GetSession(), hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	return &wsv1.ListSandboxesResponse{Sandboxes: out}, nil
}

// listSandboxes returns the tenant's visible sandboxes (newest-first). A
// caller with a session header is confined to its OWN sandboxes; `want` is an
// optional explicit session filter for a webui caller.
func (s *Service) listSandboxes(ctx context.Context, tenant, want string, hdr *connect.Header) ([]*wsv1.SandboxInfo, error) {
	sbxs, err := s.sbx.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.SandboxInfo, 0, len(sbxs))
	// An agent session may only enumerate ITS OWN sandboxes; a webui (tenant
	// console, no session header) may enumerate all of the tenant's (with the
	// optional `session` filter still honoured).
	if caller := sessionFromHeaders(hdr); caller != "" {
		want = caller
	}
	caller := sessionFromHeaders(hdr)
	for _, sb := range sbxs {
		if sb.Creator != "" && sb.Creator != tenant {
			continue
		}
		if want != "" && sb.Session != want {
			continue
		}
		info := toSandboxInfo(sb)
		info.Operable = sandboxAccessible(sb, tenant, caller)
		out = append(out, info)
	}
	sortByCreatedAtDesc(out)
	return out, nil
}

func (s *Service) CreateSandbox(ctx context.Context, req *wsv1.CreateSandboxRequest) (*wsv1.CreateSandboxResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	name := req.GetName()
	if !roles.ValidComponent(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name must be simple")
	}
	// Sandboxes run a PRE-BUILT, worker-bundled image from the deployment's
	// dedicated sandbox org. The gateway no longer injects the worker at launch,
	// so an image outside that org would have no worker and could never become
	// ready. Empty = the configured default sandbox image.
	image := req.GetImage()
	// Resolve the OS: for windows/macos pick the VM image, force kvm, set an 8Gi
	// limit, inject GOLDEN_DISK_URL and mount the shared golden-disk cache PVC.
	osName := normalizeOS(req.GetOs())
	// android is a device sandbox with its guest baked into the image; macos/
	// windows are VM sandboxes booting from a golden disk.
	vm := osName == "macos" || osName == "windows"
	device := osName == "android"
	kvm := req.GetKvm()
	memory := req.GetMemory()
	extraEnv := map[string]string{}
	goldenPVC := ""
	if vm {
		if _, ok := vmImageName[osName]; !ok {
			return nil, connect.Errorf(connect.CodeInvalidArgument,
				"unsupported os %q (want linux|windows|macos|android)", req.GetOs())
		}
		if image == "" {
			image = s.vmSandboxImage(osName)
		}
		disk := strings.TrimSpace(req.GetDisk())
		if disk == "" {
			disk = vmDefaultDisk[osName]
		}
		extraEnv["GOLDEN_DISK_URL"] = disk
		// The VM guest has no direct egress; hand it the cluster proxy so the
		// worker persists it (/run/shm/proxy) and exports HTTP(S)_PROXY to its
		// jobs (guest tools like winget/brew/apt reach the network through it).
		if s.vmProxy != "" {
			extraEnv["SANDBOX_PROXY"] = s.vmProxy
		}
		kvm = true
		if memory == "" {
			memory = "8Gi"
		}
		goldenPVC = s.goldenDiskCachePVC
		if goldenPVC == "" {
			goldenPVC = defaultGoldenDiskCachePVC
		}
		if err := s.ensureGoldenDiskCache(ctx, goldenPVC); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
	} else if device {
		// Android: a device sandbox boots an emulator — kvm +
		// extra memory, but no golden disk (the image owns its guest).
		if image == "" {
			image = s.vmSandboxImage(osName)
		}
		kvm = true
		if memory == "" {
			memory = "8Gi"
		}
	} else {
		if image == "" {
			image = s.defaultSandboxImage
		}
	}
	if image == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "no sandbox image given and no default configured")
	}
	if err := s.validateSandboxImage(image); err != nil {
		return nil, err
	}
	// Session binding: an agent session OWNS the sandbox it creates — the
	// binding comes from the caller's session header, not the (spoofable) body,
	// so a session can never create a sandbox under another session's name. A
	// webui caller (no session header) may pass an explicit `session`.
	bindSession := req.GetSession()
	if caller := sessionFromHeaders(hdrFrom(ctx)); caller != "" {
		bindSession = caller
	}
	// A sandbox name is never reused, not even by the SAME session: creating
	// over a live sandbox would silently destroy its workload and token. Refuse
	// early (the manager's ErrExists is the race-safe backstop). This also
	// refuses a name another session/tenant already occupies.
	if _, ok, gerr := s.sbx.Get(ctx, name); gerr != nil {
		return nil, connect.NewError(connect.CodeInternal, gerr.Error()).WithCause(gerr)
	} else if ok {
		return nil, connect.Errorf(connect.CodeAlreadyExists,
			"sandbox %q already exists; delete it before reusing the name", name)
	}
	vols := make([]sandboxmgr.VolumeMount, 0, len(req.GetVolumes()))
	// Reuse the SAME authorization the service path uses (resolveVolumes): every
	// requested PVC must exist and belong to the caller's tenant/org. Without
	// this a session could mount (and write) another tenant's volume, which the
	// kubelet honors regardless of the gateway's ownership model.
	svcVols, err := s.resolveVolumes(ctx, tenant, req.GetVolumes())
	if err != nil {
		return nil, err
	}
	for _, v := range svcVols {
		vols = append(vols, sandboxmgr.VolumeMount{PVC: v.PVC, MountPath: v.MountPath, ReadOnly: v.ReadOnly, SubPath: v.SubPath})
	}
	sb, _, err := s.sbx.Create(ctx, sandboxmgr.Spec{
		Name: name, Image: image, CPU: req.GetCpu(), Memory: memory,
		Env: s.sandboxEnv(withEnv(req.GetEnv(), extraEnv)), Creator: tenant, Session: bindSession,
		Runtime:       s.renderRuntime(kvm, req.GetGpuCount()),
		Bootstrap:     s.bootstrap,
		GoldenDiskPVC: goldenPVC,
		Volumes:       vols,
	})
	if err != nil {
		if errors.Is(err, sandboxmgr.ErrExists) {
			return nil, connect.Errorf(connect.CodeAlreadyExists,
				"sandbox %q already exists; delete it before reusing the name", name)
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	// Wait up to 60s for the worker to accept connections; on timeout the
	// sandbox is left in place (the caller may status/delete it).
	if err := s.sbx.WaitReady(ctx, name, 60*time.Second); err != nil {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
	}
	if cur, ok, gerr := s.sbx.Get(ctx, name); gerr == nil && ok {
		sb = cur
	}
	return &wsv1.CreateSandboxResponse{Sandbox: toSandboxInfo(sb)}, nil
}

// sandboxAccessible reports whether the caller may act on `sb`.
//
// Tenant ownership is the first gate: a sandbox created by another tenant is
// invisible. The SECOND gate is SESSION isolation: an agent session (which
// identifies itself via `X-Session-Name`) may only touch sandboxes bound to
// ITS OWN session — a feature-branch session must never drive/read/delete a
// sandbox another session (e.g. the main session) created. A human/webui caller
// (tenant token, no session header) is the tenant console and may see/manage
// every sandbox the tenant owns.
func sandboxAccessible(sb sandboxmgr.Sandbox, tenant, callerSession string) bool {
	if sb.Creator != "" && sb.Creator != tenant {
		return false
	}
	if callerSession != "" && sb.Session != callerSession {
		return false
	}
	return true
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
// deployment's dedicated sandbox org AND (when configured) the dedicated
// sandbox image registry (artifact). The image may be a full ref
// (`<registry>/<org>/<name>:<tag>`) or a bare name; the FIRST path segment after
// an optional registry host must equal the sandbox org, and the registry host —
// when one is given — must equal sandboxImageRegistryHost. The registry host is
// matched loosely (with or without scheme) because callers may echo back either
// form.
func (s *Service) validateSandboxImage(image string) error {
	if s.sandboxOrg == "" && s.sandboxImageRegistryHost == "" {
		// Neither guard configured: accept as-is (the deployment opted out).
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
	gotHost := ""
	if len(seg) == 2 && (strings.ContainsAny(seg[0], ".:") || seg[0] == "localhost") {
		gotHost = seg[0]
		rest = seg[1]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[:i]
	}
	// Drop a tag only when it is in the LAST path segment (not an org:port).
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		rest = rest[:i]
	}
	// Registry-host guard: a sandbox image must come from the artifact registry
	// when configured. A bare `<org>/<name>` (no host) is refused, so a sandbox
	// can never silently pull from Docker Hub / a public mirror.
	if s.sandboxImageRegistryHost != "" && gotHost != s.sandboxImageRegistryHost {
		return connect.Errorf(connect.CodePermissionDenied,
			"sandbox image must come from the %q registry", s.sandboxImageRegistryHost)
	}
	if s.sandboxOrg != "" {
		org := strings.SplitN(rest, "/", 2)[0]
		if org != s.sandboxOrg {
			return connect.Errorf(connect.CodePermissionDenied,
				"sandbox image must come from the %q org", s.sandboxOrg)
		}
	}
	return nil
}

func (s *Service) GetSandbox(ctx context.Context, req *wsv1.GetSandboxRequest) (*wsv1.GetSandboxResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	sb, ok, err := s.sbx.Get(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	if !sandboxAccessible(sb, tenant, sessionFromHeaders(hdrFrom(ctx))) {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	info := toSandboxInfo(sb)
	info.Operable = true
	// Best-effort worker environment (workspace root + home) so the webui can
	// anchor relative paths and show `~`. Never fails the request.
	if url, token, rerr := s.sbx.Resolve(ctx, sb.Name); rerr == nil {
		if wi, ierr := workerclient.New(url, token).Info(ctx); ierr == nil {
			info.Workspace, info.Home, info.Os, info.Arch = wi.Workspace, wi.Home, wi.OS, wi.Arch
		}
	}
	return &wsv1.GetSandboxResponse{Sandbox: info}, nil
}

func (s *Service) DeleteSandbox(ctx context.Context, req *wsv1.DeleteSandboxRequest) (*wsv1.DeleteSandboxResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	sb, ok, err := s.sbx.Get(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	if !sandboxAccessible(sb, tenant, sessionFromHeaders(hdrFrom(ctx))) {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	deleted, err := s.sbx.Delete(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeleteSandboxResponse{Ok: deleted}, nil
}

// ResolveSandbox returns the worker's url + bearer token to the OWNING tenant,
// so an execution tool can talk to the worker directly.
func (s *Service) ResolveSandbox(ctx context.Context, req *wsv1.ResolveSandboxRequest) (*wsv1.ResolveSandboxResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	sb, ok, err := s.sbx.Get(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	if !sandboxAccessible(sb, tenant, sessionFromHeaders(hdrFrom(ctx))) {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	url, token, err := s.sbx.Resolve(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &wsv1.ResolveSandboxResponse{
		Name: req.GetName(), Url: url, Token: token,
		Phase: sb.Phase, Message: sb.Message,
	}, nil
}

// SandboxLogs reads a sandbox pod's container log (current or previous
// instance), plus its live phase + failure reason. Read-only observability for
// the webui; NOT exposed as an agent tool.
func (s *Service) SandboxLogs(ctx context.Context, req *wsv1.SandboxLogsRequest) (*wsv1.SandboxLogsResponse, error) {
	if _, _, err := s.ownedSandbox(ctx, hdrFrom(ctx), req.GetName()); err != nil {
		return nil, err
	}
	tail := req.GetTailLines()
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	lines, phase, restarts, message, err := s.sbx.TailLogs(ctx, req.GetName(), sandboxmgr.LogOptions{
		TailLines: tail, Previous: req.GetPrevious(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.SandboxLogsResponse{
		Lines: lines, Phase: phase, Restarts: restarts, Message: message,
		Available: len(lines) > 0,
	}, nil
}

// ---- sandbox jobs (read-only observability) ----

// ownedSandbox resolves the caller + the named sandbox, enforcing tenant
// ownership. Returns the worker endpoint (url, token).
func (s *Service) ownedSandbox(ctx context.Context, hdr *connect.Header, name string) (string, string, error) {
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		return "", "", err
	}
	sb, ok, err := s.sbx.Get(ctx, name)
	if err != nil {
		return "", "", connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return "", "", connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	if !sandboxAccessible(sb, tenant, sessionFromHeaders(hdr)) {
		return "", "", connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	url, token, err := s.sbx.Resolve(ctx, name)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return url, token, nil
}

// ListSandboxJobs returns a sandbox's worker job history.
func (s *Service) ListSandboxJobs(ctx context.Context, req *wsv1.ListSandboxJobsRequest) (*wsv1.ListSandboxJobsResponse, error) {
	url, token, err := s.ownedSandbox(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return nil, err
	}
	jobs, err := workerclient.New(url, token).ListJobs(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.SandboxJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, &wsv1.SandboxJob{
			Id: j.ID, Command: j.Command, State: j.State, ExitCode: j.ExitCode,
			StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		})
	}
	return &wsv1.ListSandboxJobsResponse{Jobs: out}, nil
}

// GetSandboxJobOutput polls a bounded window of a job's buffered output.
func (s *Service) GetSandboxJobOutput(ctx context.Context, req *wsv1.GetSandboxJobOutputRequest) (*wsv1.GetSandboxJobOutputResponse, error) {
	url, token, err := s.ownedSandbox(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return nil, err
	}
	m := req
	out, err := workerclient.New(url, token).JobOutput(ctx, m.GetJobId(), m.GetStart(), m.GetEnd(), m.GetStream())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.GetSandboxJobOutputResponse{
		Lines: out.Lines, TotalLines: out.TotalLines,
		StartLine: out.StartLine, EndLine: out.EndLine, Done: out.Done,
	}, nil
}

// WatchSandboxJob streams a job's output (history then live) until it ends.
func (s *Service) WatchSandboxJob(ctx context.Context, req *wsv1.WatchSandboxJobRequest, st wsv1connect.BranchSessionServiceWatchSandboxJobServerStream) error {
	url, token, err := s.ownedSandbox(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return err
	}
	events, errc := workerclient.New(url, token).WatchJob(ctx, req.GetJobId())
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
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return nil
}

// ListSandboxFiles lists a directory (or a single file) in the sandbox. The
// path is passed to the worker VERBATIM: a relative path resolves against the
// worker's workspace, an absolute path is used as-is.
func (s *Service) ListSandboxFiles(ctx context.Context, req *wsv1.ListSandboxFilesRequest) (*wsv1.ListSandboxFilesResponse, error) {
	url, token, err := s.ownedSandbox(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return nil, err
	}
	m := req
	fl, err := workerclient.New(url, token).FileList(ctx, m.GetPath(), m.GetDepth(), m.GetLimit())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.SandboxFileEntry, 0, len(fl.Files))
	for _, f := range fl.Files {
		out = append(out, &wsv1.SandboxFileEntry{Path: f.Path, Size: f.Size, IsDir: f.IsDir})
	}
	return &wsv1.ListSandboxFilesResponse{IsDir: fl.IsDir, Files: out}, nil
}

// ReadSandboxFile reads a (windowed) file from the sandbox.
func (s *Service) ReadSandboxFile(ctx context.Context, req *wsv1.ReadSandboxFileRequest) (*wsv1.ReadSandboxFileResponse, error) {
	url, token, err := s.ownedSandbox(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return nil, err
	}
	m := req
	fr, err := workerclient.New(url, token).FileRead(ctx, m.GetPath(), m.GetStartLine(), m.GetEndLine())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.ReadSandboxFileResponse{
		Content: fr.Content, TotalLines: fr.TotalLines,
		StartLine: fr.StartLine, EndLine: fr.EndLine,
	}, nil
}

// ---- services (long-lived Deployments) ----

// DeployService creates/updates a long-lived Deployment + Service from a user
// image (no worker injection, no sidecar). A bounded YAML manifest, when
// given, overrides the scalar fields.
func (s *Service) DeployService(ctx context.Context, req *wsv1.DeployServiceRequest) (*wsv1.DeployServiceResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	m := req
	name := m.GetName()
	if name == "" {
		name = servicesmgr.ServiceName(sessionFromHeaders(hdrFrom(ctx)))
	}
	// A service is published publicly as `<name>.<ns>.<domain>`, so its name
	// must be a DNS-1123 label (lowercase letters/digits/'-').
	if !roles.ValidServiceName(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name must be a DNS-1123 label (lowercase letters, digits, '-')")
	}
	// Ownership guard: only the creator may update an existing service.
	if existing, gerr := s.services.Get(ctx, name); gerr == nil {
		if !writable(existing.Creator, tenant) {
			return nil, connect.NewError(connect.CodePermissionDenied, "service not writable by this tenant")
		}
		if !canWrite(existing.Namespace, hdrFrom(ctx)) {
			return nil, connect.NewError(connect.CodePermissionDenied, "service belongs to another namespace")
		}
	}

	// Resolve the exposed ports. Empty `services` = the default single public
	// port (tcp80 -> container_port). Entries sharing a suffix form ONE Service.
	ports, err := servicePorts(m.GetServices(), m.GetContainerPort())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
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
		Session:        sessionFromHeaders(hdrFrom(ctx)),
		Namespace:      resolveNamespace(hdrFrom(ctx), m.GetNamespace()),
		Ports:          ports,
		Runtime:        s.renderRuntime(m.GetKvm(), m.GetGpuCount()),
		Volumes:        vols,
		Resources:      toResources(m.GetResources()),
		ReadinessProbe: toProbeSpec(m.GetReadinessProbe()),
		LivenessProbe:  toProbeSpec(m.GetLivenessProbe()),
		StartupProbe:   toProbeSpec(m.GetStartupProbe()),
		Rollout:        toRollout(m.GetRollout()),
		EnvRefs:        toEnvRefs(m.GetEnvRefs()),
		EnvFrom:        toEnvFrom(m.GetEnvFrom()),
		ConfigMounts:   toConfigMounts(m.GetConfigMounts()),
		Sidecars:       toSidecars(m.GetSidecars()),
		NodeSelector:   m.GetNodeSelector(),
		Tolerations:    toTolerations(m.GetTolerations()),
	}
	if spec.Image == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "image is required")
	}
	svc, err := s.services.Deploy(ctx, spec)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	// Bounded readiness wait: surface a deterministic start failure (bad
	// command, image pull error, unschedulable pod) right away instead of
	// returning a Pending shell the caller cannot distinguish from success.
	if err := s.awaitServiceReady(ctx, name); err != nil {
		return nil, err
	}
	// Re-read so the response reflects the settled pod state.
	if cur, gerr := s.services.Get(ctx, name); gerr == nil {
		svc = cur
	}
	return &wsv1.DeployServiceResponse{Service: s.toServiceInfo(svc, hdrFrom(ctx))}, nil
}

// awaitServiceReady waits up to serviceReadyTimeout for a just-deployed service
// to become ready. Returns a FailedPrecondition error on a deterministic
// failure; on timeout it returns nil (the service is left running; the response
// view carries the observed phase/message).
func (s *Service) awaitServiceReady(ctx context.Context, name string) error {
	res, err := s.services.WaitReady(ctx, name, serviceReadyTimeout)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if res.Failed {
		return connect.Errorf(connect.CodeFailedPrecondition,
			"service %q failed to start: %s", name, res.Reason)
	}
	return nil
}

// RollbackService rolls a service's Deployment back to a prior revision.
func (s *Service) RollbackService(ctx context.Context, req *wsv1.RollbackServiceRequest) (*wsv1.RollbackServiceResponse, error) {
	cur, err := s.ownedService(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return nil, err
	}
	if !canWrite(cur.Namespace, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodePermissionDenied, "service belongs to another namespace")
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	svc, err := s.services.Rollback(ctx, req.GetName(), req.GetRevision())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &wsv1.RollbackServiceResponse{Service: s.toServiceInfo(svc, hdrFrom(ctx))}, nil
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
			return nil, connect.Errorf(connect.CodeInvalidArgument, "volumes[%d]: pvc must be a DNS-1123 label", i)
		}
		if m.GetMountPath() == "" || !strings.HasPrefix(m.GetMountPath(), "/") {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "volumes[%d]: mount_path must be an absolute path", i)
		}
		pvc, err := s.services.GetPVC(ctx, m.GetPvc())
		if err != nil {
			return nil, connect.Errorf(connect.CodeNotFound, "volumes[%d]: pvc %q not found", i, m.GetPvc())
		}
		if pvc.Creator != "" && pvc.Creator != tenant {
			return nil, connect.Errorf(connect.CodeNotFound, "volumes[%d]: pvc %q not found", i, m.GetPvc())
		}
		out = append(out, servicesmgr.VolumeMount{
			PVC: pvc.Name, MountPath: m.GetMountPath(),
			ReadOnly: m.GetReadOnly(), SubPath: m.GetSubPath(),
		})
	}
	return out, nil
}

// ---- Helm ----

func toHelmReleaseInfo(r helmmgr.Release) *wsv1.HelmReleaseInfo {
	info := &wsv1.HelmReleaseInfo{
		Name: r.Name, Namespace: r.Namespace, Creator: r.Creator, Session: r.Session,
		Ref: r.Ref, ChartPath: r.ChartPath, Revision: int32(r.Revision),
		Status: r.Status, UpdatedAt: r.UpdatedAt,
		ChartVersion: r.ChartVersion, AppVersion: r.AppVersion,
		OrgNamespace: r.OrgNS,
	}
	// The current revision's objects ("Kind/name"). The head denormalizes them
	// for the list view; fall back to the materialized history (e.g. a legacy
	// head written before Objects was denormalized, as in HelmHistory/Get).
	objs := r.Objects
	if len(objs) == 0 && len(r.History) > 0 {
		objs = r.History[len(r.History)-1].Objects
	}
	for _, o := range objs {
		info.Objects = append(info.Objects, o.Kind+"/"+o.Name)
	}
	return info
}

// helmChartDir extracts the repo archive and resolves the chart directory,
// refusing any path that escapes the extracted root.
func (s *Service) helmChartDir(ctx context.Context, org, repo, ref, chartPath string) (string, func(), error) {
	archive, err := s.git.ArchiveTarGz(ctx, org, repo, ref)
	if err != nil {
		return "", nil, fmt.Errorf("fetch repo archive: %w", err)
	}
	dir, cleanup, err := imagebuild.Extract(archive, 0)
	if err != nil {
		return "", nil, err
	}
	cp := chartPath
	if cp == "" {
		cp = "."
	}
	// Reject absolute / parent-escaping chart paths.
	clean := path.Clean("/" + cp)
	chartDir := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if chartDir != dir && !strings.HasPrefix(chartDir, dir+string(os.PathSeparator)) {
		cleanup()
		return "", nil, fmt.Errorf("chart path escapes the repository")
	}
	return chartDir, cleanup, nil
}

// HelmDeploy renders (and, unless dry_run, applies) a chart as a release.
func (s *Service) HelmDeploy(ctx context.Context, req *wsv1.HelmDeployRequest) (*wsv1.HelmDeployResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.helm == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "helm backend not configured")
	}
	m := req
	org, repo, ref := m.GetOrg(), m.GetRepo(), m.GetRef()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org/repo must be simple names")
	}
	if ref == "" {
		ref = roles.MainBranch
	}
	if !roles.ValidComponent(ref) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "ref must be a simple name")
	}
	if !helmReleaseRe.MatchString(m.GetRelease()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "release must be a DNS-1123 label")
	}
	// HelmDeploy UPSERTS by name: refuse to overwrite an existing release owned
	// by another tenant (cross-tenant guard), and require namespace write access.
	if cur, ok, gerr := s.helm.Get(ctx, m.GetRelease()); gerr != nil {
		return nil, connect.NewError(connect.CodeInternal, gerr.Error()).WithCause(gerr)
	} else if ok {
		if !writable(cur.Creator, tenant) {
			return nil, connect.NewError(connect.CodePermissionDenied, "release not writable by this tenant")
		}
		if !canWrite(cur.OrgNS, hdrFrom(ctx)) {
			return nil, connect.NewError(connect.CodePermissionDenied, "release belongs to another namespace")
		}
	}
	chartDir, cleanup, err := s.helmChartDir(ctx, org, repo, ref, m.GetChartPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	defer cleanup()
	opts := helmmgr.RenderOptions{
		Release: m.GetRelease(), ChartPath: m.GetChartPath(),
		ValuesYAML: m.GetValues(), Ref: ref,
		OrgNS: resolveNamespace(hdrFrom(ctx), m.GetNamespace()),
	}
	if m.GetDryRun() {
		manifest, objects, _, terr := s.helm.Template(chartDir, opts)
		if terr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, terr.Error()).WithCause(terr)
		}
		names := make([]string, 0, len(objects))
		for _, o := range objects {
			names = append(names, o.Kind+"/"+o.Name)
		}
		return &wsv1.HelmDeployResponse{Manifest: manifest, Objects: names}, nil
	}
	rel, err := s.helm.Apply(ctx, chartDir, opts, tenant, sessionFromHeaders(hdrFrom(ctx)))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.HelmDeployResponse{
		Release: toHelmReleaseInfo(rel),
		Manifest: func() string {
			if len(rel.History) > 0 {
				return rel.History[len(rel.History)-1].Manifest
			}
			return ""
		}(),
	}, nil
}

// HelmList lists the tenant's releases.
func (s *Service) HelmList(ctx context.Context, req *wsv1.HelmListRequest) (*wsv1.HelmListResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	out, err := s.listHelmReleases(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return &wsv1.HelmListResponse{Releases: out}, nil
}

// listHelmReleases returns the tenant's visible Helm releases
// are folded into their router entry).
func (s *Service) listHelmReleases(ctx context.Context, tenant string) ([]*wsv1.HelmReleaseInfo, error) {
	if s.helm == nil {
		return []*wsv1.HelmReleaseInfo{}, nil
	}
	all, err := s.helm.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.HelmReleaseInfo, 0, len(all))
	for _, r := range all {
		if r.Creator != "" && r.Creator != tenant {
			continue
		}
		info := toHelmReleaseInfo(r)
		info.Operable = writable(r.Creator, tenant) && canWrite(r.OrgNS, hdrFrom(ctx))
		out = append(out, info)
	}
	return out, nil
}

// HelmHistory returns a release and its revisions.
func (s *Service) HelmHistory(ctx context.Context, req *wsv1.HelmHistoryRequest) (*wsv1.HelmHistoryResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.helm == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "helm backend not configured")
	}
	rel, ok, err := s.helm.Get(ctx, req.GetRelease())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok || (rel.Creator != "" && rel.Creator != tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "release not found")
	}
	revs := make([]*wsv1.HelmRevisionInfo, 0, len(rel.History))
	for _, rv := range rel.History {
		objs := make([]string, 0, len(rv.Objects))
		for _, o := range rv.Objects {
			objs = append(objs, o.Kind+"/"+o.Name)
		}
		revs = append(revs, &wsv1.HelmRevisionInfo{
			Revision: int32(rv.Revision), Ref: rv.Ref, ChartPath: rv.ChartPath,
			Values: rv.Values, CreatedAt: rv.CreatedAt, Objects: objs,
		})
	}
	return &wsv1.HelmHistoryResponse{Release: toHelmReleaseInfo(rel), Revisions: revs}, nil
}

// HelmRollback re-applies a prior revision.
func (s *Service) HelmRollback(ctx context.Context, req *wsv1.HelmRollbackRequest) (*wsv1.HelmRollbackResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.helm == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "helm backend not configured")
	}
	cur, ok, err := s.helm.Get(ctx, req.GetRelease())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok || !writable(cur.Creator, tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "release not found")
	}
	if !canWrite(cur.OrgNS, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodeNotFound, "release not found")
	}
	target := int(req.GetRevision())
	if target == 0 {
		target = cur.Revision - 1
	}
	rel, err := s.helm.Rollback(ctx, req.GetRelease(), target)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &wsv1.HelmRollbackResponse{Release: toHelmReleaseInfo(rel)}, nil
}

// HelmUninstall deletes a release and its objects.
func (s *Service) HelmUninstall(ctx context.Context, req *wsv1.HelmUninstallRequest) (*wsv1.HelmUninstallResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.helm == nil {
		return &wsv1.HelmUninstallResponse{Ok: false}, nil
	}
	cur, ok, err := s.helm.Get(ctx, req.GetRelease())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok || !writable(cur.Creator, tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "release not found")
	}
	if !canWrite(cur.OrgNS, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodeNotFound, "release not found")
	}
	ok2, err := s.helm.Uninstall(ctx, req.GetRelease())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.HelmUninstallResponse{Ok: ok2}, nil
}

// ownedHelmRelease verifies the release exists and belongs to the tenant.
func (s *Service) ownedHelmRelease(ctx context.Context, tenant, release string) error {
	rel, ok, err := s.helm.Get(ctx, release)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok || (rel.Creator != "" && rel.Creator != tenant) {
		return connect.NewError(connect.CodeNotFound, "release not found")
	}
	return nil
}

// HelmObjects lists the LIVE status of a release's current-revision objects
// (workloads, pods, services, ...), so a UI/tool can show each one — and why an
// unhealthy pod is failing — without knowing the names up front.
func (s *Service) HelmObjects(ctx context.Context, req *wsv1.HelmObjectsRequest) (*wsv1.HelmObjectsResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.helm == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "helm backend not configured")
	}
	if err := s.ownedHelmRelease(ctx, tenant, req.GetRelease()); err != nil {
		return nil, err
	}
	objs, err := s.helm.Objects(ctx, req.GetRelease())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.HelmObjectInfo, 0, len(objs))
	for _, o := range objs {
		out = append(out, &wsv1.HelmObjectInfo{
			Kind: o.Kind, Name: o.Name, Namespace: o.Namespace,
			Status: o.Status, Ready: o.Ready, Message: o.Message,
			Phase: o.Phase, Restarts: o.Restarts,
			ReadyReplicas: o.ReadyReps, DesiredReplicas: o.DesiredRep,
		})
	}
	return &wsv1.HelmObjectsResponse{Objects: out}, nil
}

// HelmObjectLogs reads one object's container log (kind must be "Pod"),
// gated on the object belonging to the release's current revision.
func (s *Service) HelmObjectLogs(ctx context.Context, req *wsv1.HelmObjectLogsRequest) (*wsv1.HelmObjectLogsResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.helm == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "helm backend not configured")
	}
	if err := s.ownedHelmRelease(ctx, tenant, req.GetRelease()); err != nil {
		return nil, err
	}
	lines, available, msg, err := s.helm.ObjectLogs(ctx, req.GetRelease(), req.GetKind(), req.GetName(),
		helmmgr.LogOptions{TailLines: req.GetTailLines(), Previous: req.GetPrevious(), Container: req.GetContainer()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.HelmObjectLogsResponse{Lines: lines, Available: available, Message: msg}, nil
}

var helmReleaseRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,52}[a-z0-9])?$`)

// ---- Tier 0 conversion helpers (proto -> servicesmgr) ----

func toResources(r *wsv1.ResourceSpec) servicesmgr.Resources {
	if r == nil {
		return servicesmgr.Resources{}
	}
	return servicesmgr.Resources{
		CPURequest: r.GetCpu(), MemoryRequest: r.GetMemory(),
		CPULimit: r.GetCpuLimit(), MemoryLimit: r.GetMemoryLimit(),
	}
}

func toProbeSpec(p *wsv1.ProbeSpec) servicesmgr.Probe {
	if p == nil {
		return servicesmgr.Probe{}
	}
	return servicesmgr.Probe{
		HTTPPath: p.GetHttpPath(), HTTPPort: p.GetHttpPort(), TCPPort: p.GetTcpPort(),
		Exec:                p.GetExecCommand(),
		InitialDelaySeconds: p.GetInitialDelaySeconds(),
		PeriodSeconds:       p.GetPeriodSeconds(),
		TimeoutSeconds:      p.GetTimeoutSeconds(),
		FailureThreshold:    p.GetFailureThreshold(),
		SuccessThreshold:    p.GetSuccessThreshold(),
	}
}

func toRollout(r *wsv1.RolloutSpec) servicesmgr.Rollout {
	if r == nil {
		return servicesmgr.Rollout{}
	}
	return servicesmgr.Rollout{MaxSurge: r.GetMaxSurge(), MaxUnavailable: r.GetMaxUnavailable()}
}

func toEnvRefs(in []*wsv1.EnvRefSpec) []servicesmgr.EnvRef {
	out := make([]servicesmgr.EnvRef, 0, len(in))
	for _, e := range in {
		out = append(out, servicesmgr.EnvRef{
			Name: e.GetName(), ConfigMap: e.GetConfigMap(), ConfigKey: e.GetConfigKey(),
			Secret: e.GetSecret(), SecretKey: e.GetSecretKey(),
		})
	}
	return out
}

func toEnvFrom(in []*wsv1.EnvFromSpec) []servicesmgr.EnvFrom {
	out := make([]servicesmgr.EnvFrom, 0, len(in))
	for _, e := range in {
		out = append(out, servicesmgr.EnvFrom{ConfigMap: e.GetConfigMap(), Secret: e.GetSecret()})
	}
	return out
}

func toConfigMounts(in []*wsv1.ConfigMountSpec) []servicesmgr.ConfigMount {
	out := make([]servicesmgr.ConfigMount, 0, len(in))
	for _, c := range in {
		cm := servicesmgr.ConfigMount{
			ConfigMap: c.GetConfigMap(), Secret: c.GetSecret(), MountPath: c.GetMountPath(),
		}
		for _, it := range c.GetItems() {
			cm.Items = append(cm.Items, servicesmgr.KeyToPath{Key: it.GetKey(), Path: it.GetPath()})
		}
		out = append(out, cm)
	}
	return out
}

func toSidecars(in []*wsv1.SidecarSpec) []servicesmgr.Sidecar {
	out := make([]servicesmgr.Sidecar, 0, len(in))
	for _, sc := range in {
		out = append(out, servicesmgr.Sidecar{
			Name: sc.GetName(), Image: sc.GetImage(), Command: sc.GetCommand(),
			Env: sc.GetEnv(), CPU: sc.GetCpu(), Memory: sc.GetMemory(), Init: sc.GetInit(),
		})
	}
	return out
}

func toTolerations(in []*wsv1.TolerationSpec) []servicesmgr.Toleration {
	out := make([]servicesmgr.Toleration, 0, len(in))
	for _, t := range in {
		out = append(out, servicesmgr.Toleration{
			Key: t.GetKey(), Operator: t.GetOperator(), Value: t.GetValue(), Effect: t.GetEffect(),
		})
	}
	return out
}

// ---- persistent volume claims ----

// CreatePVC creates a named, tenant-owned claim with the deployment's storage
// class (self-hosted local-path). Admin action; the tenant owns the claim.
func (s *Service) CreatePVC(ctx context.Context, req *wsv1.CreatePVCRequest) (*wsv1.CreatePVCResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	m := req
	if !roles.ValidServiceName(m.GetName()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name must be a DNS-1123 label (lowercase letters, digits, '-')")
	}
	// Only the deployment's configured class is supported today.
	if sc := m.GetStorageClass(); sc != "" && sc != s.pvcStorageClass {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "storage_class must be %q", s.pvcStorageClass)
	}
	size := m.GetSize()
	if size == "" {
		size = s.pvcDefaultSize
	}
	// A1: CreatePVC is a same-name upsert; refuse to (re)claim an existing claim
	// that is not writable by the caller (unowned included).
	if cur, gerr := s.services.GetPVC(ctx, m.GetName()); gerr == nil {
		if !writable(cur.Creator, tenant) {
			return nil, connect.NewError(connect.CodePermissionDenied, "pvc not writable by this tenant")
		}
	}
	pvc, err := s.services.CreatePVC(ctx, m.GetName(), size, s.pvcStorageClass, tenant, resolveNamespace(hdrFrom(ctx), m.GetNamespace()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.CreatePVCResponse{Pvc: toPVCInfo(pvc)}, nil
}

// ListPVCs lists the tenant's claims.
func (s *Service) ListPVCs(ctx context.Context, req *wsv1.ListPVCsRequest) (*wsv1.ListPVCsResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	out, err := s.listPVCs(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return &wsv1.ListPVCsResponse{Pvcs: out}, nil
}

// listPVCs returns the tenant's visible PersistentVolumeClaims.
func (s *Service) listPVCs(ctx context.Context, tenant string) ([]*wsv1.PVCInfo, error) {
	if s.services == nil {
		return []*wsv1.PVCInfo{}, nil
	}
	pvcs, err := s.services.ListPVCs(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.PVCInfo, 0, len(pvcs))
	for _, p := range pvcs {
		if p.Creator != "" && p.Creator != tenant {
			continue
		}
		info := toPVCInfo(p)
		info.Operable = writable(p.Creator, tenant) && canWrite(p.Namespace, hdrFrom(ctx))
		out = append(out, info)
	}
	return out, nil
}

// DeletePVC removes a claim the tenant owns. Refused while a service mounts it.
func (s *Service) DeletePVC(ctx context.Context, req *wsv1.DeletePVCRequest) (*wsv1.DeletePVCResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return &wsv1.DeletePVCResponse{Ok: false}, nil
	}
	cur, gerr := s.services.GetPVC(ctx, req.GetName())
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, "pvc not found")
	}
	if !writable(cur.Creator, tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "pvc not found")
	}
	if !canWrite(cur.Namespace, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodeNotFound, "pvc not found")
	}
	ok, err := s.services.DeletePVC(ctx, req.GetName())
	if err != nil {
		// Mounted-by is the common refusal: surface it as FailedPrecondition.
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &wsv1.DeletePVCResponse{Ok: ok}, nil
}

func toPVCInfo(p servicesmgr.PVC) *wsv1.PVCInfo {
	return &wsv1.PVCInfo{
		Name: p.Name, Size: p.Size, StorageClass: p.StorageClass, Phase: p.Phase,
		Creator: p.Creator, CreatedAt: p.CreatedAt, MountedBy: p.MountedBy,
		Namespace: p.Namespace,
	}
}

// ListServices lists the tenant's services.
func (s *Service) ListServices(ctx context.Context, req *wsv1.ListServicesRequest) (*wsv1.ListServicesResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	out, err := s.listServices(ctx, tenant, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	return &wsv1.ListServicesResponse{Services: out}, nil
}

// listServices returns the tenant's visible services (newest-first).
func (s *Service) listServices(ctx context.Context, tenant string, hdr *connect.Header) ([]*wsv1.ServiceInfo, error) {
	if s.services == nil {
		return []*wsv1.ServiceInfo{}, nil
	}
	svcs, err := s.services.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.ServiceInfo, 0, len(svcs))
	for _, svc := range svcs {
		if svc.Creator != "" && svc.Creator != tenant {
			continue
		}
		info := s.toServiceInfo(svc, hdr)
		info.Operable = writable(svc.Creator, tenant) && canWrite(svc.Namespace, hdr)
		out = append(out, info)
	}
	sortByCreatedAtDesc(out)
	return out, nil
}

// hasCreatedAt is the shared sort-key contract for both list messages.
type hasCreatedAt interface{ GetCreatedAt() int64 }

// sortByCreatedAtDesc orders items newest-first (stable, so equal timestamps
// keep their incoming order). The k8s API returns pods/deployments in
// name-ish order, not creation order.
func sortByCreatedAtDesc[T hasCreatedAt](items []T) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].GetCreatedAt() > items[j].GetCreatedAt()
	})
}

// DeleteService removes a service the tenant owns.
func (s *Service) DeleteService(ctx context.Context, req *wsv1.DeleteServiceRequest) (*wsv1.DeleteServiceResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return &wsv1.DeleteServiceResponse{Ok: false}, nil
	}
	svc, gerr := s.services.Get(ctx, req.GetName())
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if !writable(svc.Creator, tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if !canWrite(svc.Namespace, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	ok, err := s.services.Delete(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeleteServiceResponse{Ok: ok}, nil
}

// PauseService scales a service to zero replicas without deleting it. The
// replica count is remembered so ResumeService can restore it.
func (s *Service) PauseService(ctx context.Context, req *wsv1.PauseServiceRequest) (*wsv1.PauseServiceResponse, error) {
	svc, err := s.pauseResume(ctx, hdrFrom(ctx), req.GetName(), true)
	if err != nil {
		return nil, err
	}
	return &wsv1.PauseServiceResponse{Service: svc}, nil
}

// ResumeService restores a paused service to its pre-pause replica count.
func (s *Service) ResumeService(ctx context.Context, req *wsv1.ResumeServiceRequest) (*wsv1.ResumeServiceResponse, error) {
	svc, err := s.pauseResume(ctx, hdrFrom(ctx), req.GetName(), false)
	if err != nil {
		return nil, err
	}
	return &wsv1.ResumeServiceResponse{Service: svc}, nil
}

// ScaleService sets a service's desired replica count (0 = scaled down).
func (s *Service) ScaleService(ctx context.Context, req *wsv1.ScaleServiceRequest) (*wsv1.ScaleServiceResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	cur, gerr := s.services.Get(ctx, req.GetName())
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if !writable(cur.Creator, tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if !canWrite(cur.Namespace, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if req.GetReplicas() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "replicas must be >= 0")
	}
	out, err := s.services.Scale(ctx, req.GetName(), req.GetReplicas())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.ScaleServiceResponse{Service: s.toServiceInfo(out, hdrFrom(ctx))}, nil
}

// GetServiceManifest returns the service's Deployment + Services as YAML.
func (s *Service) GetServiceManifest(ctx context.Context, req *wsv1.GetServiceManifestRequest) (*wsv1.GetServiceManifestResponse, error) {
	if _, err := s.ownedService(ctx, hdrFrom(ctx), req.GetName()); err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	y, err := s.services.Manifest(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.GetServiceManifestResponse{Yaml: y}, nil
}

// ApplyServiceManifest replaces a service from an edited multi-document YAML.
func (s *Service) ApplyServiceManifest(ctx context.Context, req *wsv1.ApplyServiceManifestRequest) (*wsv1.ApplyServiceManifestResponse, error) {
	cur, err := s.ownedService(ctx, hdrFrom(ctx), req.GetName())
	if err != nil {
		return nil, err
	}
	if !canWrite(cur.Namespace, hdrFrom(ctx)) {
		return nil, connect.NewError(connect.CodePermissionDenied, "service belongs to another namespace")
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	out, y, err := s.services.ApplyManifest(ctx, req.GetName(), req.GetYaml(), req.GetDryRun())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	resp := &wsv1.ApplyServiceManifestResponse{Yaml: y}
	if !req.GetDryRun() {
		resp.Service = s.toServiceInfo(out, hdrFrom(ctx))
	}
	return resp, nil
}

// pauseResume is the shared authorization + dispatch for Pause/Resume. Pausing
// is only meaningful for a running service; resuming only for a paused one.
func (s *Service) pauseResume(ctx context.Context, hdr *connect.Header, name string, pause bool) (*wsv1.ServiceInfo, error) {
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	cur, gerr := s.services.Get(ctx, name)
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if !writable(cur.Creator, tenant) {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if !canWrite(cur.Namespace, hdr) {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	var out servicesmgr.Service
	if pause {
		out, err = s.services.Pause(ctx, name)
	} else {
		out, err = s.services.Resume(ctx, name)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return s.toServiceInfo(out, hdr), nil
}

// ServiceLogs reads a bounded window of a service's container log.
func (s *Service) ServiceLogs(ctx context.Context, req *wsv1.ServiceLogsRequest) (*wsv1.ServiceLogsResponse, error) {
	if _, err := s.ownedService(ctx, hdrFrom(ctx), req.GetName()); err != nil {
		return nil, err
	}
	tail := req.GetTailLines()
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	lines, err := s.services.LogSource().Tail(ctx, req.GetName(), servicesmgr.LogOptions{
		TailLines: tail, Previous: req.GetPrevious(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	resp := &wsv1.ServiceLogsResponse{Lines: lines, Available: len(lines) > 0}
	// A container that failed BEFORE producing any output (runc create error,
	// image pull failure, crash loop, unschedulable pod) has no stdout/stderr,
	// so the log stream is empty and the ONLY explanation lives in the pod
	// status. Attach it so the caller/model is not left with a bare "no logs".
	if len(lines) == 0 {
		fillPodDiagnostics(ctx, s.services, req.GetName(), &resp.PodPhase, &resp.Restarts, &resp.Message)
	}
	return resp, nil
}

// fillPodDiagnostics reads a service's pod diagnostics (phase / restarts /
// waiting-terminated reason+message) into the given out-params. Best-effort:
// on any error the out-params are left untouched.
func fillPodDiagnostics(ctx context.Context, svc servicesReader, name string, phase *string, restarts *int32, message *string) {
	s, err := svc.Get(ctx, name)
	if err != nil {
		return
	}
	if s.PodPhase != "" {
		*phase = s.PodPhase
	}
	*restarts = s.Restarts
	if s.Message != "" {
		*message = s.Message
	}
}

// servicesReader is the subset of the services client the log diagnostics need
// (a seam for tests).
type servicesReader interface {
	Get(ctx context.Context, name string) (servicesmgr.Service, error)
}

// WatchServiceLogs streams a service's container log until the stream ends or
// the client disconnects.
func (s *Service) WatchServiceLogs(ctx context.Context, req *wsv1.WatchServiceLogsRequest, st wsv1connect.BranchSessionServiceWatchServiceLogsServerStream) error {
	if _, err := s.ownedService(ctx, hdrFrom(ctx), req.GetName()); err != nil {
		return err
	}
	lines, errc := s.services.LogSource().Follow(ctx, req.GetName(), servicesmgr.LogOptions{
		Previous: req.GetPrevious(),
	})
	sent := false
	for l := range lines {
		sent = true
		if err := st.Send(&wsv1.WatchServiceLogsResponse{Output: l + "\n"}); err != nil {
			return err
		}
	}
	// Terminal frame: carry pod diagnostics when the container never produced
	// output, so a failed-to-start container is explained rather than blank.
	done := &wsv1.WatchServiceLogsResponse{Done: true}
	if !sent {
		fillPodDiagnostics(ctx, s.services, req.GetName(), &done.PodPhase, &done.Restarts, &done.Message)
	}
	if err := <-errc; err != nil {
		done.Error = err.Error()
	}
	return st.Send(done)
}

// WatchWorkspace streams the tenant's sandboxes/services/PVCs live: an initial
// full snapshot, then a new frame whenever any underlying k8s object changes.
// The client never polls.
func (s *Service) WatchWorkspace(ctx context.Context, req *wsv1.WatchWorkspaceRequest, st wsv1connect.BranchSessionServiceWatchWorkspaceServerStream) error {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return err
	}
	changes, unsub := s.hub.subscribe()
	defer unsub()

	send := func() error {
		hdr := hdrFrom(ctx)
		sbxs, err := s.listSandboxes(ctx, tenant, "", hdr)
		if err != nil {
			return err
		}
		svcs, err := s.listServices(ctx, tenant, hdr)
		if err != nil {
			return err
		}
		pvcs, err := s.listPVCs(ctx, tenant)
		if err != nil {
			return err
		}
		rels, err := s.listHelmReleases(ctx, tenant)
		if err != nil {
			return err
		}
		return st.Send(&wsv1.WatchWorkspaceResponse{
			Sandboxes: sbxs, Services: svcs, Pvcs: pvcs, Releases: rels,
			SandboxesChanged: true, ServicesChanged: true, PvcsChanged: true,
			ReleasesChanged: true,
		})
	}
	// Initial snapshot.
	if err := send(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-changes:
			if !ok {
				return nil
			}
			if err := send(); err != nil {
				return err
			}
		}
	}
}

// ownedService resolves a service and enforces tenant/creator ownership.
func (s *Service) ownedService(ctx context.Context, hdr *connect.Header, name string) (servicesmgr.Service, error) {
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		return servicesmgr.Service{}, err
	}
	if s.services == nil {
		return servicesmgr.Service{}, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	svc, gerr := s.services.Get(ctx, name)
	if gerr != nil {
		return servicesmgr.Service{}, connect.NewError(connect.CodeNotFound, "service not found")
	}
	if svc.Creator != "" && svc.Creator != tenant {
		return servicesmgr.Service{}, connect.NewError(connect.CodePermissionDenied, "service belongs to another tenant")
	}
	return svc, nil
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
func (s *Service) Blame(ctx context.Context, req *wsv1.BlameRequest) (*wsv1.BlameResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if m.GetPath() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "path is required")
	}
	ref := m.GetRef()
	if ref == "" {
		ref = roles.MainBranch
	}
	lines, err := gitcommit.Blame(ctx, s.git.GitURL(m.GetOrg(), m.GetRepo()), "root", s.git.Token(), ref, m.GetPath(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.BlameLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, &wsv1.BlameLine{
			Line: int32(l.Line), Sha: l.SHA, Author: l.Author,
			AuthorEmail: l.AuthorEmail, Date: l.Date, Content: l.Content,
		})
	}
	return &wsv1.BlameResponse{Lines: out}, nil
}

// FileDiff returns the unified diff of one file between two refs (go-git;
// Forgejo's compare `patch` is empty on 1.22).
func (s *Service) FileDiff(ctx context.Context, req *wsv1.FileDiffRequest) (*wsv1.FileDiffResponse, error) {
	m := req
	if err := s.ensureVisible(ctx, hdrFrom(ctx), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if m.GetPath() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "path is required")
	}
	diff, err := gitcommit.FileDiff(ctx, s.git.GitURL(m.GetOrg(), m.GetRepo()), "root", s.git.Token(), m.GetBase(), m.GetHead(), m.GetPath(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.FileDiffResponse{Diff: diff}, nil
}

// sessionFromHeaders extracts the caller's session name (used to derive a
// default service name). The extension passes it via X-Session-Name.
// callerNamespace returns the tenant-internal namespace of the caller: the org
// segment of its session name (X-Session-Name = org:repo:branch). Empty for a
// webui caller (no session header).
func callerNamespace(hdr *connect.Header) string {
	session := sessionFromHeaders(hdr)
	if session == "" {
		return ""
	}
	if org, _, _, ok := roles.ParseSession(session); ok {
		return org
	}
	return ""
}

// canWrite reports whether the caller may MUTATE a resource in `resourceNS`
// (the caller's tenant already matched). Within the tenant a write additionally
// requires the resource's namespace to match the caller's; a caller with NO
// namespace (webui) and a resource with NO namespace (legacy) are tenant-wide.
// writable reports whether `tenant` may MUTATE an object created by `creator`.
// A1: an UNOWNED object (creator "") is NOT writable by anyone.
func writable(creator, tenant string) bool {
	return creator != "" && creator == tenant
}

// owned is the READ filter: an object is visible to `tenant` unless it belongs
// to a DIFFERENT tenant (unowned is visible to all).
func owned(creator, tenant string) bool {
	return creator == "" || creator == tenant
}

func canWrite(resourceNS string, hdr *connect.Header) bool {
	callerNS := callerNamespace(hdr)
	return callerNS == "" || resourceNS == "" || resourceNS == callerNS
}

// resolveNamespace returns the namespace to RECORD: the caller's session-derived
// namespace (agent), else the explicit request namespace (webui).
func resolveNamespace(hdr *connect.Header, explicit string) string {
	if ns := callerNamespace(hdr); ns != "" {
		return ns
	}
	return explicit
}

func sessionFromHeaders(hdr *connect.Header) string {
	if hdr == nil {
		return ""
	}
	return hdr.Get("X-Session-Name")
}

func (s *Service) toServiceInfo(svc servicesmgr.Service, hdr *connect.Header) *wsv1.ServiceInfo {
	publicURLs := s.servicePublicURLs(svc, hdr)
	return toServiceInfoImpl(svc, publicURLs)
}

func toServiceInfoImpl(svc servicesmgr.Service, publicURLs map[string]string) *wsv1.ServiceInfo {
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
	vols := make([]*wsv1.VolumeMountInfo, 0, len(svc.Volumes))
	for _, v := range svc.Volumes {
		vols = append(vols, &wsv1.VolumeMountInfo{
			Pvc: v.PVC, MountPath: v.MountPath, ReadOnly: v.ReadOnly, SubPath: v.SubPath,
		})
	}
	cms := make([]*wsv1.ConfigMountInfo, 0, len(svc.ConfigMounts))
	for _, c := range svc.ConfigMounts {
		cms = append(cms, &wsv1.ConfigMountInfo{
			ConfigMap: c.ConfigMap, Secret: c.Secret, MountPath: c.MountPath,
		})
	}
	return &wsv1.ServiceInfo{
		Name: svc.Name, Image: svc.Image, Phase: svc.Phase, Ready: svc.Ready,
		Replicas: svc.Replicas, ReadyReplicas: svc.ReadyReplicas,
		Url: svc.URL, Creator: svc.Creator, Session: svc.Session,
		PublicUrl: primary, Ports: ports,
		PodPhase: svc.PodPhase, Restarts: svc.Restarts,
		Message: svc.Message, Paused: svc.Paused,
		CreatedAt: svc.CreatedAt,
		Cpu:       svc.CPU, Memory: svc.Memory, Command: svc.Command, Env: svc.Env,
		Volumes:        vols,
		ConfigMounts:   cms,
		Resources:      fromResources(svc.Resources),
		ReadinessProbe: fromProbeSpec(svc.ReadinessProbe),
		LivenessProbe:  fromProbeSpec(svc.LivenessProbe),
		StartupProbe:   fromProbeSpec(svc.StartupProbe),
		Rollout:        fromRollout(svc.Rollout),
		SidecarCount:   svc.SidecarCount,
		Namespace:      svc.Namespace,
	}
}

// ---- Tier 0 reverse mapping (servicesmgr -> proto) ----

func fromResources(r servicesmgr.Resources) *wsv1.ResourceSpec {
	if r.Empty() {
		return nil
	}
	return &wsv1.ResourceSpec{
		Cpu: r.CPURequest, Memory: r.MemoryRequest,
		CpuLimit: r.CPULimit, MemoryLimit: r.MemoryLimit,
	}
}

func fromProbeSpec(p servicesmgr.Probe) *wsv1.ProbeSpec {
	if p.Empty() {
		return nil
	}
	return &wsv1.ProbeSpec{
		HttpPath: p.HTTPPath, HttpPort: p.HTTPPort, TcpPort: p.TCPPort,
		ExecCommand:         p.Exec,
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		FailureThreshold:    p.FailureThreshold,
		SuccessThreshold:    p.SuccessThreshold,
	}
}

func fromRollout(r servicesmgr.Rollout) *wsv1.RolloutSpec {
	if r.MaxSurge == "" && r.MaxUnavailable == "" {
		return nil
	}
	return &wsv1.RolloutSpec{MaxSurge: r.MaxSurge, MaxUnavailable: r.MaxUnavailable}
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
func (s *Service) servicePublicURLs(svc servicesmgr.Service, hdr *connect.Header) map[string]string {
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
func (s *Service) publicDomainFor(hdr *connect.Header) string {
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
func (s *Service) ListOCIImages(ctx context.Context, req *wsv1.ListOCIImagesRequest) (*wsv1.ListOCIImagesResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	owner := req.GetOwner()
	if owner == "" {
		owner = s.toolchainOrg
	}
	if !roles.ValidComponent(owner) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "owner must be a simple name")
	}
	// A tenant may browse the SHARED catalog namespaces (the toolchain org, the
	// sandbox org and the system `root`), but any other namespace must be one it
	// owns.
	if owner != s.toolchainOrg && owner != s.sandboxOrg && owner != "root" {
		owned, err := s.members.OwnsOrg(tenant, owner)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		if !owned {
			return nil, connect.NewError(connect.CodeNotFound, "namespace not found")
		}
	}
	name := req.GetName()
	host := ""
	if s.builder != nil {
		host = s.builder.RegistryHost
	}
	// Prefer the standard registry v2 API (artifact) so the whole deployment
	// sources images, builds and the catalog from ONE registry; Forgejo stays
	// the git host. Fall back to the Forgejo packages API only when no registry
	// client is configured (legacy).
	if s.registry != nil {
		imgs, err := s.registry.ListImages(ctx, owner, name)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		out := make([]*wsv1.OCIImage, 0, len(imgs))
		for _, im := range imgs {
			out = append(out, &wsv1.OCIImage{
				Owner: im.Owner, Name: im.Name, Tag: im.Tag,
				Ref: host + "/" + im.Owner + "/" + im.Name + ":" + im.Tag,
			})
		}
		return &wsv1.ListOCIImagesResponse{Images: out}, nil
	}
	pkgs, err := s.git.ListContainerPackages(ctx, owner, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.OCIImage, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, &wsv1.OCIImage{
			Owner: p.Owner, Name: p.Name, Tag: p.Tag,
			Ref: host + "/" + p.Owner + "/" + p.Name + ":" + p.Tag,
		})
	}
	return &wsv1.ListOCIImagesResponse{Images: out}, nil
}

// BuildSandboxImage builds an image from a repository Dockerfile (context = a
// repo subdirectory) and pushes it under the deployment registry. The result
// is NOT auto-registered in the catalog (the catalog is deployment-curated).
// imageNameRe: a single path segment (no '/', no ':').
var imageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ImportImage mirrors an upstream image (public or private) into the registry
// under an org the caller owns, with a no-op `FROM <source>` rebuild. It is the
// OCI analogue of repo-import: the caller names the destination org and the new
// image is recorded under that org (which must belong to the tenant).
func (s *Service) ImportImage(ctx context.Context, req *wsv1.ImportImageRequest) (*wsv1.ImportImageResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	m := req
	org, name, tag, source := m.GetOrg(), m.GetName(), m.GetTag(), m.GetSource()
	if !roles.ValidComponent(org) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org must be a simple name")
	}
	if !imageNameRe.MatchString(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name must be a single simple name")
	}
	if !roles.ValidComponent(tag) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "tag must be a simple name")
	}
	if source == "" || strings.ContainsAny(source, " \t\n") {
		return nil, connect.NewError(connect.CodeInvalidArgument, "source must be an image ref")
	}
	if s.builder == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "image builder not configured")
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
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.ImportImageResponse{ImageRef: res.ImageRef, Log: res.Log}, nil
}

func toSandboxInfo(sb sandboxmgr.Sandbox) *wsv1.SandboxInfo {
	return &wsv1.SandboxInfo{
		Name: sb.Name, Image: sb.Image, Phase: sb.Phase, Ready: sb.Ready,
		Url: sb.URL, Creator: sb.Creator, CreatedAt: sb.CreatedAt,
		Session:  sb.Session,
		Restarts: sb.Restarts, Message: sb.Message,
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
// copyHeaders is a no-op under connect v2: request metadata lives on the
// context's CallInfo, and the agent client interceptor (fwdClientInterceptor)
// forwards the caller's headers onto every outbound agent call. Kept so call
// sites stay unchanged.
func copyHeaders[T any](_ *T, _ *connect.Header) {}

var _ wsv1connect.BranchSessionServiceHandler = (*Service)(nil)

// ---- configmaps / secrets ----

// PutConfigMap creates/updates a ConfigMap (A1 gating lives in servicesmgr).
func (s *Service) PutConfigMap(ctx context.Context, req *wsv1.PutConfigMapRequest) (*wsv1.PutConfigMapResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	cm, err := s.services.PutConfigMap(ctx, req.GetName(), req.GetData(), tenant, resolveNamespace(hdrFrom(ctx), ""))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.PutConfigMapResponse{Configmap: toConfigMapInfo(cm)}, nil
}

func (s *Service) ListConfigMaps(ctx context.Context, _ *wsv1.ListConfigMapsRequest) (*wsv1.ListConfigMapsResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return &wsv1.ListConfigMapsResponse{}, nil
	}
	all, err := s.services.ListConfigMaps(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.ConfigMapInfo, 0, len(all))
	for _, cm := range all {
		if !owned(cm.Creator, tenant) {
			continue
		}
		out = append(out, toConfigMapInfo(cm))
	}
	return &wsv1.ListConfigMapsResponse{Configmaps: out}, nil
}

func (s *Service) DeleteConfigMap(ctx context.Context, req *wsv1.DeleteConfigMapRequest) (*wsv1.DeleteConfigMapResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return &wsv1.DeleteConfigMapResponse{Ok: false}, nil
	}
	cur, gerr := s.services.GetConfigMap(ctx, req.GetName())
	if gerr != nil || !writable(cur.Creator, tenant) {
		return &wsv1.DeleteConfigMapResponse{Ok: false}, nil
	}
	ok, err := s.services.DeleteConfigMap(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeleteConfigMapResponse{Ok: ok}, nil
}

func toConfigMapInfo(cm servicesmgr.ConfigMap) *wsv1.ConfigMapInfo {
	return &wsv1.ConfigMapInfo{Name: cm.Name, Data: cm.Data, Creator: cm.Creator,
		Namespace: cm.Namespace, CreatedAt: cm.CreatedAt}
}

func (s *Service) PutSecret(ctx context.Context, req *wsv1.PutSecretRequest) (*wsv1.PutSecretResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "service backend not configured")
	}
	sec, err := s.services.PutSecret(ctx, req.GetName(), req.GetData(), tenant, resolveNamespace(hdrFrom(ctx), ""))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.PutSecretResponse{Secret: toSecretInfo(sec)}, nil
}

func (s *Service) ListSecrets(ctx context.Context, _ *wsv1.ListSecretsRequest) (*wsv1.ListSecretsResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return &wsv1.ListSecretsResponse{}, nil
	}
	all, err := s.services.ListSecrets(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out := make([]*wsv1.SecretInfo, 0, len(all))
	for _, sec := range all {
		if !owned(sec.Creator, tenant) {
			continue
		}
		out = append(out, toSecretInfo(sec))
	}
	return &wsv1.ListSecretsResponse{Secrets: out}, nil
}

func (s *Service) DeleteSecret(ctx context.Context, req *wsv1.DeleteSecretRequest) (*wsv1.DeleteSecretResponse, error) {
	tenant, err := s.sandboxAuth(ctx, hdrFrom(ctx))
	if err != nil {
		return nil, err
	}
	if s.services == nil {
		return &wsv1.DeleteSecretResponse{Ok: false}, nil
	}
	cur, gerr := s.services.GetSecret(ctx, req.GetName())
	if gerr != nil || !writable(cur.Creator, tenant) {
		return &wsv1.DeleteSecretResponse{Ok: false}, nil
	}
	ok, err := s.services.DeleteSecret(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &wsv1.DeleteSecretResponse{Ok: ok}, nil
}

func toSecretInfo(sec servicesmgr.Secret) *wsv1.SecretInfo {
	return &wsv1.SecretInfo{Name: sec.Name, Data: sec.Data, Creator: sec.Creator,
		Namespace: sec.Namespace, CreatedAt: sec.CreatedAt}
}
