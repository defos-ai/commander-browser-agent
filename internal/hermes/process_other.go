//go:build !unix

package hermes

import (
	"errors"
	"os/exec"
)

func requireProcessOwnership() error {
	return errors.New("Hermes workers require Unix process ownership; use Docker or WSL")
}

func configureProcess(_ *exec.Cmd)        {}
func killProcessGroup(_ *exec.Cmd)        {}
func processGroupExists(_ *exec.Cmd) bool { return true }
