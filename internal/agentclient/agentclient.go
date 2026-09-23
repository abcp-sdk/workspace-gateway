// Package agentclient is a thin h2c Connect client over the real agent. The
// gateway uses it to forward the minimal agent surface it exposes (chat,
// models, config, providers, files) and to run the trusted session-creation
// calls (CreateSession/Fork) that only the gateway may make.
//
// Credential model — the gateway is the ONLY trusted caller of the agent:
//
//   - A browser request carries a tenant token; it is forwarded to the agent
//     VERBATIM (the agent authenticates it directly).
//   - The workspace extension carries the shared SERVICE token and names the
//     real tenant via `X-Abc-Tenant`. The agent does not know that token, so
//     the gateway MINTS a tenant token for the named tenant (AdminService,
//     using the static admin token) and presents THAT to the agent. The
//     service token and internal header never cross into the agent.
//
// The rewrite happens in an http.RoundTripper, so every call — unary and every
// streaming shape — is covered at one choke point.
package agentclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	agentv1connect "github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
)

// Config configures the agent client.
type Config struct {
	// URL is the agent base URL (h2c, no trailing slash needed).
	URL string
	// ServiceToken is the shared token the workspace extension presents to the
	// gateway. When empty the rewrite is disabled (browser-only deployments).
	ServiceToken string
	// AdminToken is the static admin token used to mint tenant tokens. Required
	// only when ServiceToken is set.
	AdminToken string
	// TokenTTL bounds how long a minted tenant token is cached. Zero = 10m.
	TokenTTL time.Duration
}

// Client wraps the generated agent clients + the credential-rewriting transport.
type Client struct {
	c        agentv1connect.AgentServiceClient
	admin    agentv1connect.AdminServiceClient
	svcToken string
}

// New dials the agent URL over h2c (the agent serves unencrypted HTTP/2 ONLY).
// HTTP/1.1 MUST be disabled: the agent's "h2c" server mode refuses HTTP/1.1, so
// enabling it makes Go send an HTTP/1.1 request that the agent rejects (the
// forwarded RPC then fails with 503).
func New(cfg Config) *Client {
	p := new(http.Protocols)
	p.SetHTTP1(false)
	p.SetUnencryptedHTTP2(true)
	base := &http.Transport{Protocols: p}

	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	rt := &authTransport{
		base:     base,
		svcToken: cfg.ServiceToken,
		broker: &broker{
			adminToken: cfg.AdminToken,
			ttl:        ttl,
			tokens:     map[string]tokenEntry{},
		},
	}
	hc := &http.Client{Transport: rt}
	client := &Client{
		c:        agentv1connect.NewAgentServiceClient(hc, trimSlash(cfg.URL)),
		admin:    agentv1connect.NewAdminServiceClient(hc, trimSlash(cfg.URL)),
		svcToken: cfg.ServiceToken,
	}
	rt.broker.admin = client.admin
	return client
}

// Raw exposes the generated agent client (the workspace service forwards with it).
func (c *Client) Raw() agentv1connect.AgentServiceClient { return c.c }

// Admin exposes the generated admin client (tenant enumeration / token minting).
func (c *Client) Admin() agentv1connect.AdminServiceClient { return c.admin }

// ServiceToken returns the shared service token ("" when disabled). Callers
// that must act for a named tenant set it as the bearer + `X-Abc-Tenant`.
func (c *Client) ServiceToken() string { return c.svcToken }

func trimSlash(s string) string {
	return strings.TrimRight(s, "/")
}

// ---- credential rewriting ----

// authTransport rewrites the outbound Authorization of agent-bound requests:
// a SERVICE-token bearer naming a tenant (`X-Abc-Tenant`) is replaced by a
// freshly minted tenant token. Any other request (a browser tenant token) is
// passed through untouched.
type authTransport struct {
	base     http.RoundTripper
	svcToken string
	broker   *broker
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tenant := t.serviceTenant(req)
	if tenant == "" {
		return t.base.RoundTrip(req)
	}
	ctx := req.Context()
	tok, err := t.broker.token(ctx, tenant)
	if err != nil {
		return nil, err
	}
	res, err := t.base.RoundTrip(withCredential(req, tok))
	if err != nil {
		return nil, err
	}
	// A minted token can be revoked server-side before its TTL expires. On
	// Unauthenticated, drop the cache entry, mint once more and replay when the
	// body can be re-read (GetBody is set for unary and server-streaming calls).
	if res.StatusCode == http.StatusUnauthorized && req.GetBody != nil {
		_ = res.Body.Close()
		t.broker.invalidate(tenant)
		body, berr := req.GetBody()
		if berr != nil {
			return nil, berr
		}
		tok2, terr := t.broker.token(ctx, tenant)
		if terr != nil {
			return nil, terr
		}
		replay := withCredential(req, tok2)
		replay.Body = body
		return t.base.RoundTrip(replay)
	}
	return res, nil
}

// serviceTenant reports the tenant a service-token request acts for, or "" when
// the request is not a service-token request (or names no valid tenant).
func (t *authTransport) serviceTenant(req *http.Request) string {
	if t.svcToken == "" {
		return ""
	}
	if req.Header.Get("Authorization") != "Bearer "+t.svcToken {
		return ""
	}
	if tenant := req.Header.Get("X-Abc-Tenant"); validTenant(tenant) {
		return tenant
	}
	return ""
}

// withCredential clones req and swaps its Authorization for the minted token,
// dropping the gateway-internal tenant header so it never reaches the agent.
func withCredential(req *http.Request, token string) *http.Request {
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+token)
	out.Header.Del("X-Abc-Tenant")
	return out
}

// validTenant mirrors the agent's tenant charset ^[A-Za-z0-9_-]{1,64}$.
func validTenant(t string) bool {
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

// ---- tenant token broker ----

type tokenEntry struct {
	token string
	exp   time.Time
}

// broker mints + caches tenant tokens via AdminService.IssueTenantToken. The
// agent rejects issue for an unknown tenant (NotFound), which is exactly the
// "a caller may only act for a real tenant" rule.
type broker struct {
	admin      agentv1connect.AdminServiceClient
	adminToken string
	ttl        time.Duration

	mu     sync.Mutex
	tokens map[string]tokenEntry
}

func (b *broker) token(ctx context.Context, tenant string) (string, error) {
	if b.adminToken == "" {
		return "", errors.New("agentclient: GATEWAY_SERVICE_TOKEN is set but AGENT_ADMIN_TOKEN is empty; cannot mint a tenant token")
	}
	now := time.Now()
	ttl := b.ttl
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	b.mu.Lock()
	if e, ok := b.tokens[tenant]; ok && now.Before(e.exp) {
		tok := e.token
		b.mu.Unlock()
		return tok, nil
	}
	b.mu.Unlock()

	req := connect.NewRequest(&agentv1.IssueTenantTokenRequest{
		TenantId: tenant,
		Label:    "workspace-gateway",
	})
	req.Header().Set("Authorization", "Bearer "+b.adminToken)
	res, err := b.admin.IssueTenantToken(ctx, req)
	if err != nil {
		return "", err
	}
	tok := res.Msg.GetPlaintext()
	if tok == "" {
		return "", errors.New("agentclient: agent returned an empty tenant token")
	}
	b.mu.Lock()
	b.tokens[tenant] = tokenEntry{token: tok, exp: now.Add(ttl)}
	b.mu.Unlock()
	return tok, nil
}

func (b *broker) invalidate(tenant string) {
	b.mu.Lock()
	delete(b.tokens, tenant)
	b.mu.Unlock()
}
