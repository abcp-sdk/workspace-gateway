//go:build !windows

package shellh

import (
	"os"
	"os/exec"
	"syscall"
)

// treeHandle: on unix the "handle" is the process group id (setpgid at
// setup); terminate kills the whole group.
type treeHandle struct {
	pid int
}

func newTreeHandle() *treeHandle { return &treeHandle{} }

// attach records the pid (the pgid equals it thanks to Setpgid).
func (h *treeHandle) attach(proc *os.Process) error {
	h.pid = proc.Pid
	return nil
}

func (h *treeHandle) terminate() {
	if h.pid != 0 {
		_ = syscall.Kill(-h.pid, syscall.SIGKILL)
		_ = syscall.Kill(h.pid, syscall.SIGKILL)
	}
}

func (h *treeHandle) close() {}

// prepareExec rewrites the argv for platforms where exec is direct.
// On unix, .cmd/.bat can't run anyway; pass through.
func prepareExec(name string, args []string) (string, []string) {
	return name, args
}

// setupProcessGroup puts the child into its own process group so a kill can
// take out the whole tree (sh children included).
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the process group of pid (negative pid targets the
// group). Best-effort: a dead group returns ESRCH which we ignore.
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
