//go:build linux

package action

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type commandWaiter struct {
	done    chan struct{}
	waitCmd func() error
}

func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func bindCommandExecutable(cmd *exec.Cmd, path string) (func(), error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return func() {}, fmt.Errorf("open executable: %w", err)
	}
	file := os.NewFile(uintptr(fd), "command-executable")
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return func() {}, fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		_ = file.Close()
		return func() {}, fmt.Errorf("executable is not a regular executable file")
	}
	childFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	cmd.Path = fmt.Sprintf("/proc/self/fd/%d", childFD)
	return func() { _ = file.Close() }, nil
}

func observeCommand(cmd *exec.Cmd) *commandWaiter {
	done := make(chan struct{})
	var observeErr error
	go func() {
		defer close(done)
		var info unix.Siginfo
		for {
			observeErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(observeErr, syscall.EINTR) {
				return
			}
		}
	}()
	return &commandWaiter{
		done: done,
		waitCmd: func() error {
			<-done
			waitErr := cmd.Wait()
			if observeErr != nil {
				return fmt.Errorf("observe command exit: %w", observeErr)
			}
			return waitErr
		},
	}
}

func (w *commandWaiter) wait() error { return w.waitCmd() }

func signalCommandGroup(pid int, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func terminateCommandGroup(pid int, grace time.Duration) error {
	if err := signalCommandGroup(pid, false); err != nil {
		return err
	}
	timer := time.NewTimer(grace)
	<-timer.C
	return signalCommandGroup(pid, true)
}
