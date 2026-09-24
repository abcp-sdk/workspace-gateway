package workspacesvc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"k8s.io/client-go/kubernetes/fake"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
	"github.com/abcp-sdk/workspace-gateway/internal/servicesmgr"
)

// stubAgent implements only GetIdentity (the rest panic if called).
type stubAgent struct {
	agentv1connect.AgentServiceClient
	tenant string
}

func (a stubAgent) GetIdentity(context.Context, *connect.Request[agentv1.GetIdentityRequest]) (*connect.Response[agentv1.GetIdentityResponse], error) {
	return connect.NewResponse(&agentv1.GetIdentityResponse{Tenant: a.tenant}), nil
}

func TestSandboxAuthServiceToken(t *testing.T) {
	s := &Service{svcToken: "svc-secret", svcTenant: "workspace-extension"}
	ctx := context.Background()

	// A matching service token resolves to the configured tenant without ever
	// dialing the agent.
	hdr := map[string][]string{"Authorization": {"Bearer svc-secret"}}
	tenant, err := s.sandboxAuth(ctx, hdr)
	if err != nil {
		t.Fatalf("sandboxAuth: %v", err)
	}
	if tenant != "workspace-extension" {
		t.Fatalf("tenant = %q, want workspace-extension", tenant)
	}

	// A wrong token must NOT short-circuit: it falls through to the agent
	// identity path, which resolves the caller's own tenant (never the service
	// tenant).
	s.agent = stubAgent{tenant: "real-tenant"}
	bad := map[string][]string{"Authorization": {"Bearer nope"}}
	got, err := s.sandboxAuth(ctx, bad)
	if err != nil {
		t.Fatalf("sandboxAuth: %v", err)
	}
	if got != "real-tenant" {
		t.Fatalf("wrong token resolved %q, want real-tenant", got)
	}
}

func TestSandboxAuthDisabledWithoutToken(t *testing.T) {
	// bearerToken is case-insensitive on the header name.
	if got := bearerToken(map[string][]string{"authorization": {"Bearer x"}}); got != "x" {
		t.Fatalf("bearerToken = %q, want x (case-insensitive header)", got)
	}
}

func TestMRErrorMapping(t *testing.T) {
	// A Forgejo 409 (merge conflict / not mergeable) must surface as
	// FailedPrecondition, not an opaque Internal.
	if got := connect.CodeOf(mrError(&forgejo.ErrConflict{Reason: "merge conflict"})); got != connect.CodeFailedPrecondition {
		t.Fatalf("conflict code = %v, want FailedPrecondition", got)
	}
	if got := connect.CodeOf(mrError(&forgejo.ErrNotFound{URL: "u"})); got != connect.CodeNotFound {
		t.Fatalf("not-found code = %v, want NotFound", got)
	}
	if got := connect.CodeOf(mrError(errors.New("boom"))); got != connect.CodeInternal {
		t.Fatalf("generic code = %v, want Internal", got)
	}
}

func TestPublicDomainFor(t *testing.T) {
	s := &Service{sandboxNS: "worker"}

	// Infer from X-Forwarded-Host: drop the first two labels.
	hdr := map[string][]string{"X-Forwarded-Host": {"workspace.agent.10.199.64.20.nip.io"}}
	if got := s.publicDomainFor(hdr); got != "10.199.64.20.nip.io" {
		t.Fatalf("inferred domain = %q", got)
	}
	// Per-port public URLs: only tcp80 ports get one; siblings get their own host.
	svc := servicesmgr.Service{Name: "app", Ports: []servicesmgr.Port{
		{Port: 80, Protocol: "tcp", TargetPort: 8080},
		{Port: 443, Protocol: "udp", TargetPort: 8080},
		{Suffix: "admin", Port: 80, Protocol: "tcp", TargetPort: 8081},
	}}
	urls := s.servicePublicURLs(svc, hdr)
	if urls[""] != "https://app.worker.10.199.64.20.nip.io" {
		t.Fatalf("primary public url = %q", urls[""])
	}
	if urls["admin"] != "https://app-admin.worker.10.199.64.20.nip.io" {
		t.Fatalf("sibling public url = %q", urls["admin"])
	}

	// fenjin.org form.
	hdr = map[string][]string{"X-Forwarded-Host": {"workspace.agent.fenjin.org"}}
	if got := s.publicDomainFor(hdr); got != "fenjin.org" {
		t.Fatalf("fenjin domain = %q", got)
	}

	// A port in the host is stripped.
	hdr = map[string][]string{"Host": {"workspace.agent.10.199.64.20.nip.io:8443"}}
	if got := s.publicDomainFor(hdr); got != "10.199.64.20.nip.io" {
		t.Fatalf("port-stripped domain = %q", got)
	}

	// Too few labels (bare/custom domain) -> no inference.
	if got := s.publicDomainFor(map[string][]string{"Host": {"workspace.com"}}); got != "" {
		t.Fatalf("bare domain should not infer: %q", got)
	}
	// In-cluster callers carry a Service DNS host: never a public domain.
	if got := s.publicDomainFor(map[string][]string{"Host": {"workspace-gateway.agent.svc.cluster.local"}}); got != "" {
		t.Fatalf("svc.cluster.local should not infer: %q", got)
	}
	// A bare IP (ClusterIP) host -> no domain.
	if got := s.publicDomainFor(map[string][]string{"Host": {"172.18.15.119"}}); got != "" {
		t.Fatalf("bare IP should not infer: %q", got)
	}
	// No host at all -> empty.
	if got := s.publicDomainFor(nil); got != "" {
		t.Fatalf("no host should be empty: %q", got)
	}

	// An explicit configured domain wins.
	s2 := &Service{sandboxNS: "worker", publicServiceDomain: "worker.example.com"}
	if got := s2.servicePublicURLs(servicesmgr.Service{Name: "app", Ports: []servicesmgr.Port{{Port: 80, Protocol: "tcp"}}}, nil)[""]; got != "https://app.worker.worker.example.com" {
		t.Fatalf("configured public url = %q", got)
	}
}

func TestServicePortsResolve(t *testing.T) {
	// Empty specs => default single tcp80 -> containerPort.
	p, err := servicePorts(nil, 8080)
	if err != nil || len(p) != 1 || p[0].Port != 80 || p[0].TargetPort != 8080 {
		t.Fatalf("default = %+v err=%v", p, err)
	}
	// Presets + grouping + target default.
	p, err = servicePorts([]*wsv1.ServicePortSpec{
		{Preset: "tcp80", TargetPort: 8080},
		{Preset: "udp443", TargetPort: 8080},
		{Name: "admin", Preset: "tcp80", TargetPort: 8081},
	}, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 3 || p[0].Protocol != "tcp" || p[1].Protocol != "udp" || p[1].Port != 443 {
		t.Fatalf("resolved = %+v", p)
	}
	if p[2].Suffix != "admin" || p[2].TargetPort != 8081 {
		t.Fatalf("sibling = %+v", p[2])
	}
	// Bad preset rejected.
	if _, err := servicePorts([]*wsv1.ServicePortSpec{{Preset: "bogus"}}, 8080); err == nil {
		t.Fatal("bad preset must error")
	}
	// Two tcp80 in the same suffix rejected (port collision).
	if _, err := servicePorts([]*wsv1.ServicePortSpec{{Preset: "tcp80"}, {Preset: "tcp80"}}, 8080); err == nil {
		t.Fatal("duplicate tcp80 must error")
	}
}

// stubAgentLocale records the CreateSession it receives and answers GetConfig
// with a configured locale.
type stubAgentLocale struct {
	agentv1connect.AgentServiceClient
	configLocale string
	lastCreate   *agentv1.CreateSessionRequest
}

func (a *stubAgentLocale) GetConfig(context.Context, *connect.Request[agentv1.GetConfigRequest]) (*connect.Response[agentv1.GetConfigResponse], error) {
	return connect.NewResponse(&agentv1.GetConfigResponse{Key: "locale", Value: a.configLocale}), nil
}

func (a *stubAgentLocale) CreateSession(_ context.Context, r *connect.Request[agentv1.CreateSessionRequest]) (*connect.Response[agentv1.CreateSessionResponse], error) {
	a.lastCreate = r.Msg
	return connect.NewResponse(&agentv1.CreateSessionResponse{Ok: true, SessionName: r.Msg.GetName()}), nil
}

// An EMPTY requested locale must be resolved to the tenant's configured locale
// BEFORE pinning — otherwise the agent pins the session to English.
func TestCreateSessionResolvesTenantLocale(t *testing.T) {
	a := &stubAgentLocale{configLocale: "zh"}
	s := &Service{agent: a}
	hdr := map[string][]string{"Authorization": {"Bearer t"}}

	if err := s.createSession(context.Background(), hdr, "sess", "maintainer", "", ""); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if got := a.lastCreate.GetLocale(); got != "zh" {
		t.Fatalf("pinned locale = %q, want zh (from tenant config)", got)
	}

	// An explicit request locale still wins over the tenant config.
	if err := s.createSession(context.Background(), hdr, "sess2", "maintainer", "", "en"); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if got := a.lastCreate.GetLocale(); got != "en" {
		t.Fatalf("pinned locale = %q, want en (explicit)", got)
	}
}

// resolveVolumes must reject an unknown / foreign claim and accept the tenant's
// own, mapping it to a servicesmgr.VolumeMount.
func TestResolveVolumesOwnership(t *testing.T) {
	fc := fake.NewSimpleClientset()
	sm := servicesmgr.NewWithClientset(fc, servicesmgr.Config{Namespace: "worker"})
	s := &Service{services: sm}
	ctx := context.Background()

	if _, err := sm.CreatePVC(ctx, "data", "1Gi", "workspace-local", "myuser"); err != nil {
		t.Fatal(err)
	}

	// The owning tenant resolves it.
	vols, err := s.resolveVolumes(ctx, "myuser", []*wsv1.VolumeMountSpec{{Pvc: "data", MountPath: "/data"}})
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if len(vols) != 1 || vols[0].PVC != "data" || vols[0].MountPath != "/data" {
		t.Fatalf("vols = %+v", vols)
	}

	// Another tenant gets NotFound (never leaks the claim).
	if _, err := s.resolveVolumes(ctx, "other", []*wsv1.VolumeMountSpec{{Pvc: "data", MountPath: "/data"}}); err == nil {
		t.Fatal("expected foreign-tenant refusal")
	}
	// An unknown claim is NotFound.
	if _, err := s.resolveVolumes(ctx, "myuser", []*wsv1.VolumeMountSpec{{Pvc: "nope", MountPath: "/data"}}); err == nil {
		t.Fatal("expected not-found refusal")
	}
	// A relative mount path is InvalidArgument.
	if _, err := s.resolveVolumes(ctx, "myuser", []*wsv1.VolumeMountSpec{{Pvc: "data", MountPath: "data"}}); err == nil {
		t.Fatal("expected bad mount_path refusal")
	}
}

func TestValidateSandboxImage(t *testing.T) {
	s := &Service{sandboxOrg: "sandbox"}
	ok := []string{
		"git.agent.svc.cluster.local/sandbox/sandbox-base:debian-trixie",
		"git.agent.svc.cluster.local/sandbox/sandbox-node:debian-trixie",
		"http://git.agent.svc.cluster.local/sandbox/sandbox-go",
		"sandbox/sandbox-base",
		"git.agent.fenjin.org/sandbox/sandbox-base:debian-trixie",
	}
	for _, img := range ok {
		if err := s.validateSandboxImage(img); err != nil {
			t.Errorf("expected %q to be accepted, got %v", img, err)
		}
	}
	bad := []string{
		"git.agent.svc.cluster.local/agent-toolchain/toolchain-base:debian-trixie",
		"docker.io/library/debian:trixie",
		"root/sandbox:abc",
		"myuser/evil:latest",
	}
	for _, img := range bad {
		if err := s.validateSandboxImage(img); err == nil {
			t.Errorf("expected %q to be refused", img)
		}
	}
	// An unset org disables the guard (deployment opt-out).
	none := &Service{}
	if err := none.validateSandboxImage("docker.io/library/debian:trixie"); err != nil {
		t.Errorf("unset sandboxOrg should accept anything, got %v", err)
	}
}
