// Package workspacesvc implements workspace.v1.BranchSessionService: the trusted
// entry point for creating sessions (which derives the role + immutable preset)
// and the read/browse surface for the webui.
package workspacesvc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
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
	defaultBase  string // default sandbox base image (empty = required)
	toolchainOrg string // default owner for ListOCIImages
	svcToken     string // shared service token (sandbox-only service-to-service)
	svcTenant    string // tenant the service token resolves to (sandbox ownership)
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
	// DefaultBase is the sandbox base image used when CreateSandbox omits one.
	DefaultBase string
	// ToolchainOrg is the default owner ListOCIImages browses.
	ToolchainOrg string
	// ServiceToken + ServiceTenant enable the sandbox-only service-to-service
	// path used by the workspace extension (which has no tenant token). Empty
	// disables it.
	ServiceToken  string
	ServiceTenant string
}

// New builds the service.
func New(d Deps) *Service {
	return &Service{
		agent: d.Agent, members: d.Members, git: d.Forgejo, sbx: d.Sandbox,
		services: d.Services,
		builder:  d.Builder, runtime: d.Runtime, defaultBase: d.DefaultBase, toolchainOrg: d.ToolchainOrg,
		svcToken: d.ServiceToken, svcTenant: d.ServiceTenant,
	}
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
		if err := s.createSession(ctx, req.Header(), session, roles.PresetFor(role), req.Msg.GetModel()); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&wsv1.EnsureBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Org: org, Repo: repo, Branch: branch,
		Role: string(role), Preset: roles.PresetFor(role), Sandbox: session,
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
	return connect.NewResponse(&wsv1.ForkBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Org: org, Repo: repo, Branch: branch,
		Role: string(role), Preset: roles.PresetFor(role), Sandbox: session,
	}}), nil
}

// CreateFreeSession creates a standalone (non-repo-bound) session. Free
// sessions carry a tenant-scoped role: admin (manage org/repo), planner
// (read + sandbox) or explorer (read-only). Any number of each may exist; the
// role decides what the session may DO, never what it may SEE (visibility is
// the tenant).
func (s *Service) CreateFreeSession(ctx context.Context, req *connect.Request[wsv1.CreateFreeSessionRequest]) (*connect.Response[wsv1.CreateFreeSessionResponse], error) {
	tenant, err := s.resolveTenant(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	role := roles.Role(req.Msg.GetRole())
	if role != roles.Admin && role != roles.Planner && role != roles.Explorer {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("role must be admin|planner|explorer"))
	}
	name := req.Msg.GetName()
	if name == "" || !roles.ValidComponent(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name required (simple)"))
	}
	if err := s.members.AddFreeSession(tenant, name, string(role)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.createSession(ctx, req.Header(), name, roles.PresetFor(role), req.Msg.GetModel()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wsv1.CreateFreeSessionResponse{Session: name, Preset: roles.PresetFor(role)}), nil
}

// createSession forwards a trusted CreateSession to the agent.
func (s *Service) createSession(ctx context.Context, hdr map[string][]string, name, preset, model string) error {
	msg := &agentv1.CreateSessionRequest{Name: name, Preset: preset, Model: model}
	r := connect.NewRequest(msg)
	copyHeaders(r, hdr)
	_, err := s.agent.CreateSession(ctx, r)
	return err
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

	// Sandbox phase per session (best effort).
	phase := map[string]string{}
	if sbxs, err := s.sbx.List(ctx); err == nil {
		for _, sb := range sbxs {
			phase[sb.Name] = sb.Phase
		}
	}

	out := []*wsv1.BranchSession{}
	for _, sess := range res.Msg.GetSessions() {
		name := sess.GetName()
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
				Sandbox: name, Phase: phase[name],
			})
			continue
		}
		if role, ok, _ := s.members.FreeRole(tenant, name); ok {
			out = append(out, &wsv1.BranchSession{
				Session: name, Role: role, Preset: role, Sandbox: name, Phase: phase[name],
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
		return connect.NewResponse(&wsv1.GetBranchSessionResponse{BranchSession: &wsv1.BranchSession{
			Session: session, Org: org, Repo: repo, Branch: branch,
			Role: string(role), Preset: roles.PresetFor(role), Sandbox: session,
		}}), nil
	}
	role, ok, _ := s.members.FreeRole(tenant, session)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("branch session not found"))
	}
	return connect.NewResponse(&wsv1.GetBranchSessionResponse{BranchSession: &wsv1.BranchSession{
		Session: session, Role: role, Preset: role, Sandbox: session,
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
	s.deleteSessionAndSandbox(ctx, req.Header(), tenant, session)
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
	s.deleteSessionAndSandbox(ctx, req.Header(), tenant, session)
	if err := s.git.DeleteBranch(ctx, org, repo, branch); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeleteBranchResponse{Ok: true}), nil
}

// deleteSessionAndSandbox deletes a session's sandbox(es) and the agent session.
func (s *Service) deleteSessionAndSandbox(ctx context.Context, hdr map[string][]string, tenant, session string) {
	_, _ = s.sbx.Delete(ctx, session)
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

// ensureMainSession idempotently creates the `org:repo:main` session bound to
// the maintainer role. Best-effort: a repo is still usable if this fails.
func (s *Service) ensureMainSession(ctx context.Context, hdr map[string][]string, org, repo string) {
	session := roles.SessionName(org, repo, roles.MainBranch)
	if s.sessionExists(ctx, hdr, session) {
		return
	}
	if err := s.createSession(ctx, hdr, session, roles.PresetFor(roles.Maintainer), ""); err != nil {
		log.Printf("warn: ensure main session %s: %v", session, err)
	}
}

// ImportRepo migrates an EXTERNAL git repository into `org` (which must belong
// to the caller's tenant). Forgejo clones the FULL repository; when `ref` is
// given, that ref becomes the default branch and every OTHER branch is deleted
// (single-branch import). The imported repo's default-branch session is ensured.
// An existing repo is refused (never overwritten).
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

	ref := m.GetRef()
	info, err := s.git.MigrateRepo(ctx, org, repo, url, m.GetAuthUser(), m.GetAuthToken(), m.GetDescription(), m.GetPrivate(), m.GetMirror())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("migrate: %w", err))
	}
	branch := info.DefaultBranch
	if branch == "" {
		branch = roles.MainBranch
	}
	if ref != "" {
		branches, err := s.git.Branches(ctx, org, repo)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		found := false
		for _, b := range branches {
			if b.Name == ref {
				found = true
			}
		}
		if !found {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("ref %q not found in the imported repository", ref))
		}
		if err := s.git.SetDefaultBranch(ctx, org, repo, ref); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, b := range branches {
			if b.Name != ref {
				_ = s.git.DeleteBranch(ctx, org, repo, b.Name)
			}
		}
		branch = ref
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
		if err := s.createSession(ctx, req.Header(), roles.SessionName(org, repo, branch), roles.PresetFor(roles.RoleForBranch(branch)), ""); err != nil {
			log.Printf("warn: ensure imported session %s/%s:%s: %v", org, repo, branch, err)
		}
	}
	return connect.NewResponse(&wsv1.ImportRepoResponse{Repo: &wsv1.RepoInfo{
		Org: org, Repo: repo, DefaultBranch: branch, Private: info.Private,
	}}), nil
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
func (s *Service) CreateMR(ctx context.Context, req *connect.Request[wsv1.CreateMRRequest]) (*connect.Response[wsv1.CreateMRResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	base := m.GetBase()
	if base == "" {
		base = roles.MainBranch
	}
	index, url, err := s.git.CreateMR(ctx, m.GetOrg(), m.GetRepo(), m.GetTitle(), m.GetHead(), base, m.GetBody())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
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
func (s *Service) MergeMR(ctx context.Context, req *connect.Request[wsv1.MergeMRRequest]) (*connect.Response[wsv1.MergeMRResponse], error) {
	m := req.Msg
	if err := s.ensureVisible(ctx, req.Header(), m.GetOrg(), m.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.git.MergeMR(ctx, m.GetOrg(), m.GetRepo(), m.GetIndex()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.MergeMRResponse{Ok: true}), nil
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
	for _, sb := range sbxs {
		// Visibility: only sandboxes this tenant created.
		if sb.Creator != "" && sb.Creator != tenant {
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
	// Any base image is accepted. Derive a runnable sandbox by injecting the
	// easyworker binary (easylab's model). Empty = the deployment default base.
	base := req.Msg.GetImage()
	if base == "" {
		base = s.defaultBase
	}
	if base == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no base image given and no default configured"))
	}
	if s.builder == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("image builder not configured"))
	}
	derived, _, err := s.builder.Derive(ctx, base)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("derive sandbox image: %w", err))
	}
	sb, _, err := s.sbx.Create(ctx, sandboxmgr.Spec{
		Name: name, Image: derived, CPU: req.Msg.GetCpu(), Memory: req.Msg.GetMemory(),
		Env: req.Msg.GetEnv(), Creator: tenant,
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
	return connect.NewResponse(&wsv1.GetSandboxResponse{Sandbox: toSandboxInfo(sb)}), nil
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
	if !roles.ValidComponent(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be simple"))
	}
	// Ownership guard: only the creator may update an existing service.
	if existing, gerr := s.services.Get(ctx, name); gerr == nil {
		if existing.Creator != "" && existing.Creator != tenant {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("service belongs to another tenant"))
		}
	}

	spec := servicesmgr.Spec{
		Name: name, Image: m.GetImage(), Command: m.GetCommand(),
		Env: m.GetEnv(), CPU: m.GetCpu(), Memory: m.GetMemory(),
		Replicas: m.GetReplicas(), ContainerPort: m.GetContainerPort(),
		ServicePort: m.GetServicePort(), Creator: tenant,
		Runtime: s.renderRuntime(m.GetKvm(), m.GetGpuCount()),
	}
	if spec.Image == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image is required"))
	}
	svc, err := s.services.Deploy(ctx, spec)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&wsv1.DeployServiceResponse{Service: toServiceInfo(svc)}), nil
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
		out = append(out, toServiceInfo(svc))
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

func toServiceInfo(svc servicesmgr.Service) *wsv1.ServiceInfo {
	return &wsv1.ServiceInfo{
		Name: svc.Name, Image: svc.Image, Phase: svc.Phase, Ready: svc.Ready,
		Replicas: svc.Replicas, Url: svc.URL,
	}
}

// ---- sandbox images ----

// ListOCIImages browses container images in the registry. `owner` selects the
// namespace (default: the deployment toolchain org); `name` narrows to one
// image, listing its tags.
func (s *Service) ListOCIImages(ctx context.Context, req *connect.Request[wsv1.ListOCIImagesRequest]) (*connect.Response[wsv1.ListOCIImagesResponse], error) {
	if _, err := s.sandboxAuth(ctx, req.Header()); err != nil {
		return nil, err
	}
	owner := req.Msg.GetOwner()
	if owner == "" {
		owner = s.toolchainOrg
	}
	if !roles.ValidComponent(owner) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("owner must be a simple name"))
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

func toSandboxInfo(sb sandboxmgr.Sandbox) *wsv1.SandboxInfo {
	return &wsv1.SandboxInfo{
		Name: sb.Name, Image: sb.Image, Phase: sb.Phase, Ready: sb.Ready,
		Url: sb.URL, Creator: sb.Creator, CreatedAt: sb.CreatedAt,
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
