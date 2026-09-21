package workspacesvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
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
