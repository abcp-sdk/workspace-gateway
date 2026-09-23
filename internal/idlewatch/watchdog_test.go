package idlewatch

import (
	"testing"
	"time"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
)

func msgWith(parts ...*agentv1.Part) *agentv1.Message {
	return &agentv1.Message{Parts: parts}
}

func toolPart(data string) *agentv1.Part {
	return &agentv1.Part{Type: "tool", Data: data}
}

func TestLatestTodosPicksNewestTodoWrite(t *testing.T) {
	msgs := []*agentv1.Message{
		msgWith(toolPart(`{"name":"todo-write","input":{"todos":[{"content":"a","status":"pending","priority":"high"}]}}`)),
		msgWith(toolPart(`{"name":"repo-read","input":{"path":"x"}}`)),
		msgWith(toolPart(`{"name":"todo-write","input":{"todos":[{"content":"a","status":"completed","priority":"high"},{"content":"b","status":"in_progress","priority":"high"}]}}`)),
	}
	todos := latestTodos(msgs)
	if len(todos) != 2 {
		t.Fatalf("todos = %v, want 2", todos)
	}
	if total, remaining := countTodos(todos); total != 2 || remaining != 1 {
		t.Fatalf("counts = %d/%d, want total 2 remaining 1", total, remaining)
	}
}

func TestLatestTodosQualifiedAndBare(t *testing.T) {
	for _, name := range []string{"todo-write", "todowrite", "bundled.todo-write"} {
		msgs := []*agentv1.Message{
			msgWith(toolPart(`{"name":"` + name + `","input":{"todos":[{"content":"x","status":"pending"}]}}`)),
		}
		if got := latestTodos(msgs); len(got) != 1 {
			t.Fatalf("%s: todos = %v, want 1", name, got)
		}
	}
}

func TestLatestTodosNone(t *testing.T) {
	msgs := []*agentv1.Message{
		msgWith(toolPart(`{"name":"repo-read","input":{}}`)),
		msgWith(&agentv1.Part{Type: "reasoning", Data: `{"text":"hi"}`}),
	}
	if got := latestTodos(msgs); got != nil {
		t.Fatalf("todos = %v, want nil", got)
	}
}

func TestCountTodosCancelledCountsAsDone(t *testing.T) {
	todos := []todo{
		{Status: "completed"},
		{Status: "cancelled"},
		{Status: "pending"},
		{Status: "in_progress"},
	}
	if total, remaining := countTodos(todos); total != 4 || remaining != 2 {
		t.Fatalf("counts = %d/%d, want total 4 remaining 2", total, remaining)
	}
}

func TestParseTime(t *testing.T) {
	if _, ok := parseTime(""); ok {
		t.Fatal("empty must not parse")
	}
	if _, ok := parseTime("not-a-time"); ok {
		t.Fatal("garbage must not parse")
	}
	if _, ok := parseTime("2026-09-22T16:32:14.049Z"); !ok {
		t.Fatal("RFC3339Nano must parse")
	}
}

func TestNudgeTextLocale(t *testing.T) {
	zh := nudgeText("zh")
	if zh == "" || nudgeText("zh-CN") != zh {
		t.Fatal("zh / zh-CN must share the Chinese text")
	}
	if nudgeText("en") == zh {
		t.Fatal("en must differ from zh")
	}
	if nudgeText("") == zh {
		t.Fatal("unknown locale must fall back to English")
	}
}

func TestCoolingDown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := &Watchdog{Cooldown: 30 * time.Minute, now: func() time.Time { return now }, nudged: map[string]time.Time{}}
	if w.coolingDown("t", "s") {
		t.Fatal("never-nudged must not be cooling down")
	}
	w.nudged["t/s"] = now.Add(-10 * time.Minute)
	if !w.coolingDown("t", "s") {
		t.Fatal("nudged 10m ago with 30m cooldown must be cooling down")
	}
	w.nudged["t/s"] = now.Add(-40 * time.Minute)
	if w.coolingDown("t", "s") {
		t.Fatal("nudged 40m ago with 30m cooldown must be ready")
	}
}
