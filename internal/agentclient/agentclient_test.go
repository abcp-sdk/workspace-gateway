package agentclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
)

// stubAdmin implements only IssueTenantToken; it records the tenants asked for.
type stubAdmin struct {
	agentv1connect.AdminServiceClient
	minted   []string
	token    string
	failWith error
}

func (a *stubAdmin) IssueTenantToken(_ context.Context, req *connect.Request[agentv1.IssueTenantTokenRequest]) (*connect.Response[agentv1.IssueTenantTokenResponse], error) {
	if a.failWith != nil {
		return nil, a.failWith
	}
	a.minted = append(a.minted, req.Msg.GetTenantId())
	return connect.NewResponse(&agentv1.IssueTenantTokenResponse{
		Plaintext: a.token,
	}), nil
}

// capture records the headers of the request it was handed and returns 200.
type capture struct {
	got     http.Header
	status  int
	replays int
}

func (c *capture) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req.Header.Clone()
	c.replays++
	return &http.Response{
		StatusCode: c.status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func newTransport(admin *stubAdmin) (*authTransport, *capture) {
	cap := &capture{status: http.StatusOK}
	return &authTransport{
		base:     cap,
		svcToken: "svc-secret",
		broker:   &broker{admin: admin, adminToken: "admin-secret", tokens: map[string]tokenEntry{}},
	}, cap
}

func TestServiceTokenMintsAndSwaps(t *testing.T) {
	admin := &stubAdmin{token: "minted-tenant-token"}
	rt, cap := newTransport(admin)

	req, _ := http.NewRequest(http.MethodPost, "http://agent/agent.v1.AgentService/CreateSession", nil)
	req.Header.Set("Authorization", "Bearer svc-secret")
	req.Header.Set("X-Abc-Tenant", "acme")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if got := cap.got.Get("Authorization"); got != "Bearer minted-tenant-token" {
		t.Fatalf("agent saw Authorization %q, want the minted tenant token", got)
	}
	if got := cap.got.Get("X-Abc-Tenant"); got != "" {
		t.Fatalf("X-Abc-Tenant leaked to the agent: %q", got)
	}
	if len(admin.minted) != 1 || admin.minted[0] != "acme" {
		t.Fatalf("minted for %v, want [acme]", admin.minted)
	}

	// A second call for the same tenant reuses the cached token (no re-mint).
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip 2: %v", err)
	}
	if len(admin.minted) != 1 {
		t.Fatalf("minted %d times, want 1 (cached)", len(admin.minted))
	}
}

func TestBrowserTokenPassesThrough(t *testing.T) {
	admin := &stubAdmin{token: "unused"}
	rt, cap := newTransport(admin)

	req, _ := http.NewRequest(http.MethodPost, "http://agent/agent.v1.AgentService/ListSessions", nil)
	req.Header.Set("Authorization", "Bearer browser-tenant-token")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if got := cap.got.Get("Authorization"); got != "Bearer browser-tenant-token" {
		t.Fatalf("Authorization = %q, want the caller's token verbatim", got)
	}
	if len(admin.minted) != 0 {
		t.Fatalf("browser request minted a token: %v", admin.minted)
	}
}

func TestServiceTokenWithoutTenantPassesThrough(t *testing.T) {
	admin := &stubAdmin{token: "unused"}
	rt, cap := newTransport(admin)

	// The service token names no tenant: the gateway must not invent one.
	req, _ := http.NewRequest(http.MethodPost, "http://agent/agent.v1.AgentService/ListSessions", nil)
	req.Header.Set("Authorization", "Bearer svc-secret")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := cap.got.Get("Authorization"); got != "Bearer svc-secret" {
		t.Fatalf("Authorization = %q, want the service token untouched", got)
	}
	if len(admin.minted) != 0 {
		t.Fatalf("minted without a named tenant: %v", admin.minted)
	}
}

func TestRevokedTokenIsRemicedOnce(t *testing.T) {
	admin := &stubAdmin{token: "minted"}
	rt, cap := newTransport(admin)
	cap.status = http.StatusUnauthorized

	body := strings.NewReader(`{"x":1}`)
	req, _ := http.NewRequest(http.MethodPost, "http://agent/agent.v1.AgentService/CreateSession", body)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"x":1}`)), nil
	}
	req.Header.Set("Authorization", "Bearer svc-secret")
	req.Header.Set("X-Abc-Tenant", "acme")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	// The first 401 invalidates + replays: two mints, two trips.
	if len(admin.minted) != 2 {
		t.Fatalf("minted %d times, want 2 (re-mint after 401)", len(admin.minted))
	}
	if cap.replays != 2 {
		t.Fatalf("replayed %d times, want 2", cap.replays)
	}
}
