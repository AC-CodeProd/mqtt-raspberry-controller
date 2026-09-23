package gpio

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/warthog618/go-gpiocdev"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type fakeLine struct {
	closed atomic.Bool
	reads  atomic.Int32
}

func (l *fakeLine) SetValue(int) error  { return nil }
func (l *fakeLine) Value() (int, error) { l.reads.Add(1); return 0, nil }
func (l *fakeLine) Close() error        { l.closed.Store(true); return nil }

type callbackClosingLine struct {
	callbackDone <-chan struct{}
	reads        atomic.Int32
}

func (l *callbackClosingLine) SetValue(int) error  { return nil }
func (l *callbackClosingLine) Value() (int, error) { l.reads.Add(1); return 0, nil }
func (l *callbackClosingLine) Close() error        { <-l.callbackDone; return nil }

func TestStateFromEventUsesLogicalActiveState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		type_ gpiocdev.LineEventType
		want  bool
	}{
		{"rising is active (including active-low lines)", gpiocdev.LineEventRisingEdge, true},
		{"falling is inactive (including active-low lines)", gpiocdev.LineEventFallingEdge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stateFromEvent(gpiocdev.LineEvent{Type: tc.type_})
			if !ok || got != tc.want {
				t.Fatalf("stateFromEvent() = %t, %t; want %t, true", got, ok, tc.want)
			}
		})
	}
}

func TestInputEventHandlerDoesNotReadLine(t *testing.T) {
	line := &fakeLine{}
	m := New(config.GPIOConfig{})
	m.inputs["input"] = line

	got := make(chan bool, 1)
	callback := inputEventHandler("input", func(name string, state bool) {
		if name != "input" {
			t.Errorf("input name = %q, want input", name)
		}
		got <- state
	})
	callback(gpiocdev.LineEvent{Type: gpiocdev.LineEventRisingEdge})

	select {
	case state := <-got:
		if !state {
			t.Fatal("rising edge produced inactive state")
		}
	case <-time.After(time.Second):
		t.Fatal("event callback did not return")
	}
	if got := line.reads.Load(); got != 0 {
		t.Fatalf("event callback read line %d times", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForConcurrentEventWithoutLineLockCycle(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan struct{})
	line := &callbackClosingLine{callbackDone: callbackDone}
	m := New(config.GPIOConfig{})
	m.inputs["input"] = line

	callback := inputEventHandler("input", func(string, bool) {
		close(entered)
		<-release
	})
	go func() {
		callback(gpiocdev.LineEvent{Type: gpiocdev.LineEventRisingEdge})
		close(callbackDone)
	}()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before callback completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked with active event callback")
	}
	if got := line.reads.Load(); got != 0 {
		t.Fatalf("event callback read line %d times", got)
	}
}

func TestOpenInputsRollbackClosesPreviouslyOpenedLine(t *testing.T) {
	m := New(config.GPIOConfig{Inputs: map[string]config.GPIOInputConfig{
		"one": {Chip: "chip", Line: 1},
		"two": {Chip: "chip", Line: 2},
	}})
	opened := &fakeLine{}
	output := &fakeLine{}
	m.outputs["relay"] = output
	var calls atomic.Int32
	m.requestLine = func(lineRequest) (line, error) {
		if calls.Add(1) == 1 {
			return opened, nil
		}
		return nil, errors.New("request failed")
	}
	if err := m.OpenInputs(nil); err == nil {
		t.Fatal("OpenInputs succeeded, want error")
	}
	if !opened.closed.Load() {
		t.Fatal("previously opened input was not closed during rollback")
	}
	if len(m.inputs) != 0 {
		t.Fatalf("inputs after rollback = %d, want 0", len(m.inputs))
	}
	if output.closed.Load() {
		t.Fatal("input rollback closed an output before controller shutdown")
	}
	if len(m.outputs) != 1 {
		t.Fatalf("outputs after input rollback = %d, want 1", len(m.outputs))
	}
}
