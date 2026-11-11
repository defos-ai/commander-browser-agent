//go:build unix

package hermes

import (
	"errors"
	"os/exec"
	"syscall"
)

func requireProcessOwnership() error { return nil }

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

func processGroupExists(command *exec.Cmd) bool {
	if command.Process == nil {
		return false
	}
	err := syscall.Kill(-command.Process.Pid, 0)
	return !errors.Is(err, syscall.ESRCH)
}
