package shellh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	ws := t.TempDir()
	return New(ws, []string{"PATH=/usr/bin:/bin", "HOME=" + ws})
}

func TestRunBasics(t *testing.T) {
	r := newTestRunner(t)

	cases := []struct {
		name    string
		command string
		wantOut string
		wantEC  int
	}{
		{"echo", "echo hello", "hello", 0},
		{"pipe", "printf 'b\\na\\nc\\n' | sort | head -1", "a", 0},
		{"seq", "true && echo ok", "ok", 0},
		{"fail", "exit 3", "", 3},
		{"subshell", "out=$(echo nested) && echo $out", "nested", 0},
		{"var", "x=world; echo hi-$x", "hi-world", 0},
		{"forloop", "for i in 1 2 3; do printf '%s,' $i; done", "1,2,3,", 0},
		{"redir", "echo tofile > out.txt && cat out.txt", "tofile", 0},
		{"missing", "nonexistent-cmd-xyz", "", 127},
		{"stderr", "echo err >&2", "", 0},
		{"cmdsubst", "echo count:$(printf 'x\\ny\\nz\\n' | wc -l | tr -d ' ')", "count:3", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			res, err := r.Run(context.Background(), tc.command, "", strings.NewReader(""), &stdout, &stderr)
			if err != nil {
				t.Fatalf("run error: %v", err)
			}
			if res.ExitCode != tc.wantEC {
				t.Errorf("exit = %d want %d (stderr: %q)", res.ExitCode, tc.wantEC, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != tc.wantOut {
				t.Errorf("stdout = %q want %q", got, tc.wantOut)
			}
		})
	}
}

// The runner must NOT inherit the worker's own environment (platform tokens
// must stay invisible to jobs).
func TestEnvIsolation(t *testing.T) {
	r := newTestRunner(t)
	t.Setenv("EASYWORKER_SECRET", "leak-me")

	var stdout bytes.Buffer
	res, err := r.Run(context.Background(), "echo secret=${EASYWORKER_SECRET:-unset}", "", strings.NewReader(""), &stdout, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "secret=unset" {
		t.Errorf("env leaked: %q", got)
	}
}

func TestWorkdirRelative(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(ws, []string{"PATH=/usr/bin:/bin"})

	var stdout bytes.Buffer
	_, err := r.Run(context.Background(), "pwd", "sub", strings.NewReader(""), &stdout, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(resOf(stdout.String()))
	if !strings.HasSuffix(got, "sub") {
		t.Errorf("pwd = %q, want suffix sub", got)
	}
}

func resOf(s string) string { return s }

func TestKillCancels(t *testing.T) {
	r := newTestRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	var stdout bytes.Buffer
	go func() {
		_, _ = r.Run(ctx, "sleep 30 && echo done", "", strings.NewReader(""), &stdout, nil)
	}()
	cancel() // kill immediately; the job must not hang the test
}

func TestLineBufferCRLFAndRing(t *testing.T) {
	var seq atomic.Int64
	next := func() int64 { return seq.Add(1) }

	var seen []LineRec
	lb := NewLineBuffer(3, next, func(r LineRec) { seen = append(seen, r) })

	if _, err := lb.Write([]byte("a\r\nb\nc\nd")); err != nil {
		t.Fatal(err)
	}
	got := lb.Recs()
	// 3 completed lines fit exactly in max=3
	if len(got) != 3 || got[0].Line != "a" || got[2].Line != "c" {
		t.Errorf("recs = %v", got)
	}
	if lb.Dropped() != 0 {
		t.Errorf("dropped = %d want 0", lb.Dropped())
	}
	// callbacks fired for every completed line
	if len(seen) != 3 || seen[0].Line != "a" || seen[2].Line != "c" {
		t.Errorf("onLine = %v", seen)
	}
	if s := lb.Tail(3); s != "b\nc\nd" { // last 3 entries incl. the partial line
		t.Errorf("tail = %q", s)
	}
	// FlushLine promotes the partial "d": ring overflows, "a" drops.
	if s := lb.FlushLine(); s != "d" {
		t.Errorf("flush = %q", s)
	}
	if s := lb.FlushLine(); s != "" {
		t.Errorf("second flush = %q", s)
	}
	if got := lb.Recs(); len(got) != 3 || got[0].Line != "b" || got[2].Line != "d" {
		t.Errorf("recs after flush = %v", got)
	}
	if lb.Dropped() != 1 {
		t.Errorf("dropped after flush = %d want 1", lb.Dropped())
	}
	// seq assignment is monotonic across flushes
	recs := lb.Recs()
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq <= recs[i-1].Seq {
			t.Errorf("seq not monotonic: %v", recs)
		}
	}
}
