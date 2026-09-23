package mqtt

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

func TestEphemeralBrokerQoS1AndRetainedAvailability(t *testing.T) {
	broker := os.Getenv("MQTT_TEST_BROKER")
	if broker == "" {
		t.Skip("set MQTT_TEST_BROKER to opt in to a real broker test")
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(idBytes)
	base := "mqtt-raspberry-controller/test/" + id

	observed := make(chan Message, 16)
	observer := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID("observer-" + id).SetCleanSession(true))
	mustToken(t, observer.Connect(), "connect observer")
	t.Cleanup(func() { observer.Disconnect(250) })
	mustToken(t, observer.Subscribe(base+"/#", 1, func(_ paho.Client, message paho.Message) { observed <- messageFromPaho(message) }), "subscribe observer")

	cfg := &config.Config{
		MQTT:     config.MQTTConfig{Broker: broker, ClientID: "controller-" + id, BaseTopic: base, AllowUnauthenticated: true, Discovery: config.DiscoveryConfig{Prefix: config.DefaultDiscoveryPrefix}},
		Device:   config.DeviceConfig{ID: "test-" + id, Name: "Integration test"},
		Entities: []config.EntityConfig{{ID: "button", Type: "button", Name: "Button", Press: []config.Action{{Type: "delay", Duration: "1ms"}}}},
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan Message, 1)
	commandTopic := base + "/button/press"
	if err := client.RegisterSubscription(commandTopic, func(message Message) { delivered <- message }); err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(time.Second) })

	online := waitMessage(t, observed, func(message Message) bool {
		return message.Topic == base+"/status" && string(message.Payload) == "online"
	})
	if online.Retained || online.QoS != 1 {
		t.Fatalf("live online metadata = %+v", online)
	}
	onlineRetained := probeRetainedStatus(t, broker, base, "online-probe-"+id)
	if string(onlineRetained.Payload) != "online" || !onlineRetained.Retained || onlineRetained.QoS != 1 {
		t.Fatalf("retained online status = %+v", onlineRetained)
	}
	mustToken(t, observer.Publish(commandTopic, 1, false, "PRESS"), "publish QoS1 command")
	select {
	case message := <-delivered:
		if message.Topic != commandTopic || string(message.Payload) != "PRESS" || message.QoS != 1 {
			t.Fatalf("delivery = %+v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("QoS1 command was not delivered")
	}

	if err := client.Close(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	offline := waitMessage(t, observed, func(message Message) bool {
		return message.Topic == base+"/status" && string(message.Payload) == "offline"
	})
	if offline.Retained || offline.QoS != 1 {
		t.Fatalf("live offline metadata = %+v", offline)
	}

	retained := make(chan Message, 1)
	probe := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID("probe-" + id).SetCleanSession(true))
	mustToken(t, probe.Connect(), "connect retained probe")
	defer probe.Disconnect(250)
	mustToken(t, probe.Subscribe(base+"/status", 1, func(_ paho.Client, message paho.Message) { retained <- messageFromPaho(message) }), "subscribe retained probe")
	select {
	case message := <-retained:
		if string(message.Payload) != "offline" || !message.Retained {
			t.Fatalf("retained status = %+v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retained offline status was not delivered")
	}
}

func probeRetainedStatus(t *testing.T, broker, baseTopic, clientID string) Message {
	t.Helper()
	messages := make(chan Message, 1)
	client := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID(clientID).SetCleanSession(true))
	mustToken(t, client.Connect(), "connect retained probe")
	defer client.Disconnect(250)
	mustToken(t, client.Subscribe(baseTopic+"/status", 1, func(_ paho.Client, message paho.Message) {
		messages <- messageFromPaho(message)
	}), "subscribe retained probe")
	select {
	case message := <-messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("retained status was not delivered")
		return Message{}
	}
}

func mustToken(t *testing.T, token paho.Token, operation string) {
	t.Helper()
	if !token.WaitTimeout(5 * time.Second) {
		t.Fatalf("%s timed out", operation)
	}
	if err := token.Error(); err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

func waitMessage(t *testing.T, messages <-chan Message, match func(Message) bool) Message {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-messages:
			if match(message) {
				return message
			}
		case <-timer.C:
			t.Fatal("timed out waiting for MQTT message")
		}
	}
}
