package gpio

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/warthog618/go-gpiocdev"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type InputHandler func(name string, state bool)

type line interface {
	SetValue(int) error
	Value() (int, error)
	Close() error
}

type lineDirection uint8

const (
	lineInput lineDirection = iota
	lineOutput
)

type lineRequest struct {
	chip         string
	offset       int
	consumer     string
	direction    lineDirection
	initial      int
	activeLow    bool
	bias         string
	debounce     time.Duration
	bothEdges    bool
	eventHandler func(gpiocdev.LineEvent)
}

type requestLineFunc func(lineRequest) (line, error)

func requestGPIOLine(request lineRequest) (line, error) {
	options := []gpiocdev.LineReqOption{gpiocdev.WithConsumer(request.consumer)}
	if request.activeLow {
		options = append(options, gpiocdev.AsActiveLow)
	} else {
		options = append(options, gpiocdev.AsActiveHigh)
	}
	if request.direction == lineOutput {
		options = append(options, gpiocdev.AsOutput(request.initial))
	} else {
		options = append(options, gpiocdev.AsInput)
		if request.bothEdges {
			options = append(options, gpiocdev.WithBothEdges)
		}
		options = append(options, gpiocdev.WithEventHandler(request.eventHandler))
		switch request.bias {
		case "pull_up":
			options = append(options, gpiocdev.WithPullUp)
		case "pull_down":
			options = append(options, gpiocdev.WithPullDown)
		default:
			options = append(options, gpiocdev.WithBiasDisabled)
		}
		if request.debounce > 0 {
			options = append(options, gpiocdev.WithDebounce(request.debounce))
		}
	}
	return gpiocdev.RequestLine(request.chip, request.offset, options...)
}

type Manager struct {
	mu sync.RWMutex

	config      config.GPIOConfig
	outputs     map[string]line
	inputs      map[string]line
	requestLine requestLineFunc
}

func New(cfg config.GPIOConfig) *Manager {
	return &Manager{
		config:      cfg,
		outputs:     make(map[string]line, len(cfg.Outputs)),
		inputs:      make(map[string]line, len(cfg.Inputs)),
		requestLine: requestGPIOLine,
	}
}

func sortedOutputNames(values map[string]config.GPIOOutputConfig) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedInputNames(values map[string]config.GPIOInputConfig) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) OpenOutputs() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, name := range sortedOutputNames(m.config.Outputs) {
		cfg := m.config.Outputs[name]
		if _, exists := m.outputs[name]; exists {
			continue
		}

		initial := 0
		if cfg.Initial {
			initial = 1
		}

		requestedLine, err := m.requestLine(lineRequest{
			chip:      cfg.Chip,
			offset:    cfg.Line,
			consumer:  "mqtt-raspberry-controller:" + name,
			direction: lineOutput,
			initial:   initial,
			activeLow: cfg.ActiveLow,
		})
		if err != nil {
			m.closeLocked()
			return fmt.Errorf("open GPIO output %q (%s:%d): %w", name, cfg.Chip, cfg.Line, err)
		}

		m.outputs[name] = requestedLine
		log.Printf("GPIO output %q opened on %s:%d (initial=%t active_low=%t)", name, cfg.Chip, cfg.Line, cfg.Initial, cfg.ActiveLow)
	}

	return nil
}

func (m *Manager) OpenInputs(handler InputHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, name := range sortedInputNames(m.config.Inputs) {
		cfg := m.config.Inputs[name]
		if _, exists := m.inputs[name]; exists {
			continue
		}

		inputName := name
		inputConfig := cfg

		eventHandler := inputEventHandler(inputName, handler)
		var debounce time.Duration
		if inputConfig.Debounce != "" {
			var err error
			debounce, err = time.ParseDuration(inputConfig.Debounce)
			if err != nil {
				m.closeInputsLocked()
				return fmt.Errorf("parse debounce for GPIO input %q: %w", inputName, err)
			}
		}

		requestedLine, err := m.requestLine(lineRequest{
			chip:         inputConfig.Chip,
			offset:       inputConfig.Line,
			consumer:     "mqtt-raspberry-controller:" + inputName,
			direction:    lineInput,
			activeLow:    inputConfig.ActiveLow,
			bias:         inputConfig.Bias,
			debounce:     debounce,
			bothEdges:    true,
			eventHandler: eventHandler,
		})
		if err != nil {
			m.closeInputsLocked()
			return fmt.Errorf("open GPIO input %q (%s:%d): %w", inputName, inputConfig.Chip, inputConfig.Line, err)
		}

		m.inputs[inputName] = requestedLine
		log.Printf("GPIO input %q opened on %s:%d (active_low=%t bias=%s)", inputName, inputConfig.Chip, inputConfig.Line, inputConfig.ActiveLow, inputConfig.Bias)
	}

	return nil
}

func inputEventHandler(name string, handler InputHandler) func(gpiocdev.LineEvent) {
	return func(event gpiocdev.LineEvent) {
		if handler == nil {
			return
		}
		state, ok := stateFromEvent(event)
		if !ok {
			log.Printf("GPIO input %q received unknown edge type %d", name, event.Type)
			return
		}
		// go-gpiocdev Close waits for callbacks while holding the line lock, so
		// callbacks must never call Value (or another line-locking method).
		handler(name, state)
	}
}

func stateFromEvent(event gpiocdev.LineEvent) (bool, bool) {
	switch event.Type {
	case gpiocdev.LineEventRisingEdge:
		return true, true
	case gpiocdev.LineEventFallingEdge:
		return false, true
	default:
		return false, false
	}
}

func (m *Manager) SetOutput(name string, state bool) error {
	m.mu.RLock()
	line, exists := m.outputs[name]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("GPIO output %q is not open", name)
	}

	value := 0
	if state {
		value = 1
	}

	if err := line.SetValue(value); err != nil {
		return fmt.Errorf("set GPIO output %q to %t: %w", name, state, err)
	}

	return nil
}

func (m *Manager) OutputState(name string) (bool, error) {
	m.mu.RLock()
	line, exists := m.outputs[name]
	m.mu.RUnlock()

	if !exists {
		return false, fmt.Errorf("GPIO output %q is not open", name)
	}

	value, err := line.Value()
	if err != nil {
		return false, fmt.Errorf("read GPIO output %q: %w", name, err)
	}

	return value == 1, nil
}

func (m *Manager) InputState(name string) (bool, error) {
	m.mu.RLock()
	line, exists := m.inputs[name]
	m.mu.RUnlock()

	if !exists {
		return false, fmt.Errorf("GPIO input %q is not open", name)
	}

	value, err := line.Value()
	if err != nil {
		return false, fmt.Errorf("read GPIO input %q: %w", name, err)
	}

	return value == 1, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.closeLocked()
}

func (m *Manager) closeInputsLocked() error {
	var firstErr error
	for name, line := range m.inputs {
		if err := line.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close GPIO input %q: %w", name, err)
		}
		delete(m.inputs, name)
	}
	return firstErr
}

func (m *Manager) closeLocked() error {
	firstErr := m.closeInputsLocked()

	for name, line := range m.outputs {
		if err := line.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close GPIO output %q: %w", name, err)
		}
		delete(m.outputs, name)
	}

	return firstErr
}
