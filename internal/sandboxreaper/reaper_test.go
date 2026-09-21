package sandboxreaper

import (
	"testing"
	"time"

	"github.com/abcp-sdk/workspace-gateway/internal/workerclient"
)

func TestIsIdle(t *testing.T) {
	now := time.Now().UnixMilli()
	ttl := 24 * time.Hour
	hours := func(h float64) int64 { return int64(h * float64(time.Hour/time.Millisecond)) }
	cases := []struct {
		name string
		age  int64 // millis
		jobs []workerclient.Job
		idle bool
	}{
		{"fresh pod, no jobs", hours(1), nil, false},
		{"old pod, no jobs", hours(25), nil, true},
		{"old pod, running job", hours(25), []workerclient.Job{{State: "running", StartedAt: now - hours(1)}}, false},
		{"old pod, recent finished job", hours(25), []workerclient.Job{{State: "done", FinishedAt: now - hours(1)}}, false},
		{"old pod, stale finished job", hours(25), []workerclient.Job{{State: "done", FinishedAt: now - hours(30)}}, true},
		{"zero createdAt never idles", 0, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			createdAt := now - c.age
			if c.age == 0 {
				createdAt = 0
			}
			if got := isIdle(createdAt, now, ttl, c.jobs); got != c.idle {
				t.Fatalf("isIdle = %v, want %v", got, c.idle)
			}
		})
	}
}
