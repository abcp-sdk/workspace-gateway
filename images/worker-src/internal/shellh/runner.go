// Package shell implements the builtin shell executor. EVERY command runs
// through the mvdan.cc/sh/v3 interpreter — there is no passthrough to a host
// shell and no fallback path. This gives identical bash semantics on
// linux/windows/macos, including images that ship no shell at all.
package shellh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Runner executes command strings through the builtin interpreter.
type Runner struct {
	// Workspace is the root every relative path resolves against and the
	// default cwd for jobs.
	Workspace string

	// Env is the base environment (already stripped/allowlisted by the
	// caller in main).
	Env []string

	mu     sync.Mutex
	childs map[int]*exec.Cmd
}

func New(workspace string, env []string) *Runner {
	return &Runner{
		Workspace: workspace,
		Env:       env,
		childs:    map[int]*exec.Cmd{},
	}
}

// RunWithEnv is Run with per-job environment additions on top of the base
// env. Additive keys override the base. It does NOT mutate the Runner, so
// concurrent jobs are safe.
func (r *Runner) RunWithEnv(ctx context.Context, command, workdir string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	if len(env) == 0 {
		return r.run(ctx, command, workdir, r.Env, stdin, stdout, stderr)
	}
	merged := append([]string{}, r.Env...)
	for k, v := range env {
		merged = append(merged, k+"="+v)
	}
	return r.run(ctx, command, workdir, merged, stdin, stdout, stderr)
}

// Result is the outcome of one interpreted command run.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Run interprets the command. workdir may be "" (workspace root) or a
// workspace-relative path. It blocks until the command completes or ctx is
// done; on ctx cancellation the process group is killed and the exit code is
// 130 (interrupt-ish), matching the legacy worker behavior.
func (r *Runner) Run(ctx context.Context, command, workdir string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	return r.run(ctx, command, workdir, r.Env, stdin, stdout, stderr)
}

// run is the shared implementation; env is the effective process environment
// (callers pass r.Env, or a per-job merge).
func (r *Runner) run(ctx context.Context, command, workdir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	cwd := r.Workspace
	if workdir != "" {
		if filepath.IsAbs(workdir) {
			// Absolute workdirs are used as-is (callers pin an absolute dir
			// such as a mounted workspace); only relative ones resolve against
			// the workspace root.
			cwd = filepath.Clean(workdir)
		} else {
			cwd = filepath.Join(r.Workspace, filepath.FromSlash(workdir))
		}
	}

	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return Result{}, fmt.Errorf("parse: %w", err)
	}

	runner, err := interp.New(
		interp.Dir(cwd),
		interp.Env(expand.ListEnviron(env...)),
		interp.StdIO(stdin, stdout, stderr),
		interp.ExecHandlers(r.execMiddleware(env)),
		interp.OpenHandler(r.openHandler()),
	)
	if err != nil {
		return Result{}, fmt.Errorf("interp: %w", err)
	}

	runErr := runner.Run(ctx, file)

	res := Result{
		ExitCode: exitCodeOf(runErr, ctx),
		Stdout:   tailOf(stdout, defaultTailLines),
		Stderr:   tailOf(stderr, defaultTailLines),
	}
	return res, nil
}

// tailOf extracts a tail snapshot from a job writer (the LineBuffer keeps a
// ring; other writers fall back to empty tails — history is served from the
// store by the job manager).
func tailOf(w io.Writer, n int) string {
	if lb, ok := w.(*LineBuffer); ok {
		return lb.Tail(n)
	}
	return ""
}

const defaultTailLines = 200

func exitCodeOf(runErr error, ctx context.Context) int {
	switch {
	case runErr == nil:
		return 0
	case ctx.Err() != nil:
		return 130
	default:
		if es, ok := interp.IsExitStatus(runErr); ok {
			return int(es)
		}
		return 127
	}
}

// LineRec is one completed output line with its job-global sequence number
// (assigned at completion, shared across the job's stdout/stderr buffers —
// interleaved order is preserved).
type LineRec struct {
	Seq  int64
	Line string
}

// LineBuffer is an io.Writer that splits into lines (normalizing CRLF) as
// they are written. It is a bounded ring: when full the OLDEST lines are
// dropped (the sqlite store is the unbounded authority; the ring only feeds
// live fanout, replay merges and tails). onLine, when set, fires for every
// completed line (outside the lock).
type LineBuffer struct {
	max     int
	nextSeq func() int64
	onLine  func(LineRec)

	mu      sync.Mutex
	recs    []LineRec
	cur     strings.Builder
	dropped int
}

func NewLineBuffer(max int, nextSeq func() int64, onLine func(LineRec)) *LineBuffer {
	if max <= 0 {
		max = 10000
	}
	return &LineBuffer{max: max, nextSeq: nextSeq, onLine: onLine}
}

func (lb *LineBuffer) Write(p []byte) (int, error) {
	var completed []LineRec
	lb.mu.Lock()
	for _, b := range p {
		switch b {
		case '\r':
			continue // CRLF collapses; lone CR dropped (terminal-like)
		case '\n':
			rec := LineRec{Seq: lb.nextSeq(), Line: lb.cur.String()}
			lb.cur.Reset()
			lb.recs = append(lb.recs, rec)
			if len(lb.recs) > lb.max {
				lb.recs = lb.recs[1:]
				lb.dropped++
			}
			completed = append(completed, rec)
		default:
			lb.cur.WriteByte(b)
		}
	}
	lb.mu.Unlock()
	if lb.onLine != nil {
		for _, rec := range completed {
			lb.onLine(rec)
		}
	}
	return len(p), nil
}

// Recs returns a copy of the ring window (complete lines only).
func (lb *LineBuffer) Recs() []LineRec {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	out := make([]LineRec, len(lb.recs))
	copy(out, lb.recs)
	return out
}

// Dropped reports how many complete lines fell off the ring front.
func (lb *LineBuffer) Dropped() int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.dropped
}

// FlushLine promotes a partial trailing line (output that never ended in
// \n) into the ring and returns it ("" when nothing pending).
func (lb *LineBuffer) FlushLine() string {
	lb.mu.Lock()
	if lb.cur.Len() == 0 {
		lb.mu.Unlock()
		return ""
	}
	rec := LineRec{Seq: lb.nextSeq(), Line: lb.cur.String()}
	lb.cur.Reset()
	lb.recs = append(lb.recs, rec)
	if len(lb.recs) > lb.max {
		lb.recs = lb.recs[1:]
		lb.dropped++
	}
	lb.mu.Unlock()
	if lb.onLine != nil {
		lb.onLine(rec)
	}
	return rec.Line
}

// Tail returns the last n lines joined with \n (plus any partial line).
func (lb *LineBuffer) Tail(n int) string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	var lines []string
	for _, r := range lb.recs {
		lines = append(lines, r.Line)
	}
	if s := lb.cur.String(); s != "" {
		lines = append(lines, s)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// execMiddleware is the builtin executor's process seam: every external
// command the interpreter runs lands here. We pin cwd, allowlist env, and
// register the process for kill. Windows batch wrappers (.cmd/.bat) are
// transparently dispatched via cmd /c (see exec_windows.go).
// Debugf is a platform-overridable trace hook (windows wires it to a log
// file; nil elsewhere). Temporary diagnostic aid for the kill-latency hunt.
var Debugf = func(format string, args ...interface{}) {}

func (r *Runner) execMiddleware(env []string) func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			hc := interp.HandlerCtx(ctx)
			if len(args) == 0 {
				return interp.ExitStatus(1)
			}
			name, argv := prepareExec(args[0], args[1:])

			cmd := exec.CommandContext(ctx, name, argv...)
			cmd.Dir = hc.Dir
			cmd.Env = env
			cmd.Stdout = hc.Stdout
			cmd.Stderr = hc.Stderr
			cmd.Stdin = hc.Stdin
			setupProcessGroup(cmd)
			// Kill the whole tree on ctx cancel via the per-command handle
			// (Job Object on windows, process group on unix) — race-free
			// because the handle is captured here, not looked up globally.
			th := newTreeHandle()
			cmd.Cancel = func() error {
				Debugf("exec pid=%v CANCEL fired mono=%d", cmdPid(cmd), MonoMS())
				th.terminate()
				return nil
			}
			cmd.WaitDelay = 2 * time.Second

			Debugf("exec starting %q", name)
			if err := cmd.Start(); err != nil {
				fmt.Fprintf(hc.Stderr, "%v\n", err)
				return interp.ExitStatus(127)
			}
			Debugf("exec started pid=%d mono=%d", cmd.Process.Pid, MonoMS())
			_ = th.attach(cmd.Process)
			r.track(cmd.Process.Pid, cmd)
			defer func() {
				r.untrack(cmd.Process.Pid)
				th.close()
			}()

			if err := cmd.Wait(); err != nil {
				Debugf("exec pid=%d WAIT err=%v ctxErr=%v mono=%d", cmd.Process.Pid, err, ctx.Err() != nil, MonoMS())
				if ctx.Err() != nil {
					return interp.ExitStatus(130)
				}
				var ee *exec.ExitError
				if ok := asExitError(err, &ee); ok {
					return interp.ExitStatus(ee.ExitCode())
				}
				return interp.ExitStatus(127)
			}
			Debugf("exec pid=%d WAIT clean", cmd.Process.Pid)
			return nil
		}
	}
}

// openHandler is the interpreter's file-open seam (redirections, etc.).
// Relative paths resolve against the interpreter's cwd; absolute paths pass
// through untouched (shell scripts legitimately use /tmp etc.). Workspace
// containment is enforced by the filesvc layer, not the shell.
func (r *Runner) openHandler() interp.OpenHandlerFunc {
	return func(ctx context.Context, path string, flag int, mode os.FileMode) (io.ReadWriteCloser, error) {
		p := path
		if !filepath.IsAbs(p) {
			hc := interp.HandlerCtx(ctx)
			p = filepath.Join(hc.Dir, p)
		}
		return os.OpenFile(p, flag, mode)
	}
}

func (r *Runner) track(pid int, cmd *exec.Cmd) {
	r.mu.Lock()
	r.childs[pid] = cmd
	r.mu.Unlock()
}

func (r *Runner) untrack(pid int) {
	r.mu.Lock()
	delete(r.childs, pid)
	r.mu.Unlock()
}

// KillAll terminates every live child (process group on unix, Job Object /
// taskkill tree on windows). Used by JobKill when no single job context is
// at hand.
func (r *Runner) KillAll() {
	r.mu.Lock()
	pids := make([]int, 0, len(r.childs))
	for pid := range r.childs {
		pids = append(pids, pid)
	}
	r.mu.Unlock()
	for _, pid := range pids {
		killProcessTree(pid)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

var _ = bytes.MinRead // keep bytes import when refactors drop its use

func cmdPid(cmd *exec.Cmd) int {
	if cmd.Process != nil {
		return cmd.Process.Pid
	}
	return 0
}

// monoBase anchors monotonic timestamps for trace lines (immune to wall-clock
// steps, unlike Debugf's wall formatting).
var monoBase = time.Now()

// MonoMS returns milliseconds since process start (monotonic).
func MonoMS() int64 { return int64(time.Since(monoBase) / time.Millisecond) }
