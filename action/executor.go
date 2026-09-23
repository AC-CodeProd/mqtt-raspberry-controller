package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type gpioOutput interface {
	SetOutput(string, bool) error
}

type mqttPublisher interface {
	Publish(string, any, byte, bool) error
}

const (
	commandOutputLimit    = uint64(64 * 1024)
	commandTerminateGrace = 250 * time.Millisecond
	commandWaitDelay      = 250 * time.Millisecond
)

type Executor struct {
	cfg  *config.Config
	gpio gpioOutput
	mqtt mqttPublisher
}

type outputStats struct {
	Total     uint64
	Captured  uint64
	Truncated bool
}

type boundedOutput struct {
	mu        sync.Mutex
	total     uint64
	data      []byte
	truncated bool
}

func New(cfg *config.Config, gpioManager gpioOutput, mqttClient mqttPublisher) *Executor {
	return &Executor{
		cfg:  cfg,
		gpio: gpioManager,
		mqtt: mqttClient,
	}
}

func (e *Executor) Execute(ctx context.Context, actions []config.Action) error {
	for i, action := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.executeOne(ctx, action); err != nil {
			if action.IgnoreError {
				log.Printf("action[%d] type=%s failed but ignore_error=true: %v", i, action.Type, err)
				continue
			}
			return fmt.Errorf("action[%d] type=%s: %w", i, action.Type, err)
		}
	}

	return nil
}

func (e *Executor) executeOne(ctx context.Context, action config.Action) error {
	switch action.Type {
	case "gpio":
		return e.gpio.SetOutput(action.GPIO, *action.Value)

	case "command":
		return executeCommand(ctx, action)

	case "delay":
		return executeDelay(ctx, action.Duration)

	case "mqtt":
		qos := e.cfg.MQTTQoS()
		if action.QoS != nil {
			qos = *action.QoS
		}
		return e.mqtt.Publish(action.Topic, action.Payload, qos, action.Retain)

	default:
		return fmt.Errorf("unsupported action type %q", action.Type)
	}
}

func executeCommand(ctx context.Context, action config.Action) error {
	stats, err := runCommand(ctx, action)
	if err != nil {
		return err
	}
	if stats.Total > 0 {
		log.Printf("command completed (%s)", formatOutputStats(stats))
	}
	return nil
}

func runCommand(parent context.Context, action config.Action) (outputStats, error) {
	if !filepath.IsAbs(action.Command) {
		return outputStats{}, fmt.Errorf("start command: executable path must be absolute")
	}

	timeout := action.CommandTimeout()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return outputStats{}, commandContextError(err, timeout, outputStats{})
	}

	cmd := exec.Command(action.Command, action.Args...)
	cmd.Dir = action.WorkingDir
	cmd.Env = minimalEnvironment(action.Env)
	cmd.WaitDelay = commandWaitDelay
	configureCommand(cmd)
	closeExecutable, err := bindCommandExecutable(cmd, action.Command)
	if err != nil {
		return outputStats{}, sanitizeStartError(err)
	}
	defer closeExecutable()

	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return output.snapshot(), sanitizeStartError(err)
	}

	waiter := observeCommand(cmd)

	select {
	case <-waiter.done:
		err := waiter.wait()
		stats := output.snapshot()
		// Cancellation can race with process exit. Rechecking after Wait makes the
		// context error deterministic without signaling a reaped numeric PID.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, commandContextError(ctxErr, timeout, stats)
		}
		if err != nil {
			return stats, fmt.Errorf("command failed (%s): %w", formatOutputStats(stats), err)
		}
		return stats, nil

	case <-ctx.Done():
		// On Linux observeCommand only observes exit with waitid(WNOWAIT). The
		// leader therefore still owns its PID/PGID until signaling is complete.
		terminationErr := terminateCommandGroup(cmd.Process.Pid, commandTerminateGrace)
		_ = waiter.wait()
		stats := output.snapshot()
		contextErr := commandContextError(ctx.Err(), timeout, stats)
		if terminationErr != nil {
			return stats, fmt.Errorf("terminate command process group: %v: %w", terminationErr, contextErr)
		}
		return stats, contextErr
	}
}

func commandContextError(err error, timeout time.Duration, stats outputStats) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("command timed out after %s (%s): %w", timeout, formatOutputStats(stats), context.DeadlineExceeded)
	}
	return fmt.Errorf("command canceled (%s): %w", formatOutputStats(stats), context.Canceled)
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	amount := uint64(len(p))
	if math.MaxUint64-w.total < amount {
		w.total = math.MaxUint64
		w.truncated = true
	} else {
		w.total += amount
	}
	remaining := int(commandOutputLimit) - len(w.data)
	captured := len(p)
	if captured > remaining {
		captured = remaining
		w.truncated = true
	}
	w.data = append(w.data, p[:captured]...)
	return len(p), nil
}

func (w *boundedOutput) snapshot() outputStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return outputStats{Total: w.total, Captured: uint64(len(w.data)), Truncated: w.truncated}
}

func formatOutputStats(stats outputStats) string {
	return fmt.Sprintf("output_total_bytes=%d output_captured_bytes=%d output_truncated=%t", stats.Total, stats.Captured, stats.Truncated)
}

func sanitizeStartError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("start command: %w", pathErr.Err)
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return fmt.Errorf("start command: %w", execErr.Err)
	}
	return fmt.Errorf("start command: unavailable")
}

func minimalEnvironment(values map[string]string) []string {
	environment := map[string]string{
		"LANG":   "C",
		"LC_ALL": "C",
		"PATH":   "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	for name, value := range values {
		environment[name] = value
	}
	return sortedEnvironment(environment)
}

func executeDelay(ctx context.Context, raw string) error {
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("parse delay %q: %w", raw, err)
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sortedEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
