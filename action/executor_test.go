package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

const helperEnvironmentKey = "MQTT_RASPBERRY_ACTION_HELPER"

func TestCommandHelper(t *testing.T) {
	if os.Getenv(helperEnvironmentKey) != "1" {
		return
	}
	args := helperArgs(os.Args)
	if len(args) == 0 {
		os.Exit(90)
	}
	switch args[0] {
	case "output":
		_, _ = io.WriteString(os.Stdout, args[1])
		_, _ = io.WriteString(os.Stderr, args[2])
	case "output-exit":
		_, _ = io.WriteString(os.Stdout, args[1])
		os.Exit(7)
	case "flood":
		n, _ := strconv.Atoi(args[1])
		chunk := bytes.Repeat([]byte("x"), 32*1024)
		for written := 0; written < n; written += len(chunk) {
			part := chunk
			if len(part) > n-written {
				part = part[:n-written]
			}
			if written/len(chunk)%2 == 0 {
				_, _ = os.Stdout.Write(part)
			} else {
				_, _ = os.Stderr.Write(part)
			}
		}
	case "environment":
		_ = os.WriteFile(args[1], []byte(strings.Join(os.Environ(), "\n")), 0600)
	case "append":
		f, err := os.OpenFile(args[1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(91)
		}
		_, _ = fmt.Fprintln(f, args[2])
		_ = f.Close()
	case "exit":
		code, _ := strconv.Atoi(args[1])
		os.Exit(code)
	case "block":
		mustWrite(args[1], strconv.Itoa(os.Getpid()))
		select {}
	case "ignore-term":
		signalIgnoreTerm()
		mustWrite(args[1], strconv.Itoa(os.Getpid()))
		select {}
	case "spawn-tree":
		child := helperCommand("spawn-child", args[2], args[3])
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		mustWrite(args[1], strconv.Itoa(child.Process.Pid))
		waitForFile(args[2])
		mustWrite(args[4], "ready")
		select {}
	case "spawn-child":
		grandchild := helperCommand("block", args[1])
		grandchild.Stdout, grandchild.Stderr = os.Stdout, os.Stderr
		if err := grandchild.Start(); err != nil {
			os.Exit(93)
		}
		mustWrite(args[2], strconv.Itoa(grandchild.Process.Pid))
		waitForFile(args[1])
		select {}
	case "spawn-ignore-term":
		child := helperCommand("ignore-term", args[2])
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(94)
		}
		mustWrite(args[1], strconv.Itoa(child.Process.Pid))
		waitForFile(args[2])
		mustWrite(args[3], "ready")
		select {}
	case "exit-with-pipe-holder":
		child := helperCommand("block", args[2])
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(95)
		}
		mustWrite(args[1], strconv.Itoa(child.Process.Pid))
		waitForFile(args[2])
	default:
		os.Exit(96)
	}
	os.Exit(0)
}

func TestExecuteDelayCompletesAndCancelsInFlight(t *testing.T) {
	e := &Executor{}
	if err := e.Execute(context.Background(), []config.Action{{Type: "delay", Duration: "1ms"}}); err != nil {
		t.Fatalf("completed delay: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := e.Execute(ctx, []config.Action{{Type: "delay", Duration: "1h"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in-flight delay error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("in-flight delay cancellation took %s", elapsed)
	}
}

func TestCommandOutputIsBoundedAndNeverDisclosed(t *testing.T) {
	secret := "super-secret-command-output"
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	action := helperAction("output", secret, secret)
	action.Env["GOCOVERDIR"] = t.TempDir()
	stats, err := runCommand(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != uint64(2*len(secret)) || stats.Captured != stats.Total || stats.Truncated {
		t.Fatalf("output stats = %+v", stats)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("logs disclosed command output: %q", logs.String())
	}

	const outputSize = 8 * 1024 * 1024
	action = helperAction("flood", strconv.Itoa(outputSize))
	action.Env["GOCOVERDIR"] = t.TempDir()
	stats, err = runCommand(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != outputSize || stats.Captured != commandOutputLimit || !stats.Truncated {
		t.Fatalf("flood output stats = %+v", stats)
	}
}

func TestBoundedOutputSaturatesAndIsConcurrencySafe(t *testing.T) {
	output := boundedOutput{total: ^uint64(0) - 1}
	writes := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, _ = output.Write(bytes.Repeat([]byte("x"), int(commandOutputLimit)))
			writes <- struct{}{}
		}()
	}
	<-writes
	<-writes
	stats := output.snapshot()
	if stats.Total != ^uint64(0) || stats.Captured != commandOutputLimit || !stats.Truncated {
		t.Fatalf("saturated output stats = %+v", stats)
	}
	if len(output.data) != int(commandOutputLimit) || cap(output.data) > int(commandOutputLimit) {
		t.Fatalf("bounded output allocation len=%d cap=%d", len(output.data), cap(output.data))
	}
}

func TestCommandFailureDoesNotDiscloseOutputArgsOrEnvironment(t *testing.T) {
	secretOutput := "secret-output-value"
	secretArgument := "secret-argument-value"
	secretEnvironment := "secret-environment-value"
	action := helperAction("output-exit", secretOutput, secretArgument)
	action.Env["PRIVATE_VALUE"] = secretEnvironment
	_, err := runCommand(context.Background(), action)
	if err == nil {
		t.Fatal("runCommand succeeded")
	}
	message := err.Error()
	for _, secret := range []string{secretOutput, secretArgument, secretEnvironment} {
		if strings.Contains(message, secret) {
			t.Fatalf("command error disclosed secret %q: %v", secret, err)
		}
	}
	if !strings.Contains(message, "output_total_bytes=") || !strings.Contains(message, "output_truncated=") {
		t.Fatalf("command error omitted output metadata: %v", err)
	}
}

func TestCommandUsesMinimalExplicitEnvironment(t *testing.T) {
	t.Setenv("MQTT_PASSWORD", "must-not-be-inherited")
	path := filepath.Join(t.TempDir(), "environment")
	action := helperAction("environment", path)
	action.Env["ACTION_VALUE"] = "configured"
	if _, err := runCommand(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	got := make(map[string]string, len(lines))
	for _, line := range lines {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			got[name] = value
		}
	}
	if got["ACTION_VALUE"] != "configured" || got[helperEnvironmentKey] != "1" {
		t.Fatalf("configured environment missing: %q", lines)
	}
	if _, ok := got["MQTT_PASSWORD"]; ok {
		t.Fatalf("inherited secret present in environment: %q", lines)
	}
	for name := range got {
		if name != "PATH" && name != "LANG" && name != "LC_ALL" && name != "ACTION_VALUE" && name != helperEnvironmentKey {
			t.Fatalf("unexpected inherited environment variable %q", name)
		}
	}
}

func TestCommandTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		cancel  bool
		want    error
	}{
		{name: "timeout", timeout: "100ms", want: context.DeadlineExceeded},
		{name: "shutdown cancellation", timeout: "1h", cancel: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			action := helperAction("block", ready)
			action.Timeout = tc.timeout
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := runCommand(ctx, action); result <- err }()
			waitForTestFile(t, ready)
			if tc.cancel {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, tc.want) {
					t.Fatalf("runCommand error = %v, want %v", err, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("command cancellation was not bounded")
			}
		})
	}
}

func TestCommandKillsChildAndGrandchild(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child-pid")
	grandchildPIDFile := filepath.Join(dir, "grandchild-pid")
	grandchildReady := filepath.Join(dir, "grandchild-ready")
	ready := filepath.Join(dir, "ready")
	action := helperAction("spawn-tree", childPIDFile, grandchildReady, grandchildPIDFile, ready)
	action.Timeout = "5s"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { _, err := runCommand(ctx, action); result <- err }()
	waitForTestFile(t, ready)
	childPID := readPID(t, childPIDFile)
	grandchildPID := readPID(t, grandchildPIDFile)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runCommand error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command tree cancellation was not bounded")
	}
	waitForProcessExit(t, childPID)
	waitForProcessExit(t, grandchildPID)
}

func TestCommandKillsSIGTERMIgnoringChild(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	childReady := filepath.Join(dir, "child-ready")
	ready := filepath.Join(dir, "ready")
	action := helperAction("spawn-ignore-term", pidFile, childReady, ready)
	action.Timeout = "5s"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { _, err := runCommand(ctx, action); result <- err }()
	waitForTestFile(t, ready)
	pid := readPID(t, pidFile)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runCommand error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SIGTERM-resistant child cancellation was not bounded")
	}
	waitForProcessExit(t, pid)
}

func TestCommandDescendantHoldingPipesIsBounded(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	ready := filepath.Join(dir, "ready")
	action := helperAction("exit-with-pipe-holder", pidFile, ready)
	action.Timeout = "2s"
	result := make(chan error, 1)
	go func() { _, err := runCommand(context.Background(), action); result <- err }()
	waitForTestFile(t, ready)
	pid := readPID(t, pidFile)
	t.Cleanup(func() { killPID(pid) })
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("runCommand error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("descendant holding output pipes kept command execution blocked")
	}
	killPID(pid)
	waitForProcessExit(t, pid)
}

func TestCommandStartErrorIsSanitized(t *testing.T) {
	action := config.Action{Type: "command", Command: filepath.Join(t.TempDir(), "missing-secret-name"), Timeout: "1s"}
	_, err := runCommand(context.Background(), action)
	if err == nil || !strings.Contains(err.Error(), "start command") {
		t.Fatalf("start error = %v", err)
	}
	if strings.Contains(err.Error(), "missing-secret-name") {
		t.Fatalf("start error disclosed command path: %v", err)
	}
}

func TestCommandSequenceOrderErrorsAndIgnoreError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order")
	e := &Executor{}
	first := helperAction("append", path, "first")
	fail := helperAction("exit", "7")
	last := helperAction("append", path, "last")
	if err := e.Execute(context.Background(), []config.Action{first, fail, last}); err == nil {
		t.Fatal("Execute succeeded after command failure")
	}
	assertFileLines(t, path, []string{"first"})
	fail.IgnoreError = true
	if err := e.Execute(context.Background(), []config.Action{fail, last}); err != nil {
		t.Fatal(err)
	}
	assertFileLines(t, path, []string{"first", "last"})
}

func helperAction(mode string, args ...string) config.Action {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return config.Action{
		Type:    "command",
		Command: executable,
		Args:    append([]string{"-test.run=^TestCommandHelper$", "--", mode}, args...),
		Env:     map[string]string{helperEnvironmentKey: "1"},
		Timeout: "5s",
	}
}

func helperCommand(mode string, args ...string) *exec.Cmd {
	executable, _ := os.Executable()
	cmd := exec.Command(executable, append([]string{"-test.run=^TestCommandHelper$", "--", mode}, args...)...)
	cmd.Env = append(minimalEnvironment(nil), helperEnvironmentKey+"=1")
	return cmd
}

func helperArgs(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func mustWrite(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		os.Exit(97)
	}
}

func waitForFile(path string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	os.Exit(98)
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for readiness file %s", path)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func signalIgnoreTerm() {
	ignoreTerminationSignal()
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux process-group behavior")
	}
}

func assertFileLines(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("file lines = %v, want %v", got, want)
	}
}
