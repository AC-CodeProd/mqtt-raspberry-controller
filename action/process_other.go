//go:build !linux

package action

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type commandWaiter struct {
	done    chan struct{}
	waitCmd func() error
}

func configureCommand(_ *exec.Cmd) {}

func bindCommandExecutable(cmd *exec.Cmd, path string) (func(), error) {
	info, err := os.Stat(path)
	if err != nil {
		return func() {}, fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return func() {}, fmt.Errorf("executable is not a regular executable file")
	}
	cmd.Path = path
	return func() {}, nil
}

func observeCommand(cmd *exec.Cmd) *commandWaiter {
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	return &commandWaiter{done: done, waitCmd: func() error { <-done; return waitErr }}
}

func (w *commandWaiter) wait() error { return w.waitCmd() }

func signalCommandGroup(pid int, _ bool) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func terminateCommandGroup(pid int, _ time.Duration) error {
	return signalCommandGroup(pid, true)
}
