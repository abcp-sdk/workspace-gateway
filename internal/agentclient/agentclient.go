// Package agentclient is a thin h2c Connect client over the real agent. The
// gateway uses it to forward the minimal agent surface it exposes (chat,
// models, config, providers, files) and to run the trusted session-creation
// calls (CreateSession/Fork) that only the gateway may make.
package agentclient

import (
	"net/http"
	"strings"

	agentv1connect "github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
)

// Client wraps the generated agent client.
type Client struct {
	c agentv1connect.AgentServiceClient
}

// New dials the agent URL over h2c (the agent serves unencrypted HTTP/2 ONLY).
// HTTP/1.1 MUST be disabled: the agent's "h2c" server mode refuses HTTP/1.1, so
// enabling it makes Go send an HTTP/1.1 request that the agent rejects (the
// forwarded RPC then fails with 503).
func New(agentURL string) *Client {
	p := new(http.Protocols)
	p.SetHTTP1(false)
	p.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{Protocols: p}}
	return &Client{c: agentv1connect.NewAgentServiceClient(hc, trimSlash(agentURL))}
}

// Raw exposes the generated client (the workspace service forwards with it).
func (c *Client) Raw() agentv1connect.AgentServiceClient { return c.c }

func trimSlash(s string) string {
	return strings.TrimRight(s, "/")
}
