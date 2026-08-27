//go:build unix

package orchestrator

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// isolate gives a step its own process group and makes cancellation signal
// the group rather than its leader. This is what makes the step deadline
// real. /bin/sh is dash on most Linuxes, and dash forks rather than execs, so
// signalling the child alone leaves the grandchild running — and a live
// grandchild is enough to hold the step open, because os/exec waits on
// whoever still holds the stdout pipe, not on whoever we started. (macOS
// /bin/sh is bash, which execs a lone simple command, so the shell *becomes*
// the command and the naive kill happens to work. That is why this cost a
// CI run to find.)
//
// Setpgid is also what makes -pid a legal target: without it the negative pid
// names the daemon's own group, and the daemon kills itself.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group was already gone. Losing the race is not a failure
			// to cancel, and saying otherwise would turn a clean timeout
			// into a platform fault.
			return os.ErrProcessDone
		}
		return err
	}
}
