//go:build windows

package commands

import (
	"os/exec"
	"strconv"
)

func configureDevCommand(cmd *exec.Cmd) {
}

func killDevCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
