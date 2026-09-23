package mqtt

import (
	"testing"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type fakePahoMessage struct {
	duplicate bool
	qos       byte
	retained  bool
	topic     string
	id        uint16
	payload   []byte
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(&config.Config{MQTT: config.MQTTConfig{
		Broker: "tcp://127.0.0.1:1883", ClientID: "test", BaseTopic: "test", AllowUnauthenticated: true,
		Discovery: config.DiscoveryConfig{Prefix: config.DefaultDiscoveryPrefix},
	}, Device: config.DeviceConfig{ID: "test", Name: "Test"},
		Entities: []config.EntityConfig{{ID: "button", Type: "button", Name: "Button", Press: []config.Action{{Type: "delay", Duration: "1ms"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (m *fakePahoMessage) Duplicate() bool   { return m.duplicate }
func (m *fakePahoMessage) Qos() byte         { return m.qos }
func (m *fakePahoMessage) Retained() bool    { return m.retained }
func (m *fakePahoMessage) Topic() string     { return m.topic }
func (m *fakePahoMessage) MessageID() uint16 { return m.id }
func (m *fakePahoMessage) Payload() []byte   { return m.payload }
func (m *fakePahoMessage) Ack()              {}

func TestMessageFromPahoPreservesMetadataAndCopiesPayload(t *testing.T) {
	source := &fakePahoMessage{duplicate: true, qos: 1, retained: true, topic: "device/button/press", id: 42, payload: []byte("PRESS")}
	got := messageFromPaho(source)
	source.payload[0] = 'X'
	if got.Topic != "device/button/press" || string(got.Payload) != "PRESS" || !got.Retained || !got.Duplicate || got.QoS != 1 || got.MessageID != 42 {
		t.Fatalf("message metadata not preserved: %+v", got)
	}
}

func TestDispatchMessageRejectsOversizedPayload(t *testing.T) {
	called := false
	source := &fakePahoMessage{topic: "device/button/press", payload: make([]byte, MaxCommandPayload+1)}
	c := newTestClient(t)
	if c.dispatchMessage(func(Message) { called = true }, source) {
		t.Fatal("oversized payload was accepted")
	}
	if called {
		t.Fatal("handler called for oversized payload")
	}
}

func TestCloseTimesOutWhileCallbackRemainsActive(t *testing.T) {
	c := newTestClient(t)
	if !c.beginCallback() {
		t.Fatal("initial callback rejected")
	}
	start := time.Now()
	err := c.Close(20 * time.Millisecond)
	if err == nil {
		t.Fatal("Close succeeded with callback still active")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("Close exceeded its deadline: %s", elapsed)
	}
	c.callbacks.Done()
}

func TestCloseWaitsForCallbacksAndStopsNewOnes(t *testing.T) {
	c := newTestClient(t)
	if !c.beginCallback() {
		t.Fatal("initial callback rejected")
	}
	done := make(chan struct{})
	go func() { _ = c.Close(time.Second); close(done) }()
	time.Sleep(10 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("Close returned with callback active")
	default:
	}
	if c.beginCallback() {
		t.Fatal("callback accepted after shutdown began")
	}
	c.callbacks.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after callback completed")
	}
}
