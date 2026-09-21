package jobsvc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/easylab-platform/easyworker/internal/shellh"
)

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	ws := t.TempDir()
	dbPath := filepath.Join(ws, "jobs.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewManager(shellh.New(ws, []string{"PATH=/usr/bin:/bin"}), store, 1000, 200), dbPath
}

func TestExecuteLifecycle(t *testing.T) {
	m, _ := newTestManager(t)

	id, err := m.Execute(context.Background(), "echo hi && sleep 0.2 && echo bye", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	job, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	// subscribe before completion: live gets the output
	_, live := job.Subscribe()
	select {
	case line := <-live:
		if line == "" {
			t.Error("empty live line")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live output within 5s")
	}

	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}

	res := job.Result()
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}
	if res.Stdout != "hi\nbye" {
		t.Errorf("stdout tail = %q", res.Stdout)
	}

	lines, total, _, _, done, err := m.OutputLines(id, -1, 0, 0)
	if err != nil || total != 2 || len(lines) != 2 || lines[0] != "hi" || lines[1] != "bye" || !done {
		t.Errorf("history = %v (total %d done %v err %v)", lines, total, done, err)
	}
}

func TestExecuteNoTrailingNewline(t *testing.T) {
	m, _ := newTestManager(t)

	id, _ := m.Execute(context.Background(), "printf no-newline-here", "", nil)
	job, _ := m.Get(id)
	<-job.Done()

	lines, total, _, _, _, _ := m.OutputLines(id, -1, 0, 0)
	if total != 1 || len(lines) != 1 || lines[0] != "no-newline-here" {
		t.Errorf("history = %v (total %d)", lines, total)
	}
}

func TestStreamFilter(t *testing.T) {
	m, _ := newTestManager(t)

	id, _ := m.Execute(context.Background(), "echo to-out && echo to-err >&2", "", nil)
	job, _ := m.Get(id)
	<-job.Done()

	all, totalAll, _, _, _, _ := m.OutputLines(id, -1, 0, 0)
	if totalAll != 2 || len(all) != 2 {
		t.Fatalf("all = %v (total %d)", all, totalAll)
	}
	out, totalOut, _, _, _, _ := m.OutputLines(id, StreamStdout, 0, 0)
	if totalOut != 1 || len(out) != 1 || out[0] != "to-out" {
		t.Errorf("stdout = %v (total %d)", out, totalOut)
	}
	errs, totalErr, _, _, _, _ := m.OutputLines(id, StreamStderr, 0, 0)
	if totalErr != 1 || len(errs) != 1 || errs[0] != "to-err" {
		t.Errorf("stderr = %v (total %d)", errs, totalErr)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	ws := t.TempDir()
	dbPath := filepath.Join(ws, "jobs.db")

	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	m1 := NewManager(shellh.New(ws, []string{"PATH=/usr/bin:/bin"}), store1, 1000, 200)
	id, _ := m1.Execute(context.Background(), "echo persist-me", "", nil)
	job, _ := m1.Get(id)
	<-job.Done()

	// let the async line writer flush, then close
	time.Sleep(400 * time.Millisecond)
	store1.Close()

	// reopen: history must survive (emptyDir semantics: container restart)
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	m2 := NewManager(shellh.New(ws, []string{"PATH=/usr/bin:/bin"}), store2, 1000, 200)

	lines, total, _, _, done, err := m2.OutputLines(id, -1, 0, 0)
	if err != nil || total != 1 || len(lines) != 1 || lines[0] != "persist-me" || !done {
		t.Errorf("post-restart history = %v (total %d done %v err %v)", lines, total, done, err)
	}
	row, ok, _ := store2.QueryJob(id)
	if !ok || row.State != StateDone {
		t.Errorf("post-restart row = %+v ok=%v", row, ok)
	}
}

func TestBootRecovery(t *testing.T) {
	ws := t.TempDir()
	dbPath := filepath.Join(ws, "jobs.db")

	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a job that was running when the worker died
	if err := store1.EnqueueJob("orphan1", "sleep forever", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	store1.Close()

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	row, ok, _ := store2.QueryJob("orphan1")
	if !ok || row.State != StateFailed || row.ExitCode != 137 {
		t.Errorf("orphan = %+v ok=%v (want failed/137)", row, ok)
	}
}

func TestRetention(t *testing.T) {
	m, dbPath := newTestManager(t)

	id, _ := m.Execute(context.Background(), "echo old-job", "", nil)
	job, _ := m.Get(id)
	<-job.Done()
	time.Sleep(400 * time.Millisecond)

	// age it beyond the window by rewriting finished_at directly
	if _, err := m.Store().db.Exec(`UPDATE jobs SET finished_at=? WHERE id=?`, time.Now().Add(-48*time.Hour).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	if n := m.Store().Retention(24); n != 1 {
		t.Errorf("retention deleted %d jobs, want 1", n)
	}
	m.Prune(0) // memory map follows the store (retention loop does this hourly)
	_, total, _, _, _, _ := m.OutputLines(id, -1, 0, 0)
	if total != 0 {
		t.Errorf("retained lines after retention: %d", total)
	}
	_ = dbPath
}

func TestKillAndStdin(t *testing.T) {
	m, _ := newTestManager(t)

	// stdin round-trip
	id, _ := m.Execute(context.Background(), "cat", "", nil)
	job, _ := m.Get(id)
	if err := job.Stdin([]byte("hello\n"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cat did not finish after stdin close")
	}
	lines, _, _, _, _, _ := m.OutputLines(id, -1, 0, 0)
	if len(lines) != 1 || lines[0] != "hello" {
		t.Errorf("lines = %v", lines)
	}

	// kill a sleeping job
	id2, _ := m.Execute(context.Background(), "sleep 60", "", nil)
	job2, _ := m.Get(id2)
	time.Sleep(100 * time.Millisecond)
	job2.Kill()
	select {
	case <-job2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not finish the job")
	}
	if job2.State != StateKilled {
		t.Errorf("state = %s want killed", job2.State)
	}
}
