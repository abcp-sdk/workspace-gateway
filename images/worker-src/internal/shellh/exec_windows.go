//go:build windows

package shellh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// treeHandle is the per-command kill handle: a Job Object created at attach
// time and captured by the exec Cancel closure — the kill path never depends
// on the shared registry (race-free). The registry below only serves
// KillAll.
type treeHandle struct {
	pid    int
	job    windows.Handle
	hasJob bool
}

func newTreeHandle() *treeHandle { return &treeHandle{} }

// attach creates the Job Object, assigns the freshly started process
// (KILL_ON_JOB_CLOSE), registers for KillAll, and arms the handle.
func (h *treeHandle) attach(proc *os.Process) error {
	h.pid = proc.Pid
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		dblog("pid=%d CreateJobObject err=%v", proc.Pid, err)
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		dblog("pid=%d SetInfo err=%v", proc.Pid, err)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	// os.Process exposes no HANDLE; derive one from the PID.
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		windows.CloseHandle(job)
		dblog("pid=%d OpenProcess err=%v", proc.Pid, err)
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(p)
	if err := windows.AssignProcessToJobObject(job, p); err != nil {
		windows.CloseHandle(job)
		dblog("pid=%d Assign err=%v", proc.Pid, err)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	h.job = job
	h.hasJob = true
	registerJob(proc.Pid, job)
	dblog("pid=%d attach OK (job assigned)", proc.Pid)
	return nil
}

// terminate kills the whole tree: Job Object first, taskkill /T /F fallback.
func (h *treeHandle) terminate() {
	if h.pid == 0 {
		dblog("terminate: no pid (pre-attach cancel)")
		return
	}
	if h.hasJob {
		if err := windows.TerminateJobObject(h.job, 1); err == nil {
			dblog("pid=%d TerminateJobObject OK", h.pid)
			return
		} else {
			dblog("pid=%d TerminateJobObject err=%v -> taskkill", h.pid, err)
		}
	} else {
		dblog("pid=%d terminate: no job handle -> taskkill", h.pid)
	}
	c := exec.Command("taskkill", "/T", "/F", "/PID", itoa(h.pid))
	out, err := c.CombinedOutput()
	dblog("pid=%d taskkill err=%v out=%q", h.pid, err, string(out))
}

// close releases the Job Object after a natural exit (KILL_ON_JOB_CLOSE
// already reaped any stragglers) and unregisters.
func (h *treeHandle) close() {
	if h.pid != 0 {
		unregisterJob(h.pid)
	}
	if h.hasJob {
		windows.CloseHandle(h.job)
		h.hasJob = false
	}
}

func dblog(format string, args ...interface{}) { Debugf(format, args...) }

// ---- KillAll registry ----

var (
	jobMu    sync.Mutex
	jobByPid = map[int]windows.Handle{}
)

func registerJob(pid int, h windows.Handle) {
	jobMu.Lock()
	jobByPid[pid] = h
	jobMu.Unlock()
}

func unregisterJob(pid int) {
	jobMu.Lock()
	delete(jobByPid, pid)
	jobMu.Unlock()
}

// killAllHandle terminates one registered job (best-effort).
func killAllHandle(pid int) {
	jobMu.Lock()
	h, ok := jobByPid[pid]
	delete(jobByPid, pid)
	jobMu.Unlock()
	if ok {
		_ = windows.TerminateJobObject(h, 1)
		windows.CloseHandle(h)
	}
}

// prepareExec routes batch wrappers through cmd /c: exec.Command cannot
// execute .cmd/.bat directly on Windows (they are not PE images). npm/pnpm
// and friends are all batch wrappers, so this path is common.
func prepareExec(name string, args []string) (string, []string) {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat") {
		return "cmd.exe", append([]string{"/c", name}, args...)
	}
	return name, args
}

// setupProcessGroup: Windows has no fork-group semantics; isolation and
// tree-kill come from the Job Object attached after start.
func setupProcessGroup(cmd *exec.Cmd) {}

func killProcessTree(pid int) { killAllHandle(pid) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

var _ = syscall.SIGKILL // keep syscall import parity with exec_unix.go
