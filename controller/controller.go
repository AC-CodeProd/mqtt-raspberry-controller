package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
	mqttclient "github.com/AC-CodeProd/mqtt-raspberry-controller/mqtt"
)

const (
	// EntityQueueCapacity bounds pending commands for each entity. New commands
	// are rejected when full; accepted commands are always processed FIFO.
	EntityQueueCapacity = 32
	DuplicateCacheSize  = 256
)

type mqttClient interface {
	RegisterSubscription(string, mqttclient.MessageHandler) error
	AddSessionHandler(mqttclient.SessionHandler)
	AddConnectHandler(mqttclient.ConnectHandler)
	Publish(string, any, byte, bool) error
	StatusTopic() string
}

type gpioManager interface {
	OutputState(string) (bool, error)
	InputState(string) (bool, error)
}

type actionExecutor interface {
	Execute(context.Context, []config.Action) error
}

type entityCommand struct {
	entity  config.EntityConfig
	message mqttclient.Message
}

type switchRuntime struct {
	mu       sync.Mutex
	state    bool
	queue    chan entityCommand
	overflow overflowLog
}

type buttonRuntime struct {
	mu       sync.Mutex
	queue    chan entityCommand
	overflow overflowLog
}

type overflowLog struct {
	mu      sync.Mutex
	dropped uint64
	last    time.Time
}

type dedupeEntry struct {
	key        string
	generation uint64
}

type Controller struct {
	cfg          *config.Config
	gpio         gpioManager
	mqtt         mqttClient
	executor     actionExecutor
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.RWMutex
	accepting    bool
	dedupeMu     sync.Mutex
	dedupe       map[string]uint64
	dedupeOrder  []dedupeEntry
	dedupeSeq    uint64
	rejectedLog  overflowLog
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	discoveryMu  sync.Mutex

	switches      map[string]*switchRuntime
	buttons       map[string]*buttonRuntime
	binaryByInput map[string][]config.EntityConfig
}

func New(
	ctx context.Context,
	cfg *config.Config,
	gpioManager gpioManager,
	mqttClient mqttClient,
	executor actionExecutor,
) *Controller {
	ctx, cancel := context.WithCancel(ctx)
	c := &Controller{
		cfg:           cfg,
		gpio:          gpioManager,
		mqtt:          mqttClient,
		executor:      executor,
		ctx:           ctx,
		cancel:        cancel,
		accepting:     true,
		dedupe:        make(map[string]uint64),
		shutdownDone:  make(chan struct{}),
		switches:      make(map[string]*switchRuntime),
		buttons:       make(map[string]*buttonRuntime),
		binaryByInput: make(map[string][]config.EntityConfig),
	}

	for _, entity := range cfg.Entities {
		switch entity.Type {
		case "switch":
			initial := false
			if entity.InitialState != nil {
				initial = *entity.InitialState
			}
			runtime := &switchRuntime{state: initial, queue: make(chan entityCommand, EntityQueueCapacity)}
			c.switches[entity.ID] = runtime
			c.wg.Add(1)
			go c.runSwitchWorker(runtime)
		case "button":
			runtime := &buttonRuntime{queue: make(chan entityCommand, EntityQueueCapacity)}
			c.buttons[entity.ID] = runtime
			c.wg.Add(1)
			go c.runButtonWorker(runtime)
		case "binary_sensor":
			c.binaryByInput[entity.Source.GPIO] = append(c.binaryByInput[entity.Source.GPIO], entity)
		}
	}

	return c
}

func (c *Controller) Register() error {
	for i := range c.cfg.Entities {
		entity := c.cfg.Entities[i]

		switch entity.Type {
		case "switch":
			if entity.State.Type == "gpio" {
				log.Printf("switch %q state uses GPIO output %q", entity.ID, entity.State.GPIO)
			}
			commandTopic := c.entityTopic(entity.ID, "set")
			entityCopy := entity
			if err := c.mqtt.RegisterSubscription(commandTopic, func(message mqttclient.Message) {
				c.enqueueSwitch(entityCopy, message)
			}); err != nil {
				return err
			}

		case "button":
			commandTopic := c.entityTopic(entity.ID, "press")
			entityCopy := entity
			if err := c.mqtt.RegisterSubscription(commandTopic, func(message mqttclient.Message) {
				c.enqueueButton(entityCopy, message)
			}); err != nil {
				return err
			}
		}
	}

	c.mqtt.AddSessionHandler(c.resetDedupe)
	c.mqtt.AddConnectHandler(func() {
		if !c.beginCallback() {
			return
		}
		defer c.wg.Done()
		if err := c.PublishDiscovery(); err != nil {
			log.Printf("publish Home Assistant discovery: %v", err)
		}
		c.PublishAllStates()
	})

	return nil
}

func (c *Controller) beginCallback() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.accepting || c.ctx.Err() != nil {
		return false
	}
	c.wg.Add(1)
	return true
}

func (c *Controller) enqueueSwitch(entity config.EntityConfig, message mqttclient.Message) {
	if !c.acceptMessage(entity, message) {
		return
	}
	runtime := c.switches[entity.ID]
	select {
	case runtime.queue <- entityCommand{entity: entity, message: message}:
	default:
		runtime.overflow.record("switch", entity.ID)
	}
}

func (c *Controller) enqueueButton(entity config.EntityConfig, message mqttclient.Message) {
	if !c.acceptMessage(entity, message) {
		return
	}
	runtime := c.buttons[entity.ID]
	select {
	case runtime.queue <- entityCommand{entity: entity, message: message}:
	default:
		runtime.overflow.record("button", entity.ID)
	}
}

func (o *overflowLog) record(kind, entityID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped++
	now := time.Now()
	if !o.last.IsZero() && now.Sub(o.last) < time.Second {
		return
	}
	log.Printf("reject %s %q command: FIFO queue is full (capacity=%d, dropped_since_last_log=%d)", kind, entityID, EntityQueueCapacity, o.dropped)
	o.dropped = 0
	o.last = now
}

func (o *overflowLog) recordRejection(format string, args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped++
	now := time.Now()
	if !o.last.IsZero() && now.Sub(o.last) < time.Second {
		return
	}
	if o.dropped > 1 {
		format += " (suppressed_since_last_log=%d)"
		args = append(args, o.dropped-1)
	}
	log.Printf(format, args...)
	o.dropped = 0
	o.last = now
}

func (c *Controller) acceptMessage(entity config.EntityConfig, message mqttclient.Message) bool {
	c.mu.RLock()
	accepting := c.accepting
	c.mu.RUnlock()
	if !accepting || c.ctx.Err() != nil {
		return false
	}
	if message.Retained {
		c.rejectedLog.recordRejection("reject retained MQTT control for %q payload=%q", entity.ID, boundedPayload(message.Payload))
		return false
	}
	if message.QoS > 0 {
		key := fmt.Sprintf("%s:%d", message.Topic, message.MessageID)
		c.dedupeMu.Lock()
		_, seen := c.dedupe[key]
		if !message.Duplicate || !seen {
			c.dedupeSeq++
			generation := c.dedupeSeq
			c.dedupe[key] = generation
			c.dedupeOrder = append(c.dedupeOrder, dedupeEntry{key: key, generation: generation})
			for len(c.dedupeOrder) > DuplicateCacheSize {
				oldest := c.dedupeOrder[0]
				c.dedupeOrder = c.dedupeOrder[1:]
				if c.dedupe[oldest.key] == oldest.generation {
					delete(c.dedupe, oldest.key)
				}
			}
		}
		c.dedupeMu.Unlock()
		if message.Duplicate && seen {
			c.rejectedLog.recordRejection("reject duplicate MQTT control for %q qos=%d message_id=%d", entity.ID, message.QoS, message.MessageID)
			return false
		}
	}
	return true
}

func (c *Controller) resetDedupe() {
	c.dedupeMu.Lock()
	c.dedupe = make(map[string]uint64)
	c.dedupeOrder = nil
	c.dedupeSeq = 0
	c.dedupeMu.Unlock()
}

func boundedPayload(payload []byte) string {
	const max = 128
	if len(payload) <= max {
		return string(payload)
	}
	return string(payload[:max]) + "..."
}

func (c *Controller) runSwitchWorker(runtime *switchRuntime) {
	defer c.wg.Done()
	for {
		if c.ctx.Err() != nil {
			return
		}
		select {
		case <-c.ctx.Done():
			return
		case command := <-runtime.queue:
			if c.ctx.Err() != nil {
				return
			}
			c.handleSwitch(command.entity, command.message.Payload)
		}
	}
}

func (c *Controller) runButtonWorker(runtime *buttonRuntime) {
	defer c.wg.Done()
	for {
		if c.ctx.Err() != nil {
			return
		}
		select {
		case <-c.ctx.Done():
			return
		case command := <-runtime.queue:
			if c.ctx.Err() != nil {
				return
			}
			c.handleButton(command.entity, command.message.Payload)
		}
	}
}

// Shutdown rejects new work, cancels active actions, discards queued commands,
// and waits for every per-entity worker up to the supplied deadline.
func (c *Controller) Shutdown(timeout time.Duration) error {
	c.shutdownOnce.Do(func() {
		c.mu.Lock()
		c.accepting = false
		c.mu.Unlock()
		c.cancel()
		go func() {
			c.wg.Wait()
			close(c.shutdownDone)
		}()
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.shutdownDone:
		return nil
	case <-timer.C:
		return fmt.Errorf("controller shutdown timed out after %s", timeout)
	}
}

func (c *Controller) HandleGPIOInput(name string, state bool) {
	if !c.beginCallback() {
		return
	}
	defer c.wg.Done()
	entities := c.binaryByInput[name]
	for _, entity := range entities {
		if err := c.publishState(entity, state); err != nil {
			log.Printf("publish binary_sensor %q state: %v", entity.ID, err)
		}
	}
}

func (c *Controller) PublishAllStates() {
	for _, entity := range c.cfg.Entities {
		switch entity.Type {
		case "switch":
			state, err := c.switchState(entity)
			if err != nil {
				log.Printf("read switch %q state: %v", entity.ID, err)
				continue
			}
			if err := c.publishState(entity, state); err != nil {
				log.Printf("publish switch %q state: %v", entity.ID, err)
			}

		case "binary_sensor":
			state, err := c.gpio.InputState(entity.Source.GPIO)
			if err != nil {
				// Inputs are opened after the initial MQTT connection, so this can be temporary.
				continue
			}
			if err := c.publishState(entity, state); err != nil {
				log.Printf("publish binary_sensor %q state: %v", entity.ID, err)
			}
		}
	}
}

func (c *Controller) PublishDiscovery() error {
	c.discoveryMu.Lock()
	defer c.discoveryMu.Unlock()
	return c.reconcileDiscovery()
}

func (c *Controller) handleSwitch(entity config.EntityConfig, payload []byte) {
	desired, ok := parseSwitchPayload(payload)
	if !ok {
		c.rejectedLog.recordRejection("switch %q received invalid payload %q", entity.ID, boundedPayload([]byte(strings.TrimSpace(string(payload)))))
		return
	}

	runtime := c.switches[entity.ID]
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	actions := entity.Off
	if desired {
		actions = entity.On
	}

	if err := c.executor.Execute(c.ctx, actions); err != nil {
		log.Printf("switch %q failed to change to %s: %v", entity.ID, statePayload(desired), err)

		// Republish the actual/current state so Home Assistant rolls back its UI.
		current, stateErr := c.switchStateLocked(entity, runtime)
		if stateErr != nil {
			log.Printf("read switch %q final state after failed action: %v", entity.ID, stateErr)
			return
		}
		if publishErr := c.publishState(entity, current); publishErr != nil {
			log.Printf("republish switch %q state: %v", entity.ID, publishErr)
		}
		return
	}

	if entity.State.Type == "memory" {
		runtime.state = desired
	}

	current, err := c.switchStateLocked(entity, runtime)
	if err != nil {
		log.Printf("read switch %q state after command: %v", entity.ID, err)
		return
	}

	if err := c.publishState(entity, current); err != nil {
		log.Printf("publish switch %q state: %v", entity.ID, err)
		return
	}

	log.Printf("switch %q changed to %s", entity.ID, statePayload(current))
}

func (c *Controller) handleButton(entity config.EntityConfig, payload []byte) {
	if strings.TrimSpace(strings.ToUpper(string(payload))) != "PRESS" {
		c.rejectedLog.recordRejection("button %q received invalid payload %q", entity.ID, boundedPayload([]byte(strings.TrimSpace(string(payload)))))
		return
	}

	runtime := c.buttons[entity.ID]
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if err := c.executor.Execute(c.ctx, entity.Press); err != nil {
		log.Printf("button %q action failed: %v", entity.ID, err)
		return
	}

	log.Printf("button %q pressed", entity.ID)
}

func (c *Controller) switchState(entity config.EntityConfig) (bool, error) {
	runtime := c.switches[entity.ID]
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return c.switchStateLocked(entity, runtime)
}

func (c *Controller) switchStateLocked(entity config.EntityConfig, runtime *switchRuntime) (bool, error) {
	switch entity.State.Type {
	case "memory":
		return runtime.state, nil
	case "gpio":
		return c.gpio.OutputState(entity.State.GPIO)
	default:
		return false, fmt.Errorf("unsupported state type %q", entity.State.Type)
	}
}

func (c *Controller) publishState(entity config.EntityConfig, state bool) error {
	return c.mqtt.Publish(
		c.entityTopic(entity.ID, "state"),
		statePayload(state),
		c.cfg.MQTTQoS(),
		true,
	)
}

func (c *Controller) discoveryPayload(entity config.EntityConfig) map[string]any {
	device := map[string]any{
		"identifiers":  []string{c.cfg.Device.ID},
		"name":         c.cfg.Device.Name,
		"manufacturer": c.cfg.Device.Manufacturer,
		"model":        c.cfg.Device.Model,
	}

	base := map[string]any{
		"name":                  entity.Name,
		"unique_id":             c.cfg.Device.ID + "_" + entity.ID,
		"availability_topic":    c.mqtt.StatusTopic(),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device":                device,
		"qos":                   c.cfg.MQTTQoS(),
	}

	if entity.Icon != "" {
		base["icon"] = entity.Icon
	}
	if entity.DeviceClass != "" {
		base["device_class"] = entity.DeviceClass
	}

	switch entity.Type {
	case "switch":
		base["command_topic"] = c.entityTopic(entity.ID, "set")
		base["state_topic"] = c.entityTopic(entity.ID, "state")
		base["payload_on"] = "ON"
		base["payload_off"] = "OFF"
		base["optimistic"] = false

	case "button":
		base["command_topic"] = c.entityTopic(entity.ID, "press")
		base["payload_press"] = "PRESS"

	case "binary_sensor":
		base["state_topic"] = c.entityTopic(entity.ID, "state")
		base["payload_on"] = "ON"
		base["payload_off"] = "OFF"
	}

	return base
}

func (c *Controller) entityTopic(entityID, suffix string) string {
	return c.cfg.MQTT.BaseTopic + "/" + entityID + "/" + suffix
}

func parseSwitchPayload(payload []byte) (bool, bool) {
	switch strings.ToUpper(strings.TrimSpace(string(payload))) {
	case "ON", "1", "TRUE":
		return true, true
	case "OFF", "0", "FALSE":
		return false, true
	default:
		return false, false
	}
}

func statePayload(state bool) string {
	if state {
		return "ON"
	}
	return "OFF"
}
