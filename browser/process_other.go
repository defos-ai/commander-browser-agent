//go:build !unix

package browser

import (
	"errors"
	"os/exec"
)

func configureCommand(*exec.Cmd) error {
	return errors.New("reusable browsers require Unix process-group support; use Docker or WSL on Windows")
}
