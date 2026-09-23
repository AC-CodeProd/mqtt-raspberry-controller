package controller

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
	mqttclient "github.com/AC-CodeProd/mqtt-raspberry-controller/mqtt"
)

type fakeMQTT struct {
	mu        sync.Mutex
	subs      map[string]mqttclient.MessageHandler
	session   []mqttclient.SessionHandler
	connect   []mqttclient.ConnectHandler
	published []string
}

func newFakeMQTT() *fakeMQTT { return &fakeMQTT{subs: make(map[string]mqttclient.MessageHandler)} }
func (m *fakeMQTT) RegisterSubscription(topic string, h mqttclient.MessageHandler) error {
	m.subs[topic] = h
	return nil
}
func (m *fakeMQTT) AddSessionHandler(h mqttclient.SessionHandler) { m.session = append(m.session, h) }
func (m *fakeMQTT) AddConnectHandler(h mqttclient.ConnectHandler) { m.connect = append(m.connect, h) }
func (m *fakeMQTT) Publish(topic string, _ any, _ byte, _ bool) error {
	m.mu.Lock()
	m.published = append(m.published, topic)
	m.mu.Unlock()
	return nil
}
func (m *fakeMQTT) StatusTopic() string { return "base/status" }
func (m *fakeMQTT) connectNow() {
	for _, session := range m.session {
		session()
	}
	for _, connect := range m.connect {
		connect()
	}
}

type fakeGPIO struct{}

func (fakeGPIO) OutputState(string) (bool, error) { return false, nil }
func (fakeGPIO) InputState(string) (bool, error)  { return false, nil }

type fakeExecutor struct {
	mu    sync.Mutex
	calls []string
	hook  func(context.Context, string) error
}

func (e *fakeExecutor) Execute(ctx context.Context, actions []config.Action) error {
	value := actions[0].Payload
	e.mu.Lock()
	e.calls = append(e.calls, value)
	e.mu.Unlock()
	if e.hook != nil {
		return e.hook(ctx, value)
	}
	return nil
}
func (e *fakeExecutor) count() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.calls) }
func (e *fakeExecutor) values() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func switchConfig(ids ...string) *config.Config {
	cfg := &config.Config{MQTT: config.MQTTConfig{BaseTopic: "base"}, Device: config.DeviceConfig{ID: "device"}}
	for _, id := range ids {
		cfg.Entities = append(cfg.Entities, config.EntityConfig{ID: id, Type: "switch", State: config.StateConfig{Type: "memory"}, On: []config.Action{{Type: "mqtt", Payload: id + ":ON"}}, Off: []config.Action{{Type: "mqtt", Payload: id + ":OFF"}}})
	}
	return cfg
}
func buttonConfig(ids ...string) *config.Config {
	cfg := &config.Config{MQTT: config.MQTTConfig{BaseTopic: "base"}, Device: config.DeviceConfig{ID: "device"}}
	for _, id := range ids {
		cfg.Entities = append(cfg.Entities, config.EntityConfig{ID: id, Type: "button", Press: []config.Action{{Type: "mqtt", Payload: id}}})
	}
	return cfg
}
func waitCount(t *testing.T, e *fakeExecutor, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.count() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("executor calls = %d, want %d", e.count(), want)
}

func TestSwitchCommandsAreFIFO(t *testing.T) {
	m, e := newFakeMQTT(), &fakeExecutor{}
	c := New(context.Background(), switchConfig("lamp"), fakeGPIO{}, m, e)
	defer c.Shutdown(time.Second)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	h := m.subs["base/lamp/set"]
	for _, payload := range []string{"ON", "OFF", "ON"} {
		h(mqttclient.Message{Topic: "base/lamp/set", Payload: []byte(payload)})
	}
	waitCount(t, e, 3)
	got := e.values()
	want := []string{"lamp:ON", "lamp:OFF", "lamp:ON"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

func TestRetainedAndQoSDuplicatesAreScopedToConnection(t *testing.T) {
	m, e := newFakeMQTT(), &fakeExecutor{}
	c := New(context.Background(), buttonConfig("bell"), fakeGPIO{}, m, e)
	defer c.Shutdown(time.Second)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	h := m.subs["base/bell/press"]

	// Establish the first clean session, then accept one packet and reject its retransmission.
	m.connectNow()
	h(mqttclient.Message{Topic: "base/bell/press", Payload: []byte("PRESS"), QoS: 1, MessageID: 9})
	h(mqttclient.Message{Topic: "base/bell/press", Payload: []byte("PRESS"), QoS: 1, MessageID: 9, Duplicate: true})
	h(mqttclient.Message{Topic: "base/bell/press", Payload: []byte("PRESS"), Retained: true})

	// A reconnect starts a new packet-ID scope. An unseen DUP must be accepted to avoid data loss.
	m.connectNow()
	h(mqttclient.Message{Topic: "base/bell/press", Payload: []byte("PRESS"), QoS: 1, MessageID: 9, Duplicate: true})
	h(mqttclient.Message{Topic: "base/bell/press", Payload: []byte("PRESS"), QoS: 1, MessageID: 9, Duplicate: true})
	h(mqttclient.Message{Topic: "base/bell/press", Payload: []byte("PRESS"), QoS: 0, Duplicate: true})

	waitCount(t, e, 3)
	time.Sleep(20 * time.Millisecond)
	if got := e.count(); got != 3 {
		t.Fatalf("executor calls = %d, want 3", got)
	}
}

func TestRetainedSwitchCommandIsRejected(t *testing.T) {
	m, e := newFakeMQTT(), &fakeExecutor{}
	c := New(context.Background(), switchConfig("lamp"), fakeGPIO{}, m, e)
	defer c.Shutdown(time.Second)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	m.subs["base/lamp/set"](mqttclient.Message{
		Topic: "base/lamp/set", Payload: []byte("ON"), Retained: true,
	})
	time.Sleep(20 * time.Millisecond)
	if got := e.count(); got != 0 {
		t.Fatalf("retained switch command executed %d actions", got)
	}
}

func TestDuplicateCacheRefreshesReusedPacketID(t *testing.T) {
	c := New(context.Background(), &config.Config{}, fakeGPIO{}, newFakeMQTT(), &fakeExecutor{})
	defer c.Shutdown(time.Second)
	entity := config.EntityConfig{ID: "bell"}
	for id := 1; id <= DuplicateCacheSize; id++ {
		if !c.acceptMessage(entity, mqttclient.Message{Topic: "base/bell/press", QoS: 1, MessageID: uint16(id)}) {
			t.Fatalf("initial packet %d rejected", id)
		}
	}
	// Reusing packet ID 1 for a new non-DUP command must refresh its cache generation.
	if !c.acceptMessage(entity, mqttclient.Message{Topic: "base/bell/press", QoS: 1, MessageID: 1}) {
		t.Fatal("reused non-DUP packet ID rejected")
	}
	if !c.acceptMessage(entity, mqttclient.Message{Topic: "base/bell/press", QoS: 1, MessageID: DuplicateCacheSize + 1}) {
		t.Fatal("new packet rejected")
	}
	if c.acceptMessage(entity, mqttclient.Message{Topic: "base/bell/press", QoS: 1, MessageID: 1, Duplicate: true}) {
		t.Fatal("duplicate of refreshed packet ID accepted")
	}
}

func TestQueueSaturationIsBoundedAndRejectsNewest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	e := &fakeExecutor{hook: func(context.Context, string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}}
	m := newFakeMQTT()
	c := New(context.Background(), buttonConfig("bell"), fakeGPIO{}, m, e)
	defer c.Shutdown(time.Second)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	h := m.subs["base/bell/press"]
	h(mqttclient.Message{Payload: []byte("PRESS")})
	<-entered
	before := runtime.NumGoroutine()
	for i := 0; i < 10_000; i++ {
		h(mqttclient.Message{Payload: []byte("PRESS")})
	}
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutines grew under saturation: before=%d after=%d", before, after)
	}
	if got := len(c.buttons["bell"].queue); got != EntityQueueCapacity {
		t.Fatalf("queue length = %d, want %d", got, EntityQueueCapacity)
	}
	close(release)
	waitCount(t, e, EntityQueueCapacity+1)
}

func TestDifferentEntitiesProgressIndependently(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	e := &fakeExecutor{hook: func(_ context.Context, id string) error {
		if id == "a" {
			close(blocked)
			<-release
		}
		return nil
	}}
	m := newFakeMQTT()
	c := New(context.Background(), buttonConfig("a", "b"), fakeGPIO{}, m, e)
	defer c.Shutdown(time.Second)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	m.subs["base/a/press"](mqttclient.Message{Payload: []byte("PRESS")})
	<-blocked
	m.subs["base/b/press"](mqttclient.Message{Payload: []byte("PRESS")})
	waitCount(t, e, 2)
	close(release)
}

func TestCancelledSwitchRepublishesFinalObservableState(t *testing.T) {
	entered := make(chan struct{})
	e := &fakeExecutor{hook: func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	m := newFakeMQTT()
	c := New(context.Background(), switchConfig("lamp"), fakeGPIO{}, m, e)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	m.subs["base/lamp/set"](mqttclient.Message{Payload: []byte("ON")})
	<-entered
	if err := c.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for _, topic := range m.published {
		if topic == "base/lamp/state" {
			found = true
		}
	}
	if !found {
		t.Fatal("cancelled switch did not republish its final state")
	}
}

func TestSignalContextCancelsActiveAndDropsQueuedWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM integration test")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	entered := make(chan struct{})
	e := &fakeExecutor{hook: func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	m := newFakeMQTT()
	c := New(ctx, buttonConfig("bell"), fakeGPIO{}, m, e)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	h := m.subs["base/bell/press"]
	h(mqttclient.Message{Payload: []byte("PRESS")})
	<-entered
	h(mqttclient.Message{Payload: []byte("PRESS")})
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("SIGTERM did not cancel the root context")
	}
	if err := c.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	if got := e.count(); got != 1 {
		t.Fatalf("executor calls = %d, want active command only", got)
	}
}

func TestShutdownCancelsActiveAndDropsQueued(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	e := &fakeExecutor{hook: func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}}
	m := newFakeMQTT()
	c := New(context.Background(), buttonConfig("bell"), fakeGPIO{}, m, e)
	if err := c.Register(); err != nil {
		t.Fatal(err)
	}
	h := m.subs["base/bell/press"]
	h(mqttclient.Message{Payload: []byte("PRESS")})
	<-entered
	h(mqttclient.Message{Payload: []byte("PRESS")})
	start := time.Now()
	if err := c.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("shutdown did not promptly cancel active action")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("active action did not receive cancellation")
	}
	if got := e.count(); got != 1 {
		t.Fatalf("executor calls = %d, want 1", got)
	}
	h(mqttclient.Message{Payload: []byte("PRESS")})
	if got := e.count(); got != 1 {
		t.Fatalf("command accepted after shutdown: calls=%d", got)
	}
	if err := c.Shutdown(time.Second); err != nil {
		t.Fatalf("second shutdown failed: %v", err)
	}
}
