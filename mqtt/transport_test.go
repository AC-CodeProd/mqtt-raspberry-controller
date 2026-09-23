package mqtt

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

type fakeToken struct {
	err      error
	complete bool
	done     chan struct{}
}

func completedToken(err error) *fakeToken {
	done := make(chan struct{})
	close(done)
	return &fakeToken{err: err, complete: true, done: done}
}
func pendingToken() *fakeToken                      { return &fakeToken{done: make(chan struct{})} }
func (t *fakeToken) Wait() bool                     { <-t.done; return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return t.complete }
func (t *fakeToken) Done() <-chan struct{}          { return t.done }
func (t *fakeToken) Error() error                   { return t.err }

type transportPublish struct {
	topic   string
	qos     byte
	retain  bool
	payload any
}

type fakeTransport struct {
	mu             sync.Mutex
	connected      bool
	connectToken   paho.Token
	publishToken   paho.Token
	subscribeToken paho.Token
	publishes      []transportPublish
	subscriptions  []string
	handlers       map[string]paho.MessageHandler
	disconnects    []uint
	events         *[]string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{connected: true, connectToken: completedToken(nil), publishToken: completedToken(nil), subscribeToken: completedToken(nil), handlers: make(map[string]paho.MessageHandler)}
}
func (f *fakeTransport) record(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}
func (f *fakeTransport) Connect() paho.Token { f.record("connect"); return f.connectToken }
func (f *fakeTransport) IsConnected() bool   { return f.connected }
func (f *fakeTransport) Publish(topic string, qos byte, retain bool, payload any) paho.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, transportPublish{topic, qos, retain, payload})
	f.record("publish:" + topic)
	return f.publishToken
}
func (f *fakeTransport) Subscribe(topic string, _ byte, handler paho.MessageHandler) paho.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscriptions = append(f.subscriptions, topic)
	f.handlers[topic] = handler
	f.record("subscribe:" + topic)
	return f.subscribeToken
}
func (f *fakeTransport) Disconnect(quiesce uint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, quiesce)
	f.connected = false
	f.record("disconnect")
}

func clientWithTransport(t *testing.T, transport *fakeTransport) *Client {
	t.Helper()
	client := newTestClient(t)
	client.transport = transport
	return client
}

func TestConnectWaitsForTokenAndReturnsError(t *testing.T) {
	connectErr := errors.New("denied")
	transport := newFakeTransport()
	transport.connectToken = completedToken(connectErr)
	client := clientWithTransport(t, transport)
	if err := client.Connect(); !errors.Is(err, connectErr) || !strings.Contains(err.Error(), "connect to MQTT broker") {
		t.Fatalf("Connect error = %v", err)
	}
}

func TestRegisterSubscriptionConnectedAndDelivery(t *testing.T) {
	transport := newFakeTransport()
	client := clientWithTransport(t, transport)
	received := make(chan Message, 1)
	if err := client.RegisterSubscription("test/button/press", func(message Message) { received <- message }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transport.subscriptions, []string{"test/button/press"}) {
		t.Fatalf("subscriptions = %v", transport.subscriptions)
	}
	transport.handlers["test/button/press"](nil, &fakePahoMessage{topic: "test/button/press", payload: []byte("PRESS"), qos: 1, id: 7})
	select {
	case got := <-received:
		if got.Topic != "test/button/press" || string(got.Payload) != "PRESS" || got.QoS != 1 || got.MessageID != 7 {
			t.Fatalf("message = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription callback not delivered")
	}
	if err := client.RegisterSubscription("test/button/press", func(Message) {}); err == nil {
		t.Fatal("duplicate subscription accepted")
	}
}

func TestPublishConnectionTimeoutAndTokenErrors(t *testing.T) {
	transport := newFakeTransport()
	client := clientWithTransport(t, transport)
	transport.connected = false
	if err := client.Publish("test/topic", "x", 1, false); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("disconnected error = %v", err)
	}
	transport.connected = true
	transport.publishToken = pendingToken()
	if err := client.publishWithTimeout("test/topic", "x", 1, false, time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	publishErr := errors.New("rejected")
	transport.publishToken = completedToken(publishErr)
	if err := client.Publish("test/topic", "x", 2, true); !errors.Is(err, publishErr) {
		t.Fatalf("token error = %v", err)
	}
	got := transport.publishes[len(transport.publishes)-1]
	if got.topic != "test/topic" || got.qos != 2 || !got.retain || got.payload != "x" {
		t.Fatalf("publish = %+v", got)
	}
}

func TestSubscribeTokenErrors(t *testing.T) {
	transport := newFakeTransport()
	transport.subscribeToken = pendingToken()
	client := clientWithTransport(t, transport)
	if err := client.RegisterSubscription("test/a", func(Message) {}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout = %v", err)
	}

	transport = newFakeTransport()
	subscribeErr := errors.New("denied")
	transport.subscribeToken = completedToken(subscribeErr)
	client = clientWithTransport(t, transport)
	if err := client.RegisterSubscription("test/b", func(Message) {}); !errors.Is(err, subscribeErr) {
		t.Fatalf("subscribe error = %v", err)
	}
}

func TestReconnectSequenceSessionSubscribeOnlineConnect(t *testing.T) {
	events := []string{}
	transport := newFakeTransport()
	transport.events = &events
	client := clientWithTransport(t, transport)
	transport.connected = false
	if err := client.RegisterSubscription("test/button/press", func(Message) {}); err != nil {
		t.Fatal(err)
	}
	transport.connected = true
	client.AddSessionHandler(func() { events = append(events, "session") })
	client.AddConnectHandler(func() { events = append(events, "handler") })
	client.handleConnect(transport)
	want := []string{"session", "subscribe:test/button/press", "publish:test/status", "handler"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	client.handleConnect(transport)
	want = append(want, "session", "subscribe:test/button/press", "publish:test/status", "handler")
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("reconnect events = %v, want %v", events, want)
	}
}

func TestClosePublishesOfflineBeforeDisconnect(t *testing.T) {
	events := []string{}
	transport := newFakeTransport()
	transport.events = &events
	client := clientWithTransport(t, transport)
	if err := client.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"publish:test/status", "disconnect"}) {
		t.Fatalf("events = %v", events)
	}
	if len(transport.disconnects) != 1 || transport.disconnects[0] == 0 || transport.disconnects[0] > 500 {
		t.Fatalf("disconnect quiesce = %v", transport.disconnects)
	}
	got := transport.publishes[0]
	if got.payload != "offline" || got.qos != 1 || !got.retain {
		t.Fatalf("offline publish = %+v", got)
	}
	if client.beginCallback() {
		t.Fatal("callback accepted after Close")
	}
}

func TestCloseReturnsPublishErrorAfterDisconnect(t *testing.T) {
	transport := newFakeTransport()
	publishErr := errors.New("offline rejected")
	transport.publishToken = completedToken(publishErr)
	client := clientWithTransport(t, transport)
	if err := client.Close(time.Second); !errors.Is(err, publishErr) {
		t.Fatalf("Close error = %v", err)
	}
	if len(transport.disconnects) != 1 {
		t.Fatal("Close did not disconnect after offline publish error")
	}
}
