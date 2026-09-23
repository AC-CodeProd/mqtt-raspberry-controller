package mqtt

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

const MaxCommandPayload = 4096

type Message struct {
	Topic     string
	Payload   []byte
	Retained  bool
	Duplicate bool
	QoS       byte
	MessageID uint16
}

type MessageHandler func(Message)
type SessionHandler func()
type ConnectHandler func()

type rateLimitedLog struct {
	mu         sync.Mutex
	suppressed uint64
	last       time.Time
}

func (l *rateLimitedLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < time.Second {
		l.suppressed++
		return
	}
	if l.suppressed > 0 {
		format += " (suppressed_since_last_log=%d)"
		args = append(args, l.suppressed)
	}
	log.Printf(format, args...)
	l.suppressed = 0
	l.last = now
}

type pahoTransport interface {
	Connect() paho.Token
	IsConnected() bool
	Publish(string, byte, bool, any) paho.Token
	Subscribe(string, byte, paho.MessageHandler) paho.Token
	Disconnect(uint)
}

type Client struct {
	cfg         *config.Config
	brokerLog   string
	transport   pahoTransport
	mu          sync.RWMutex
	subscribers map[string]MessageHandler
	onSession   []SessionHandler
	onConnect   []ConnectHandler
	accepting   bool
	callbacks   sync.WaitGroup
	rejectedLog rateLimitedLog
}

func New(cfg *config.Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	password, err := cfg.MQTTPassword()
	if err != nil {
		return nil, err
	}
	brokerLog, err := cfg.BrokerLogAddress()
	if err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, brokerLog: brokerLog, subscribers: make(map[string]MessageHandler), accepting: true}
	options := paho.NewClientOptions().AddBroker(cfg.MQTT.Broker).SetClientID(cfg.MQTT.ClientID).
		SetUsername(cfg.MQTT.Username).SetPassword(password).SetCleanSession(true).
		SetAutoReconnect(true).SetConnectRetry(false).SetConnectTimeout(cfg.MQTTConnectTimeout()).
		SetKeepAlive(cfg.MQTTKeepAlive()).SetPingTimeout(10 * time.Second).SetOrderMatters(true)
	brokerURL, err := cfg.BrokerURL()
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(brokerURL.Scheme) {
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps", "wss":
		tlsConfig, err := cfg.MQTTTLSConfig()
		if err != nil {
			return nil, err
		}
		options.SetTLSConfig(tlsConfig)
	}
	options.SetWill(c.StatusTopic(), "offline", cfg.MQTTQoS(), true)
	options.SetConnectionLostHandler(func(_ paho.Client, err error) { log.Printf("MQTT connection lost: %v", err) })
	options.SetReconnectingHandler(func(_ paho.Client, _ *paho.ClientOptions) { log.Printf("MQTT reconnecting to %s", c.brokerLog) })
	options.SetOnConnectHandler(func(client paho.Client) { c.handleConnect(client) })
	c.transport = paho.NewClient(options)
	return c, nil
}

func (c *Client) Connect() error {
	log.Printf("connecting to MQTT broker %s", c.brokerLog)
	token := c.transport.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("connect to MQTT broker: %w", err)
	}
	return nil
}

func (c *Client) RegisterSubscription(topic string, handler MessageHandler) error {
	c.mu.Lock()
	if _, exists := c.subscribers[topic]; exists {
		c.mu.Unlock()
		return fmt.Errorf("MQTT subscription %q is already registered", topic)
	}
	if !c.accepting {
		c.mu.Unlock()
		return fmt.Errorf("MQTT client is shutting down")
	}
	c.subscribers[topic] = handler
	connected := c.transport != nil && c.transport.IsConnected()
	c.mu.Unlock()
	if connected {
		return c.subscribe(topic, handler)
	}
	return nil
}

func (c *Client) AddConnectHandler(handler ConnectHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accepting {
		c.onConnect = append(c.onConnect, handler)
	}
}

// AddSessionHandler registers work that must run after MQTT connects but before
// subscriptions for the new clean session become live.
func (c *Client) AddSessionHandler(handler SessionHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accepting {
		c.onSession = append(c.onSession, handler)
	}
}

func (c *Client) StopAccepting() { c.mu.Lock(); c.accepting = false; c.mu.Unlock() }

func (c *Client) beginCallback() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.accepting {
		return false
	}
	c.callbacks.Add(1)
	return true
}

func (c *Client) Publish(topic string, payload any, qos byte, retain bool) error {
	return c.publishWithTimeout(topic, payload, qos, retain, 5*time.Second)
}

func (c *Client) publishWithTimeout(topic string, payload any, qos byte, retain bool, timeout time.Duration) error {
	if c.transport == nil || !c.transport.IsConnected() {
		return fmt.Errorf("MQTT client is not connected")
	}
	if timeout <= 0 {
		return fmt.Errorf("publish to %q timed out", topic)
	}
	token := c.transport.Publish(topic, qos, retain, payload)
	if !token.WaitTimeout(timeout) {
		return fmt.Errorf("publish to %q timed out", topic)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("publish to %q: %w", topic, err)
	}
	return nil
}

func (c *Client) Close(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	c.StopAccepting()
	callbacksDone := make(chan struct{})
	go func() {
		c.callbacks.Wait()
		close(callbacksDone)
	}()
	select {
	case <-callbacksDone:
	case <-time.After(time.Until(deadline)):
		return fmt.Errorf("wait for MQTT callbacks timed out after %s", timeout)
	}
	if c.transport == nil || !c.transport.IsConnected() {
		return nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("MQTT shutdown timed out after %s", timeout)
	}
	publishTimeout := min(5*time.Second, remaining)
	publishErr := c.publishWithTimeout(c.StatusTopic(), "offline", c.cfg.MQTTQoS(), true, publishTimeout)
	remaining = time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("MQTT shutdown timed out after %s: %w", timeout, publishErr)
	}
	quiesce := min(500*time.Millisecond, remaining)
	c.transport.Disconnect(uint(quiesce.Milliseconds()))
	if publishErr != nil {
		return fmt.Errorf("publish MQTT offline status: %w", publishErr)
	}
	return nil
}

func (c *Client) StatusTopic() string { return c.cfg.MQTT.BaseTopic + "/status" }

func (c *Client) handleConnect(client pahoTransport) {
	if !c.beginCallback() {
		return
	}
	defer c.callbacks.Done()
	c.mu.RLock()
	subscribers := make(map[string]MessageHandler, len(c.subscribers))
	for topic, handler := range c.subscribers {
		subscribers[topic] = handler
	}
	sessionHandlers := append([]SessionHandler(nil), c.onSession...)
	connectHandlers := append([]ConnectHandler(nil), c.onConnect...)
	c.mu.RUnlock()
	log.Printf("connected to MQTT broker %s", c.brokerLog)
	for _, handler := range sessionHandlers {
		handler()
	}
	for topic, handler := range subscribers {
		if err := c.subscribeWithClient(client, topic, handler); err != nil {
			log.Printf("subscribe to %q: %v", topic, err)
		}
	}
	if err := c.Publish(c.StatusTopic(), "online", c.cfg.MQTTQoS(), true); err != nil {
		log.Printf("publish MQTT online status: %v", err)
	}
	for _, handler := range connectHandlers {
		handler()
	}
}

func (c *Client) subscribe(topic string, handler MessageHandler) error {
	return c.subscribeWithClient(c.transport, topic, handler)
}

func (c *Client) subscribeWithClient(client pahoTransport, topic string, handler MessageHandler) error {
	token := client.Subscribe(topic, c.cfg.MQTTQoS(), func(_ paho.Client, message paho.Message) {
		if !c.beginCallback() {
			return
		}
		defer c.callbacks.Done()
		c.dispatchMessage(handler, message)
	})
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("subscription to %q timed out", topic)
	}
	if err := token.Error(); err != nil {
		return err
	}
	log.Printf("subscribed to MQTT topic %s", topic)
	return nil
}

func (c *Client) dispatchMessage(handler MessageHandler, message paho.Message) bool {
	payload := message.Payload()
	if len(payload) > MaxCommandPayload {
		c.rejectedLog.printf("reject MQTT message topic=%q: payload is %d bytes (max %d)", message.Topic(), len(payload), MaxCommandPayload)
		return false
	}
	// The callback only copies and enqueues. Controller workers provide bounded FIFO dispatch.
	handler(messageFromPaho(message))
	return true
}

func messageFromPaho(message paho.Message) Message {
	return Message{
		Topic: message.Topic(), Payload: append([]byte(nil), message.Payload()...),
		Retained: message.Retained(), Duplicate: message.Duplicate(),
		QoS: message.Qos(), MessageID: message.MessageID(),
	}
}
