package workspacesvc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"k8s.io/client-go/kubernetes/fake"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/servicesmgr"
)

// stubAgent implements only GetIdentity (the rest panic if called).
type stubAgent struct {
	agentv1connect.AgentServiceClient
	tenant string
}

func (a stubAgent) GetIdentity(context.Context, *agentv1.GetIdentityRequest) (*agentv1.GetIdentityResponse, error) {
	return &agentv1.GetIdentityResponse{Tenant: a.tenant}, nil
}

func TestSandboxAuthServiceToken(t *testing.T) {
	s := &Service{svcToken: "svc-secret", svcTenant: "workspace-extension"}
	ctx := context.Background()

	// A matching service token resolves to the configured tenant without ever
	// dialing the agent.
	hdr := testHdr("Authorization", "Bearer svc-secret")
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
	bad := testHdr("Authorization", "Bearer nope")
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
	if got := bearerToken(testHdr("authorization", "Bearer x")); got != "x" {
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
	hdr := testHdr("X-Forwarded-Host", "workspace.agent.10.199.64.20.nip.io")
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
	hdr = testHdr("X-Forwarded-Host", "workspace.agent.fenjin.org")
	if got := s.publicDomainFor(hdr); got != "fenjin.org" {
		t.Fatalf("fenjin domain = %q", got)
	}

	// A port in the host is stripped.
	hdr = testHdr("Host", "workspace.agent.10.199.64.20.nip.io:8443")
	if got := s.publicDomainFor(hdr); got != "10.199.64.20.nip.io" {
		t.Fatalf("port-stripped domain = %q", got)
	}

	// Too few labels (bare/custom domain) -> no inference.
	if got := s.publicDomainFor(testHdr("Host", "workspace.com")); got != "" {
		t.Fatalf("bare domain should not infer: %q", got)
	}
	// In-cluster callers carry a Service DNS host: never a public domain.
	if got := s.publicDomainFor(testHdr("Host", "workspace-gateway.agent.svc.cluster.local")); got != "" {
		t.Fatalf("svc.cluster.local should not infer: %q", got)
	}
	// A bare IP (ClusterIP) host -> no domain.
	if got := s.publicDomainFor(testHdr("Host", "172.18.15.119")); got != "" {
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

func (a *stubAgentLocale) GetConfig(context.Context, *agentv1.GetConfigRequest) (*agentv1.GetConfigResponse, error) {
	return &agentv1.GetConfigResponse{Key: "locale", Value: a.configLocale}, nil
}

func (a *stubAgentLocale) CreateSession(_ context.Context, r *agentv1.CreateSessionRequest) (*agentv1.CreateSessionResponse, error) {
	a.lastCreate = r
	return &agentv1.CreateSessionResponse{Ok: true, SessionName: r.GetName()}, nil
}

// An EMPTY requested locale must be resolved to the tenant's configured locale
// BEFORE pinning — otherwise the agent pins the session to English.
func TestCreateSessionResolvesTenantLocale(t *testing.T) {
	a := &stubAgentLocale{configLocale: "zh"}
	s := &Service{agent: a}
	hdr := testHdr("Authorization", "Bearer t")

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

// TestValidateSandboxImageRegistryHost pins the sandbox-image SOURCE guard: when
// SANDBOX_IMAGE_REGISTRY_HOST is set, a sandbox image must come from that
// registry (artifact) — not the build/push registry (Forgejo) or a bare name.
func TestValidateSandboxImageRegistryHost(t *testing.T) {
	s := &Service{sandboxOrg: "sandbox", sandboxImageRegistryHost: "artifact.worker.svc.cluster.local"}
	ok := []string{
		"artifact.worker.svc.cluster.local/sandbox/sandbox-base:debian-trixie",
		"artifact.worker.svc.cluster.local/sandbox/sandbox-node:debian-trixie",
		"http://artifact.worker.svc.cluster.local/sandbox/sandbox-go",
	}
	for _, img := range ok {
		if err := s.validateSandboxImage(img); err != nil {
			t.Errorf("expected %q to be accepted, got %v", img, err)
		}
	}
	bad := []string{
		// The build/push registry (Forgejo) is NO LONGER a valid sandbox source.
		"git.agent.svc.cluster.local/sandbox/sandbox-base:debian-trixie",
		// A bare `<org>/<name>` (no host) could resolve to Docker Hub: refused.
		"sandbox/sandbox-base",
		"docker.io/library/debian:trixie",
		"artifact.worker.svc.cluster.local/agent-toolchain/toolchain-base:debian-trixie",
	}
	for _, img := range bad {
		if err := s.validateSandboxImage(img); err == nil {
			t.Errorf("expected %q to be refused", img)
		}
	}
}

func TestSandboxAccessible(t *testing.T) {
	sb := sandboxmgr.Sandbox{Name: "sb", Creator: "myuser", Session: "o:r:main"}
	cases := []struct {
		name    string
		tenant  string
		session string
		want    bool
	}{
		// Tenant console (no session header): every sandbox the tenant owns.
		{"tenant console sees own", "myuser", "", true},
		// Another tenant is invisible regardless.
		{"foreign tenant hidden", "other", "", false},
		{"foreign tenant + session hidden", "other", "o:r:feat", false},
		// Agent session: ONLY its own session's sandboxes.
		{"own session allowed", "myuser", "o:r:main", true},
		{"other session blocked", "myuser", "o:r:feat", false},
	}
	for _, c := range cases {
		if got := sandboxAccessible(sb, c.tenant, c.session); got != c.want {
			t.Errorf("%s: sandboxAccessible = %v, want %v", c.name, got, c.want)
		}
	}
	// A legacy sandbox with no session binding is invisible to an agent session
	// (only the tenant console can see it).
	legacy := sandboxmgr.Sandbox{Name: "l", Creator: "myuser", Session: ""}
	if sandboxAccessible(legacy, "myuser", "o:r:main") {
		t.Error("unbound sandbox must be hidden from an agent session")
	}
	if !sandboxAccessible(legacy, "myuser", "") {
		t.Error("unbound sandbox must be visible to the tenant console")
	}
}

func TestBranchTargetAllowed(t *testing.T) {
	cases := []struct {
		name   string
		caller string
		org    string
		repo   string
		want   bool
	}{
		// A human webui caller (no session header) is the tenant console.
		{"no session unrestricted", "", "other", "lib", true},
		// A branch session may only act on its OWN repository.
		{"own repo allowed", "acme:web:main", "acme", "web", true},
		{"other repo refused", "acme:web:main", "other", "lib", false},
		{"other org refused", "acme:web:main", "acme", "lib", false},
		// A free (non-branch) session has no repo binding to compare.
		{"free session unrestricted", "myuser-admin", "acme", "web", true},
	}
	for _, c := range cases {
		if got := branchTargetAllowed(c.caller, c.org, c.repo); got != c.want {
			t.Errorf("%s: branchTargetAllowed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCreateSandboxRefusesExisting(t *testing.T) {
	sm := sandboxmgr.NewWithClientset(fake.NewSimpleClientset(), sandboxmgr.Config{Namespace: "worker"})
	s := &Service{
		svcToken: "tok", svcTenant: "ten", sbx: sm,
		sandboxOrg: "sandbox", defaultSandboxImage: "sandbox/sandbox-base",
	}
	ctx := context.Background()
	// Seed a live sandbox, then attempt to create the same name again.
	if _, _, err := sm.Create(ctx, sandboxmgr.Spec{Name: "dup", Image: "sandbox/sandbox-base"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := &wsv1.CreateSandboxRequest{Name: "dup"}
	ctx = WithTestHeaders(ctx, testHdr("Authorization", "Bearer tok"))
	_, err := s.CreateSandbox(ctx, req)
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("create existing code = %v (err %v), want AlreadyExists", connect.CodeOf(err), err)
	}
	// The original sandbox is untouched.
	if _, ok, _ := sm.Get(ctx, "dup"); !ok {
		t.Fatal("existing sandbox was destroyed by a refused create")
	}
}

// CreateSandbox must authorize mounted PVCs the SAME way the service path does:
// a claim the caller's tenant does not own (or that does not exist) is refused
// BEFORE any pod is created, so a session can never mount another tenant's
// volume. (The owning-tenant success path is covered by
// TestResolveVolumesOwnership, which exercises the same resolveVolumes call.)
func TestCreateSandboxAuthorizesPVCs(t *testing.T) {
	fc := fake.NewSimpleClientset()
	sm := servicesmgr.NewWithClientset(fc, servicesmgr.Config{Namespace: "worker"})
	sbx := sandboxmgr.NewWithClientset(fc, sandboxmgr.Config{Namespace: "worker"})
	s := &Service{
		services: sm, sbx: sbx,
		sandboxOrg: "sandbox", defaultSandboxImage: "sandbox/sandbox-base",
		svcToken: "tok", svcTenant: "myuser",
	}
	ctx := WithTestHeaders(context.Background(), testHdr("Authorization", "Bearer tok"))

	// A claim owned by another tenant is refused (never leaks it).
	if _, err := sm.CreatePVC(ctx, "foreign", "1Gi", "workspace-local", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSandbox(ctx, &wsv1.CreateSandboxRequest{
		Name: "nope", Volumes: []*wsv1.VolumeMountSpec{{Pvc: "foreign", MountPath: "/data"}},
	}); err == nil {
		t.Fatal("expected foreign-PVC refusal")
	} else if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign-PVC refusal code = %v, want NotFound", connect.CodeOf(err))
	}
	if _, ok, _ := sbx.Get(ctx, "nope"); ok {
		t.Fatal("a foreign-PVC create must not leave a sandbox behind")
	}

	// An unknown claim is refused too.
	if _, err := s.CreateSandbox(ctx, &wsv1.CreateSandboxRequest{
		Name: "missing", Volumes: []*wsv1.VolumeMountSpec{{Pvc: "ghost", MountPath: "/data"}},
	}); err == nil {
		t.Fatal("expected unknown-PVC refusal")
	}
}

// TestCancelBuild exercises the CancelBuild handler directly: an unknown id is
// NotFound, a running build is canceled (ok=true, state failed, log annotated),
// and a second cancel is a no-op (ok=false).
func TestCancelBuild(t *testing.T) {
	s := &Service{svcToken: "tok", svcTenant: "ten"}
	ctx := WithTestHeaders(context.Background(), testHdr("Authorization", "Bearer tok"))

	if _, err := s.CancelBuild(ctx, &wsv1.CancelBuildRequest{BuildId: "missing"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown build: code = %v, want NotFound", connect.CodeOf(err))
	}

	id := "b1"
	canceled := false
	buildsMu.Lock()
	builds[id] = &buildState{state: "running", image: "o/i:t", created: 1, cancel: func() { canceled = true }}
	buildsMu.Unlock()
	t.Cleanup(func() { buildsMu.Lock(); delete(builds, id); buildsMu.Unlock() })

	res, err := s.CancelBuild(ctx, &wsv1.CancelBuildRequest{BuildId: id})
	if err != nil || !res.GetOk() {
		t.Fatalf("cancel running: res=%v err=%v", res, err)
	}
	if !canceled {
		t.Fatal("cancel func was not called")
	}
	buildsMu.Lock()
	st := builds[id]
	state, log := st.state, st.log
	buildsMu.Unlock()
	if state != "failed" || log != "build canceled by user" {
		t.Fatalf("state=%q log=%q, want failed + canceled note", state, log)
	}

	// Second cancel is a no-op.
	if res, err := s.CancelBuild(ctx, &wsv1.CancelBuildRequest{BuildId: id}); err != nil || res.GetOk() {
		t.Fatalf("second cancel: res=%v err=%v, want ok=false no error", res, err)
	}
}

func TestSortByCreatedAtDesc(t *testing.T) {
	items := []*wsv1.SandboxInfo{
		{Name: "a", CreatedAt: 100},
		{Name: "b", CreatedAt: 300},
		{Name: "c", CreatedAt: 200},
		{Name: "d", CreatedAt: 200}, // tie keeps incoming order (stable)
	}
	sortByCreatedAtDesc(items)
	got := []string{items[0].GetName(), items[1].GetName(), items[2].GetName(), items[3].GetName()}
	want := []string{"b", "c", "d", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSessionSandboxesOrdering(t *testing.T) {
	mk := func(name, phase string, ready bool, created int64) sandboxmgr.Sandbox {
		return sandboxmgr.Sandbox{Name: name, Phase: phase, Ready: ready, CreatedAt: created, Session: "s"}
	}
	// A Running sandbox is the representative even if an older Failed one exists;
	// the rest follow newest-first.
	refs := sessionSandboxes([]sandboxmgr.Sandbox{
		mk("failed-new", "Failed", false, 300),
		mk("running", "Running", true, 100),
		mk("pending", "Pending", false, 200),
	}, "s")
	if len(refs) != 3 {
		t.Fatalf("want 3 refs, got %d", len(refs))
	}
	if refs[0].GetName() != "running" || refs[0].GetPhase() != "Running" {
		t.Fatalf("representative = %q/%q, want running/Running", refs[0].GetName(), refs[0].GetPhase())
	}
	// Remaining two: newest-first (failed-new 300, pending 200).
	if refs[1].GetName() != "failed-new" || refs[2].GetName() != "pending" {
		t.Fatalf("tail order = %q,%q", refs[1].GetName(), refs[2].GetName())
	}
	// With no Running/Ready, the newest is representative.
	refs = sessionSandboxes([]sandboxmgr.Sandbox{
		mk("old", "Pending", false, 1),
		mk("new", "Failed", false, 9),
	}, "s")
	if refs[0].GetName() != "new" {
		t.Fatalf("fallback representative = %q, want new", refs[0].GetName())
	}
	// Another session's sandboxes are ignored.
	if got := sessionSandboxes([]sandboxmgr.Sandbox{mk("x", "Running", true, 1)}, "other"); got != nil {
		t.Fatalf("want nil for foreign session, got %v", got)
	}
}

// testHdr builds a connect.Header from key/value pairs (v2 request metadata).
func testHdr(pairs ...string) *connect.Header {
	h := &connect.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}
