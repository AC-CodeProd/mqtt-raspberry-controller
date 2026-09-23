package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
	mqttclient "github.com/AC-CodeProd/mqtt-raspberry-controller/mqtt"
)

type recordedPublish struct {
	topic   string
	payload any
	qos     byte
	retain  bool
}
type behaviorMQTT struct {
	mu          sync.Mutex
	subs        map[string]mqttclient.MessageHandler
	sessions    []mqttclient.SessionHandler
	connects    []mqttclient.ConnectHandler
	published   []recordedPublish
	registerErr map[string]error
	publishErr  map[string]error
}

func newBehaviorMQTT() *behaviorMQTT {
	return &behaviorMQTT{subs: map[string]mqttclient.MessageHandler{}, registerErr: map[string]error{}, publishErr: map[string]error{}}
}
func (m *behaviorMQTT) RegisterSubscription(topic string, h mqttclient.MessageHandler) error {
	if err := m.registerErr[topic]; err != nil {
		return err
	}
	m.subs[topic] = h
	return nil
}
func (m *behaviorMQTT) AddSessionHandler(h mqttclient.SessionHandler) {
	m.sessions = append(m.sessions, h)
}
func (m *behaviorMQTT) AddConnectHandler(h mqttclient.ConnectHandler) {
	m.connects = append(m.connects, h)
}
func (m *behaviorMQTT) Publish(topic string, payload any, qos byte, retain bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = append(m.published, recordedPublish{topic, payload, qos, retain})
	return m.publishErr[topic]
}
func (m *behaviorMQTT) StatusTopic() string { return "house/status" }
func (m *behaviorMQTT) snapshot() []recordedPublish {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedPublish(nil), m.published...)
}
func (m *behaviorMQTT) reconnect() {
	for _, h := range m.sessions {
		h()
	}
	for _, h := range m.connects {
		h()
	}
}

type behaviorGPIO struct {
	outputs   map[string]bool
	inputs    map[string]bool
	outputErr map[string]error
	inputErr  map[string]error
}

func (g *behaviorGPIO) OutputState(name string) (bool, error) {
	return g.outputs[name], g.outputErr[name]
}
func (g *behaviorGPIO) InputState(name string) (bool, error) { return g.inputs[name], g.inputErr[name] }

type behaviorExecutor struct {
	mu    sync.Mutex
	calls [][]config.Action
	err   error
}

func (e *behaviorExecutor) Execute(_ context.Context, actions []config.Action) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, append([]config.Action(nil), actions...))
	return e.err
}

func fullControllerConfig(discovery bool) *config.Config {
	qos := byte(1)
	return &config.Config{
		MQTT:   config.MQTTConfig{ClientID: "node-client", BaseTopic: "house", QoS: &qos, Discovery: config.DiscoveryConfig{Enabled: &discovery, Prefix: "ha"}},
		Device: config.DeviceConfig{ID: "node", Name: "Node", Manufacturer: "Maker", Model: "Model"},
		Entities: []config.EntityConfig{
			{ID: "relay", Type: "switch", Name: "Relay", Icon: "mdi:switch", State: config.StateConfig{Type: "gpio", GPIO: "out"}, On: []config.Action{{Type: "gpio", GPIO: "out"}}, Off: []config.Action{{Type: "gpio", GPIO: "out"}}},
			{ID: "bell", Type: "button", Name: "Bell", Press: []config.Action{{Type: "delay", Payload: "bell", Duration: "1ms"}}},
			{ID: "door", Type: "binary_sensor", Name: "Door", DeviceClass: "opening", Source: config.SourceConfig{Type: "gpio", GPIO: "in"}},
		},
	}
}

func TestRegisterUsesExactControlSubscriptions(t *testing.T) {
	mqtt, gpio, executor := newBehaviorMQTT(), &behaviorGPIO{}, &behaviorExecutor{}
	controller := New(context.Background(), fullControllerConfig(true), gpio, mqtt, executor)
	defer controller.Shutdown(time.Second)
	if err := controller.Register(); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(mqtt.subs))
	for topic := range mqtt.subs {
		got = append(got, topic)
	}
	sort.Strings(got)
	want := []string{"house/bell/press", "house/relay/set"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subscriptions = %v, want %v", got, want)
	}
	if len(mqtt.sessions) != 1 || len(mqtt.connects) != 1 {
		t.Fatalf("handlers session=%d connect=%d", len(mqtt.sessions), len(mqtt.connects))
	}
}

func TestRegisterReturnsSubscriptionFailureWithoutLaterRegistrations(t *testing.T) {
	mqtt := newBehaviorMQTT()
	failure := errors.New("denied")
	mqtt.registerErr["house/relay/set"] = failure
	controller := New(context.Background(), fullControllerConfig(true), &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer controller.Shutdown(time.Second)
	if err := controller.Register(); !errors.Is(err, failure) {
		t.Fatalf("Register error = %v", err)
	}
	if len(mqtt.subs) != 0 || len(mqtt.sessions) != 0 || len(mqtt.connects) != 0 {
		t.Fatalf("registration continued after failure: %+v", mqtt.subs)
	}
}

func TestDiscoveryTopicsAndJSONForEveryEntity(t *testing.T) {
	mqtt := newBehaviorMQTT()
	controller := New(context.Background(), fullControllerConfig(true), &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer controller.Shutdown(time.Second)
	if err := controller.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	publications := mqtt.snapshot()
	wantTopics := []string{"ha/switch/node/relay/config", "ha/button/node/bell/config", "ha/binary_sensor/node/door/config"}
	if len(publications) != len(wantTopics) {
		t.Fatalf("discovery publications = %d", len(publications))
	}
	for index, publication := range publications {
		if publication.topic != wantTopics[index] || publication.qos != 1 || !publication.retain {
			t.Fatalf("publication[%d] = %+v", index, publication)
		}
		encoded, ok := publication.payload.([]byte)
		if !ok {
			t.Fatalf("payload type = %T", publication.payload)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if got["name"] != fullControllerConfig(true).Entities[index].Name || got["unique_id"] != "node_"+fullControllerConfig(true).Entities[index].ID || got["availability_topic"] != "house/status" {
			t.Fatalf("base discovery JSON = %#v", got)
		}
		device := got["device"].(map[string]any)
		if device["name"] != "Node" || device["manufacturer"] != "Maker" || device["model"] != "Model" || !reflect.DeepEqual(device["identifiers"], []any{"node"}) {
			t.Fatalf("device JSON = %#v", device)
		}
		switch index {
		case 0:
			if got["command_topic"] != "house/relay/set" || got["state_topic"] != "house/relay/state" || got["payload_on"] != "ON" || got["payload_off"] != "OFF" || got["optimistic"] != false || got["icon"] != "mdi:switch" {
				t.Fatalf("switch JSON = %#v", got)
			}
		case 1:
			if got["command_topic"] != "house/bell/press" || got["payload_press"] != "PRESS" {
				t.Fatalf("button JSON = %#v", got)
			}
		case 2:
			if got["state_topic"] != "house/door/state" || got["device_class"] != "opening" {
				t.Fatalf("sensor JSON = %#v", got)
			}
		}
	}
}

func TestDiscoveryDisabledAndPartialFailure(t *testing.T) {
	mqtt := newBehaviorMQTT()
	controller := New(context.Background(), fullControllerConfig(false), &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer controller.Shutdown(time.Second)
	if err := controller.PublishDiscovery(); err != nil || len(mqtt.snapshot()) != 0 {
		t.Fatalf("disabled discovery = %v, %#v", err, mqtt.snapshot())
	}
	controller.cfg = fullControllerConfig(true)
	failure := errors.New("broker full")
	mqtt.publishErr["ha/button/node/bell/config"] = failure
	if err := controller.PublishDiscovery(); !errors.Is(err, failure) || !strings.Contains(err.Error(), `"bell"`) {
		t.Fatalf("discovery error = %v", err)
	}
	if got := len(mqtt.snapshot()); got != 2 {
		t.Fatalf("publications before failure = %d, want 2", got)
	}
}

func TestAllStatesGPIOInputsAndPartialErrors(t *testing.T) {
	mqtt := newBehaviorMQTT()
	gpio := &behaviorGPIO{outputs: map[string]bool{"out": true}, inputs: map[string]bool{"in": false}, outputErr: map[string]error{}, inputErr: map[string]error{}}
	controller := New(context.Background(), fullControllerConfig(true), gpio, mqtt, &behaviorExecutor{})
	defer controller.Shutdown(time.Second)
	controller.PublishAllStates()
	got := mqtt.snapshot()
	if len(got) != 2 || got[0].topic != "house/relay/state" || got[0].payload != "ON" || got[1].topic != "house/door/state" || got[1].payload != "OFF" {
		t.Fatalf("states = %#v", got)
	}
	mqtt.published = nil
	controller.HandleGPIOInput("in", true)
	got = mqtt.snapshot()
	if len(got) != 1 || got[0].topic != "house/door/state" || got[0].payload != "ON" {
		t.Fatalf("input publication = %#v", got)
	}
	mqtt.published = nil
	gpio.outputErr["out"] = errors.New("read failed")
	controller.PublishAllStates()
	got = mqtt.snapshot()
	if len(got) != 1 || got[0].topic != "house/door/state" {
		t.Fatalf("state publishing did not continue after read failure: %#v", got)
	}
	mqtt.published = nil
	delete(gpio.outputErr, "out")
	mqtt.publishErr["house/relay/state"] = errors.New("publish failed")
	controller.PublishAllStates()
	got = mqtt.snapshot()
	if len(got) != 2 || got[1].topic != "house/door/state" {
		t.Fatalf("state publishing did not continue after publish failure: %#v", got)
	}
}

func TestSwitchButtonCommandsAndReconnectRepublish(t *testing.T) {
	mqtt := newBehaviorMQTT()
	gpio := &behaviorGPIO{outputs: map[string]bool{"out": true}, inputs: map[string]bool{"in": true}, outputErr: map[string]error{}, inputErr: map[string]error{}}
	executor := &behaviorExecutor{}
	controller := New(context.Background(), fullControllerConfig(true), gpio, mqtt, executor)
	defer controller.Shutdown(time.Second)
	if err := controller.Register(); err != nil {
		t.Fatal(err)
	}
	mqtt.subs["house/relay/set"](mqttclient.Message{Payload: []byte(" ON ")})
	mqtt.subs["house/bell/press"](mqttclient.Message{Payload: []byte("PRESS")})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		executor.mu.Lock()
		count := len(executor.calls)
		executor.mu.Unlock()
		if count == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	executor.mu.Lock()
	if len(executor.calls) != 2 {
		t.Fatalf("executor calls = %d", len(executor.calls))
	}
	executor.mu.Unlock()
	before := len(mqtt.snapshot())
	mqtt.reconnect()
	after := mqtt.snapshot()
	if len(after)-before != 5 {
		t.Fatalf("reconnect publications = %d, want discovery(3)+states(2): %#v", len(after)-before, after[before:])
	}
}
