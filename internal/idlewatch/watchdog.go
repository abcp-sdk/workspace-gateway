// Package idlewatch nudges a session that has stopped working while its todo
// list is still unfinished.
//
// Every sweep it walks each tenant's sessions and, for a session that is
//   - IDLE (the agent reports no running turn), and
//   - QUIET (its last message is older than IdleAfter),
//
// inspects the session's most recent `todo-write` call. When that list has a
// todo that is neither completed nor cancelled, it publishes a `trigger` into
// the session's mailbox (source `system:idlewatch`) asking the agent to either
// mark the work done or keep going.
//
// The watchdog only READS the agent (ListSessions/State/ListMessages) and
// publishes onto NATS; it never mutates a session itself.
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

// Watchdog periodically nudges stalled sessions.
type Watchdog struct {
	// Agent is the agent's service client (Raw). Tenant-scoped calls set the
	// shared service token + `X-Abc-Tenant` so the transport mints a tenant
	// token; ServiceToken is that shared token.
	Agent        agentv1connect.AgentServiceClient
	Admin        agentv1connect.AdminServiceClient
	AdminToken   string
	ServiceToken string
	// Bus publishes the mailbox trigger and reads the session's effective
	// locale from the vars KV.
	Bus bus.Bus

	// IdleAfter is how long a session must have been quiet (no new message)
	// before it is nudged. Default 1h.
	IdleAfter time.Duration
	// Interval is the sweep period. Default 5m.
	Interval time.Duration
	// Cooldown suppresses a repeat nudge for the same session. Default 30m.
	Cooldown time.Duration

	// now is injectable for tests.
	now func() time.Time
	// nudged is the in-memory cooldown ledger (session -> last nudge).
	nudged map[string]time.Time
}

// Run sweeps until ctx is cancelled.
func (w *Watchdog) Run(ctx context.Context) {
	if w.Agent == nil || w.Admin == nil || w.Bus == nil || w.ServiceToken == "" || w.AdminToken == "" {
		log.Printf("idlewatch: disabled (missing agent/admin/nats wiring)")
		return
	}
	if w.IdleAfter <= 0 {
		w.IdleAfter = time.Hour
	}
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if w.Cooldown <= 0 {
		w.Cooldown = 30 * time.Minute
	}
	if w.nudged == nil {
		w.nudged = map[string]time.Time{}
	}
	if w.now == nil {
		w.now = time.Now
	}
	log.Printf("idlewatch: idle-after=%s interval=%s cooldown=%s", w.IdleAfter, interval, w.Cooldown)
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
	if w.nudged == nil {
		w.nudged = map[string]time.Time{}
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

// maybeNudge applies the idle/quiet/unfinished rules to one session.
func (w *Watchdog) maybeNudge(ctx context.Context, tenant string, s *agentv1.Session) {
	name := s.GetName()
	if w.coolingDown(tenant, name) {
		return
	}
	// QUIET: the last message must be older than IdleAfter.
	last, ok := parseTime(s.GetLastMessageAt())
	if !ok || w.now().Sub(last) < w.IdleAfter {
		return
	}
	// IDLE: no running turn.
	st, err := w.state(ctx, tenant, name)
	if err != nil {
		return
	}
	if statusOf(st) != "idle" {
		return
	}
	// UNFINISHED: the latest todo-write must have a live todo.
	msgs, err := w.listMessages(ctx, tenant, name)
	if err != nil {
		return
	}
	todos := latestTodos(msgs.GetMessages())
	_, remaining := countTodos(todos)
	if remaining == 0 {
		return
	}
	locale := w.effectiveLocale(ctx, tenant, name, s.GetLocale())
	if err := w.publish(ctx, tenant, name, nudgeText(locale)); err != nil {
		log.Printf("idlewatch: publish %s/%s: %v", tenant, name, err)
		return
	}
	w.nudged[tenant+"/"+name] = w.now()
	log.Printf("idlewatch: nudged %s/%s (remaining=%d, last_message=%s ago)", tenant, name, remaining, w.now().Sub(last).Truncate(time.Second))
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
	req := connect.NewRequest(&agentv1.ListMessagesRequest{Id: session, Limit: 100})
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

func (w *Watchdog) coolingDown(tenant, session string) bool {
	last, ok := w.nudged[tenant+"/"+session]
	return ok && w.now().Sub(last) < w.Cooldown
}

// ---- pure rules (unit-tested) ----

type todo struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// latestTodos returns the todo list from the session's NEWEST `todo-write`
// tool part, or nil when the session has none. `todo-write` replaces the whole
// list each call, so the most recent one IS the current list.
func latestTodos(msgs []*agentv1.Message) []todo {
	for i := len(msgs) - 1; i >= 0; i-- {
		parts := msgs[i].GetParts()
		for j := len(parts) - 1; j >= 0; j-- {
			p := parts[j]
			if p.GetType() != "tool" {
				continue
			}
			var payload struct {
				Name  string `json:"name"`
				Input struct {
					Todos *[]todo `json:"todos"`
				} `json:"input"`
			}
			if err := json.Unmarshal([]byte(p.GetData()), &payload); err != nil {
				continue
			}
			if !isTodoWrite(payload.Name) || payload.Input.Todos == nil {
				continue
			}
			return *payload.Input.Todos
		}
	}
	return nil
}

// isTodoWrite matches the bundled todo tool, bare or extension-qualified.
func isTodoWrite(name string) bool {
	return name == "todo-write" || name == "todowrite" || strings.HasSuffix(name, ".todo-write")
}

// countTodos returns (total, remaining); a completed OR cancelled todo counts
// as done.
func countTodos(todos []todo) (total, remaining int) {
	for _, t := range todos {
		total++
		if t.Status != "completed" && t.Status != "cancelled" {
			remaining++
		}
	}
	return total, remaining
}

// statusOf reads the `status` field of a State response.
func statusOf(st *agentv1.StateResponse) string {
	if st == nil || st.GetState() == nil {
		return ""
	}
	return st.GetState().GetFields()["status"].GetStringValue()
}

// parseTime parses an RFC3339 timestamp (empty / malformed => false).
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// nudgeText is the mailbox body, localized by the session's effective locale.
func nudgeText(locale string) string {
	if strings.HasPrefix(strings.ToLower(locale), "zh") {
		return "发现你已经停止工作了，但你还有未完成的代办。如果你已经完成了任务，请把代办更新为已完成；否则请继续执行你的任务！"
	}
	return "You appear to have stopped working, but you still have unfinished todos. If you have completed the task, mark the todos as completed; otherwise continue carrying out your task!"
}
