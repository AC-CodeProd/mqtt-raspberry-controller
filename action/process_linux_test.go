//go:build linux

package action

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func ignoreTerminationSignal() {
	signal.Ignore(syscall.SIGTERM)
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

func killPID(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func TestBindCommandExecutablePinsOpenedFile(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "command")
	replacementPath := filepath.Join(dir, "replacement")
	outputPath := filepath.Join(dir, "output")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\nprintf original >\"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, []byte("#!/bin/sh\nprintf replacement >\"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(commandPath, outputPath)
	closeExecutable, err := bindCommandExecutable(cmd, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeExecutable()
	if err := os.Rename(replacementPath, commandPath); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("executed replacement path content: %q", got)
	}
}
