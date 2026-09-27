package idlewatch

import (
	"context"
	"testing"

	"github.com/abcp-sdk/abc-protocol-go/v2/bus"
	"github.com/abcp-sdk/abc-protocol-go/v2/protocol"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
)

func msg(role string, parts ...*agentv1.Part) *agentv1.Message {
	return &agentv1.Message{Role: role, Parts: parts}
}

func part(typ string) *agentv1.Part { return &agentv1.Part{Type: typ} }

func TestEndsOnToolResult(t *testing.T) {
	cases := []struct {
		name string
		msgs []*agentv1.Message
		want bool
	}{
		{
			name: "assistant step ending on tool_result triggers",
			msgs: []*agentv1.Message{msg("assistant", part("tool"), part("tool_result"))},
			want: true,
		},
		{
			name: "assistant step with trailing text does NOT trigger",
			msgs: []*agentv1.Message{msg("assistant", part("tool"), part("tool_result"), part("text"))},
			want: false,
		},
		{
			name: "user message does NOT trigger",
			msgs: []*agentv1.Message{msg("user", part("text"))},
			want: false,
		},
		{
			name: "event message does NOT trigger",
			msgs: []*agentv1.Message{msg("event", part("text"))},
			want: false,
		},
		{
			name: "assistant text-only step does NOT trigger",
			msgs: []*agentv1.Message{msg("assistant", part("text"))},
			want: false,
		},
		{
			name: "tip is what matters, not an earlier message",
			msgs: []*agentv1.Message{
				msg("assistant", part("tool"), part("tool_result")),
				msg("assistant", part("text")),
			},
			want: false,
		},
		{name: "no messages does NOT trigger", msgs: nil, want: false},
	}
	for _, tc := range cases {
		if got := endsOnToolResult(tc.msgs); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
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

// fakeBus implements just the KV read the interrupt check needs.
type fakeBus struct {
	bus.Bus
	vals map[string]string
}

func (f *fakeBus) KVGet(_ context.Context, bucket, key string) (string, error) {
	return f.vals[bucket+"\x00"+key], nil
}

func TestTurnInterrupted(t *testing.T) {
	w := &Watchdog{Bus: &fakeBus{vals: map[string]string{}}}
	// Missing marker -> not interrupted.
	if w.turnInterrupted(context.Background(), "t", "s") {
		t.Fatal("missing marker must be treated as not interrupted")
	}
	// reason=interrupted -> true.
	key := "abc-session-turn\x00" + protocol.TenantKVKey("t", protocol.SessionToken("s"))
	w = &Watchdog{Bus: &fakeBus{vals: map[string]string{key: `{"reason":"interrupted"}`}}}
	if !w.turnInterrupted(context.Background(), "t", "s") {
		t.Fatal("reason=interrupted must be detected")
	}
	// reason=stop -> false.
	w = &Watchdog{Bus: &fakeBus{vals: map[string]string{key: `{"reason":"stop"}`}}}
	if w.turnInterrupted(context.Background(), "t", "s") {
		t.Fatal("reason=stop must not be interrupted")
	}
}
