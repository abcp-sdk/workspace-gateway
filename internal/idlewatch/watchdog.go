// Package idlewatch re-triggers a session that stopped mid-task after a tool
// call.
//
// Every sweep it walks each tenant's sessions and, for a session that is
//   - IDLE (the agent reports no running turn), AND
//   - whose LATEST message is an assistant step ending on a `tool_result`
//     (the model executed a tool and then produced no text — it stopped),
//   - and whose last turn was NOT ended by a user interrupt,
//
// publishes a `trigger` into the session's mailbox (source `system:idlewatch`)
// telling the model to continue or wrap up.
//
// The interrupt signal comes from the agent's turn-END marker (the
// `abc-session-turn` KV bucket, keyed tenant+session token): the agent writes
// `{ reason, finish, tip }` awaited BEFORE emitting the terminal `status:idle`,
// so a reader that sees idle can trust the marker. A MISSING marker (older
// session, agent hard-crash) is treated as "not interrupted" and DOES trigger.
//
// The watchdog only READS the agent (ListSessions/State/ListMessages) + the KV
// and publishes onto NATS; it never mutates a session itself.
package idlewatch

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"connectrpc.com/connect"

	abcagent "github.com/abcp-sdk/abc-protocol-go/v2/agent"
	"github.com/abcp-sdk/abc-protocol-go/v2/bus"
	"github.com/abcp-sdk/abc-protocol-go/v2/protocol"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	agentv1connect "github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
)

// turnMarkerBucket mirrors the agent's `BUCKET_SESSION_TURN`.
const turnMarkerBucket = "abc-session-turn"

// Watchdog re-triggers stalled sessions.
type Watchdog struct {
	// Agent is the agent's service client (Raw). Tenant-scoped calls set the
	// shared service token + `X-Abc-Tenant` so the transport mints a tenant
	// token; ServiceToken is that shared token.
	Agent        agentv1connect.AgentServiceClient
	Admin        agentv1connect.AdminServiceClient
	AdminToken   string
	ServiceToken string
	// Bus publishes the mailbox trigger and reads the turn-end marker + the
	// session's effective locale from KV.
	Bus bus.Bus

	// Interval is the sweep period. Default 2m.
	Interval time.Duration

	// now is injectable for tests.
	now func() time.Time
}

// Run sweeps until ctx is cancelled.
func (w *Watchdog) Run(ctx context.Context) {
	if w.Agent == nil || w.Admin == nil || w.Bus == nil || w.ServiceToken == "" || w.AdminToken == "" {
		log.Printf("idlewatch: disabled (missing agent/admin/nats wiring)")
		return
	}
	interval := w.Interval
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	if w.now == nil {
		w.now = time.Now
	}
	log.Printf("idlewatch: interval=%s", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.SweepOnce(ctx)
		}
	}
}

// SweepOnce performs one pass over every tenant's sessions.
func (w *Watchdog) SweepOnce(ctx context.Context) {
	if w.now == nil {
		w.now = time.Now
	}
	tenants, err := w.listTenants(ctx)
	if err != nil {
		log.Printf("idlewatch: list tenants: %v", err)
		return
	}
	for _, tenant := range tenants {
		sessions, err := w.listSessions(ctx, tenant)
		if err != nil {
			log.Printf("idlewatch: list sessions %s: %v", tenant, err)
			continue
		}
		for _, s := range sessions.GetSessions() {
			name := s.GetName()
			if name == "" {
				continue
			}
			w.maybeNudge(ctx, tenant, s)
		}
	}
}

// maybeNudge applies the idle + ends-on-tool-result + not-interrupted rules to
// one session.
func (w *Watchdog) maybeNudge(ctx context.Context, tenant string, s *agentv1.Session) {
	name := s.GetName()
	// IDLE: no running turn. Prefer the status carried on the list row; fall
	// back to a State RPC only when it is absent (e.g. a single-session reply).
	status := s.GetStatus()
	if status == "" {
		st, err := w.state(ctx, tenant, name)
		if err != nil {
			return
		}
		status = statusOf(st)
	}
	if status != "idle" {
		return
	}
	// The LATEST message must be an assistant step that ends on a tool_result:
	// the model ran a tool and then stopped without producing text.
	msgs, err := w.listMessages(ctx, tenant, name)
	if err != nil {
		return
	}
	if !endsOnToolResult(msgs.GetMessages()) {
		return
	}
	// A user interrupt must NOT be re-triggered. A missing/unreadable marker is
	// treated as "not interrupted" (trigger).
	if w.turnInterrupted(ctx, tenant, name) {
		return
	}
	locale := w.effectiveLocale(ctx, tenant, name, s.GetLocale())
	if err := w.publish(ctx, tenant, name, nudgeText(locale)); err != nil {
		log.Printf("idlewatch: publish %s/%s: %v", tenant, name, err)
		return
	}
	log.Printf("idlewatch: re-triggered %s/%s (stopped after a tool call)", tenant, name)
}

// turnInterrupted reports whether the session's LAST turn was ended by a user
// interrupt, per the agent's `abc-session-turn` marker. A missing marker or any
// read error is treated as "not interrupted" (false).
func (w *Watchdog) turnInterrupted(ctx context.Context, tenant, session string) bool {
	key := protocol.TenantKVKey(tenant, protocol.SessionToken(session))
	raw, err := w.Bus.KVGet(ctx, turnMarkerBucket, key)
	if err != nil || raw == "" {
		return false
	}
	var marker struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return false
	}
	return marker.Reason == "interrupted"
}

// ---- agent reads (tenant-scoped) ----

func (w *Watchdog) listTenants(ctx context.Context) ([]string, error) {
	req := connect.NewRequest(&agentv1.ListTenantsRequest{})
	req.Header().Set("Authorization", "Bearer "+w.AdminToken)
	res, err := w.Admin.ListTenants(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Msg.GetTenants()))
	for _, t := range res.Msg.GetTenants() {
		if t.GetId() != "" {
			out = append(out, t.GetId())
		}
	}
	return out, nil
}

func (w *Watchdog) listSessions(ctx context.Context, tenant string) (*agentv1.ListSessionsResponse, error) {
	req := connect.NewRequest(&agentv1.ListSessionsRequest{})
	w.scope(req.Header(), tenant)
	res, err := w.Agent.ListSessions(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (w *Watchdog) state(ctx context.Context, tenant, session string) (*agentv1.StateResponse, error) {
	req := connect.NewRequest(&agentv1.StateRequest{Id: session})
	w.scope(req.Header(), tenant)
	res, err := w.Agent.State(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (w *Watchdog) listMessages(ctx context.Context, tenant, session string) (*agentv1.ListMessagesResponse, error) {
	// Only the tip is needed: `limit:1` walks one message back from the tip.
	req := connect.NewRequest(&agentv1.ListMessagesRequest{Id: session, Limit: 1})
	w.scope(req.Header(), tenant)
	res, err := w.Agent.ListMessages(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// scope authenticates a tenant-scoped call: the shared service token + the
// tenant header, which the agentclient transport rewrites into a tenant token.
func (w *Watchdog) scope(h interface{ Set(string, string) }, tenant string) {
	h.Set("Authorization", "Bearer "+w.ServiceToken)
	h.Set("X-Abc-Tenant", tenant)
}

// effectiveLocale prefers the session's projected `vars.agent.locale` (the
// agent's effective locale for the last turn), then the session row, then "en".
func (w *Watchdog) effectiveLocale(ctx context.Context, tenant, session, sessionLocale string) string {
	if w.Bus != nil {
		key := protocol.SessionVarKey(tenant, "agent", session, "locale")
		if v, err := w.Bus.KVGet(ctx, protocol.VarsBucket, key); err == nil && v != "" {
			return v
		}
	}
	if sessionLocale != "" {
		return sessionLocale
	}
	return "en"
}

func (w *Watchdog) publish(ctx context.Context, tenant, session, text string) error {
	return abcagent.New(w.Bus).PublishMailbox(
		ctx, tenant, session, "trigger",
		map[string]any{"text": text},
		"system:idlewatch",
	)
}

// ---- pure rules (unit-tested) ----

// endsOnToolResult reports whether the newest message is an assistant step
// whose LAST part is a `tool_result`. That is the "ran a tool then stopped"
// shape: a normal finish produces a trailing text part.
func endsOnToolResult(msgs []*agentv1.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	// The chain is oldest→newest; the tip is the last row.
	last := msgs[len(msgs)-1]
	if last.GetRole() != "assistant" {
		return false
	}
	parts := last.GetParts()
	if len(parts) == 0 {
		return false
	}
	return parts[len(parts)-1].GetType() == "tool_result"
}

// statusOf reads the `status` field of a State response.
func statusOf(st *agentv1.StateResponse) string {
	if st == nil || st.GetState() == nil {
		return ""
	}
	return st.GetState().GetFields()["status"].GetStringValue()
}

// nudgeText is the mailbox body, localized by the session's effective locale.
func nudgeText(locale string) string {
	if strings.HasPrefix(strings.ToLower(locale), "zh") {
		return "你在一次工具调用之后停止了，没有继续。请继续完成你的任务；若已完成，请用文本说明结果并收尾。"
	}
	return "You stopped after a tool call without continuing. Please carry on with your task; if you are done, respond with a text summary to finish."
}
