package gpio

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/warthog618/go-gpiocdev"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type behaviorLine struct {
	value    int
	setErr   error
	readErr  error
	closeErr error
	sets     []int
	closed   int
}

func (l *behaviorLine) SetValue(value int) error {
	l.sets = append(l.sets, value)
	l.value = value
	return l.setErr
}
func (l *behaviorLine) Value() (int, error) { return l.value, l.readErr }
func (l *behaviorLine) Close() error        { l.closed++; return l.closeErr }

func TestOutputsOpenSetReadAndCloseWithoutHardware(t *testing.T) {
	manager := New(config.GPIOConfig{Outputs: map[string]config.GPIOOutputConfig{
		"relay": {Chip: "fakechip", Line: 17, ActiveLow: true, Initial: true},
	}})
	requested := &behaviorLine{value: 1}
	var request lineRequest
	manager.requestLine = func(got lineRequest) (line, error) {
		request = got
		return requested, nil
	}
	if err := manager.OpenOutputs(); err != nil {
		t.Fatal(err)
	}
	if request.chip != "fakechip" || request.offset != 17 || request.consumer != "mqtt-raspberry-controller:relay" || request.direction != lineOutput || request.initial != 1 || !request.activeLow {
		t.Fatalf("output request = %+v", request)
	}
	if state, err := manager.OutputState("relay"); err != nil || !state {
		t.Fatalf("initial OutputState = %t, %v", state, err)
	}
	if err := manager.SetOutput("relay", false); err != nil {
		t.Fatal(err)
	}
	if len(requested.sets) != 1 || requested.sets[0] != 0 {
		t.Fatalf("set values = %v", requested.sets)
	}
	if err := manager.Close(); err != nil || requested.closed != 1 {
		t.Fatalf("Close = %v, count=%d", err, requested.closed)
	}
	if _, err := manager.OutputState("relay"); err == nil {
		t.Fatal("closed output remained readable")
	}
}

func TestInputsOpenReadEventAndCloseWithoutHardware(t *testing.T) {
	manager := New(config.GPIOConfig{Inputs: map[string]config.GPIOInputConfig{
		"contact": {Chip: "fakechip", Line: 27, Bias: "pull_up", Debounce: "5ms"},
	}})
	requested := &behaviorLine{value: 1}
	var request lineRequest
	manager.requestLine = func(got lineRequest) (line, error) {
		request = got
		return requested, nil
	}
	var eventName string
	var eventState bool
	if err := manager.OpenInputs(func(name string, state bool) {
		eventName, eventState = name, state
	}); err != nil {
		t.Fatal(err)
	}
	if request.chip != "fakechip" || request.offset != 27 || request.consumer != "mqtt-raspberry-controller:contact" || request.direction != lineInput || request.activeLow || request.bias != "pull_up" || request.debounce != 5*time.Millisecond || !request.bothEdges || request.eventHandler == nil {
		t.Fatalf("input request = %+v", request)
	}
	request.eventHandler(gpiocdev.LineEvent{Type: gpiocdev.LineEventRisingEdge})
	if eventName != "contact" || !eventState {
		t.Fatalf("input event = %q/%t", eventName, eventState)
	}
	if state, err := manager.InputState("contact"); err != nil || !state {
		t.Fatalf("InputState = %t, %v", state, err)
	}
	if err := manager.Close(); err != nil || requested.closed != 1 {
		t.Fatalf("Close = %v, count=%d", err, requested.closed)
	}
}

func TestGPIOStateAndCloseErrorsAreWrapped(t *testing.T) {
	manager := New(config.GPIOConfig{})
	setErr, readErr, closeErr := errors.New("set"), errors.New("read"), errors.New("close")
	output := &behaviorLine{setErr: setErr, readErr: readErr, closeErr: closeErr}
	input := &behaviorLine{readErr: readErr}
	manager.outputs["relay"] = output
	manager.inputs["contact"] = input
	if err := manager.SetOutput("relay", true); !errors.Is(err, setErr) || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("SetOutput error = %v", err)
	}
	if _, err := manager.OutputState("relay"); !errors.Is(err, readErr) {
		t.Fatalf("OutputState error = %v", err)
	}
	if _, err := manager.InputState("contact"); !errors.Is(err, readErr) {
		t.Fatalf("InputState error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, closeErr) || output.closed != 1 || input.closed != 1 {
		t.Fatalf("Close error = %v, close counts=%d/%d", err, output.closed, input.closed)
	}
}

func TestOpenOutputsRollbackClosesEveryPreviouslyOpenedLine(t *testing.T) {
	manager := New(config.GPIOConfig{Outputs: map[string]config.GPIOOutputConfig{
		"a": {Chip: "chip", Line: 1}, "b": {Chip: "chip", Line: 2},
	}})
	opened := &behaviorLine{}
	manager.requestLine = func(request lineRequest) (line, error) {
		if request.offset == 1 {
			return opened, nil
		}
		return nil, errors.New("request")
	}
	if err := manager.OpenOutputs(); err == nil || !strings.Contains(err.Error(), `output "b"`) {
		t.Fatalf("OpenOutputs error = %v", err)
	}
	if opened.closed != 1 || len(manager.outputs) != 0 {
		t.Fatalf("rollback closed=%d outputs=%d", opened.closed, len(manager.outputs))
	}
}
