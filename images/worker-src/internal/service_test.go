package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/easylab-platform/easyworker/gen/worker/v1"
	workerv1connect "github.com/easylab-platform/easyworker/gen/worker/v1/workerv1connect"
	"github.com/easylab-platform/easyworker/internal/filesvc"
	"github.com/easylab-platform/easyworker/internal/jobsvc"
	"github.com/easylab-platform/easyworker/internal/shellh"
)

func newTestServer(t *testing.T) (client workerv1connect.WorkerServiceClient, cleanup func()) {
	t.Helper()
	ws := t.TempDir()
	runner := shellh.New(ws, []string{"PATH=/usr/bin:/bin"})
	store, err := jobsvc.OpenStore(filepath.Join(ws, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(jobsvc.NewManager(runner, store, 1000, 200), filesvc.New(ws), runner)

	mux := http.NewServeMux()
	mux.Handle(workerv1connect.NewWorkerServiceHandler(svc))
	srv := httptest.NewServer(mux)

	c := workerv1connect.NewWorkerServiceClient(http.DefaultClient, srv.URL)
	return c, func() { srv.Close(); store.Close() }
}

func TestInfo(t *testing.T) {
	c, done := newTestServer(t)
	defer done()

	info, err := c.Info(context.Background(), connect.NewRequest(&workerv1.InfoRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if info.Msg.Shell != "builtin(mvdan-sh)" {
		t.Errorf("shell = %q", info.Msg.Shell)
	}
	if info.Msg.Workspace == "" {
		t.Error("workspace empty")
	}
	if len(info.Msg.BootId) != 32 {
		t.Errorf("boot_id = %q, want 32-hex", info.Msg.BootId)
	}
}

func TestExecuteWatchLifecycle(t *testing.T) {
	c, done := newTestServer(t)
	defer done()

	ctx := context.Background()
	exec, err := c.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{
		Command: "echo first && sleep 0.3 && echo second && echo warn >&2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	jobID := exec.Msg.JobId
	if jobID == "" {
		t.Fatal("empty job id")
	}

	// watch: replay + live + done
	stream, err := c.WatchJob(ctx, connect.NewRequest(&workerv1.WatchJobRequest{JobId: jobID}))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	var doneEv *workerv1.WatchJobResponse_Done
	for stream.Receive() {
		msg := stream.Msg()
		switch ev := msg.Event.(type) {
		case *workerv1.WatchJobResponse_Output:
			lines = append(lines, ev.Output)
		case *workerv1.WatchJobResponse_Done_:
			doneEv = ev.Done
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if doneEv == nil || doneEv.ExitCode != 0 {
		t.Fatalf("done event = %+v", doneEv)
	}
	// live fanout carries BOTH stdout and stderr in sequence order
	if len(lines) != 3 || lines[0] != "first" || lines[1] != "second" || lines[2] != "warn" {
		t.Errorf("live lines = %v", lines)
	}

	// wait on the finished job returns immediately
	wait, err := c.JobWait(ctx, connect.NewRequest(&workerv1.JobWaitRequest{JobId: jobID, TimeoutMs: 1000}))
	if err != nil || wait.Msg.State != "done" {
		t.Fatalf("wait = %s err=%v", wait.Msg.State, err)
	}

	// history: all + stream filters
	hist, err := c.JobOutput(ctx, connect.NewRequest(&workerv1.JobOutputRequest{JobId: jobID, Start: 0, End: 0}))
	if err != nil || hist.Msg.TotalLines != 3 {
		t.Fatalf("all history err=%v total=%d lines=%v", err, hist.Msg.TotalLines, hist.Msg.Lines)
	}
	stdoutOnly, _ := c.JobOutput(ctx, connect.NewRequest(&workerv1.JobOutputRequest{JobId: jobID, Start: 0, End: 0, Stream: "stdout"}))
	if stdoutOnly.Msg.TotalLines != 2 || stdoutOnly.Msg.Lines[0] != "first" {
		t.Errorf("stdout filter = %v", stdoutOnly.Msg.Lines)
	}
	stderrOnly, _ := c.JobOutput(ctx, connect.NewRequest(&workerv1.JobOutputRequest{JobId: jobID, Start: 0, End: 0, Stream: "stderr"}))
	if stderrOnly.Msg.TotalLines != 1 || stderrOnly.Msg.Lines[0] != "warn" {
		t.Errorf("stderr filter = %v", stderrOnly.Msg.Lines)
	}

	// negative-start pagination
	last, _ := c.JobOutput(ctx, connect.NewRequest(&workerv1.JobOutputRequest{JobId: jobID, Start: -1, End: 0}))
	if last.Msg.TotalLines != 3 || len(last.Msg.Lines) != 1 || last.Msg.Lines[0] != "warn" {
		t.Errorf("tail-1 = %v", last.Msg.Lines)
	}
}

// Regression for the untyped-constant clamp: jobWaitMaxMs=60_000 compared
// against time.Duration meant 60µs, so every JobWait fired instantly with
// "running". A long wait on a job finishing at ~1s must block and report
// done, not return early.
func TestJobWaitHonorsLongTimeouts(t *testing.T) {
	c, done := newTestServer(t)
	defer done()
	ctx := context.Background()

	exec, err := c.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{Command: "sleep 1"}))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	wait, err := c.JobWait(ctx, connect.NewRequest(&workerv1.JobWaitRequest{JobId: exec.Msg.JobId, TimeoutMs: 10000}))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if wait.Msg.State != "done" {
		t.Fatalf("state = %s want done (waited %v)", wait.Msg.State, elapsed)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("JobWait returned after %v — timeout was clamped again", elapsed)
	}
}

func TestFilesRoundTrip(t *testing.T) {
	c, done := newTestServer(t)
	defer done()
	ctx := context.Background()

	if _, err := c.FileWrite(ctx, connect.NewRequest(&workerv1.FileWriteRequest{Path: "d/f.bin", Content: []byte{0, 1, 2, 255}})); err != nil {
		t.Fatal(err)
	}
	rd, err := c.FileRead(ctx, connect.NewRequest(&workerv1.FileReadRequest{Path: "d/f.bin"}))
	if err != nil || len(rd.Msg.Content) != 4 || rd.Msg.Content[3] != 255 {
		t.Fatalf("binary round-trip mismatch: %v err=%v", rd.Msg.Content, err)
	}
	ls, err := c.FileList(ctx, connect.NewRequest(&workerv1.FileListRequest{Path: "d"}))
	if err != nil || !ls.Msg.IsDir || len(ls.Msg.Files) != 1 || ls.Msg.Files[0].Path != "d/f.bin" {
		t.Fatalf("list: %v err=%v", ls.Msg, err)
	}

	// escape must fail
	if _, err := c.FileRead(ctx, connect.NewRequest(&workerv1.FileReadRequest{Path: "../../etc/passwd"})); err == nil {
		t.Error("traversal must fail")
	}
}

func TestStdinAndKill(t *testing.T) {
	c, done := newTestServer(t)
	defer done()
	ctx := context.Background()

	exec, err := c.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{Command: "cat"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.JobStdin(ctx, connect.NewRequest(&workerv1.JobStdinRequest{JobId: exec.Msg.JobId, Data: []byte("hello\n"), Close: true})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hist, err := c.JobOutput(ctx, connect.NewRequest(&workerv1.JobOutputRequest{JobId: exec.Msg.JobId, Start: 0, End: 0}))
		if err == nil && hist.Msg.Done && len(hist.Msg.Lines) == 1 && hist.Msg.Lines[0] == "hello" {
			goto killed
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("cat did not complete")
killed:
	exec2, err := c.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{Command: "sleep 60"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.JobKill(ctx, connect.NewRequest(&workerv1.JobKillRequest{JobId: exec2.Msg.JobId})); err != nil {
		t.Fatal(err)
	}
	wait, err := c.JobWait(ctx, connect.NewRequest(&workerv1.JobWaitRequest{JobId: exec2.Msg.JobId, TimeoutMs: 5000}))
	if err != nil {
		t.Fatal(err)
	}
	if wait.Msg.State != "killed" {
		t.Errorf("state = %s want killed", wait.Msg.State)
	}
}
