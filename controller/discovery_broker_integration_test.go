package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
	mqttclient "github.com/AC-CodeProd/mqtt-raspberry-controller/mqtt"
)

func TestDiscoveryCleanupWithBroker(t *testing.T) {
	broker := os.Getenv("MQTT_TEST_BROKER")
	if broker == "" {
		t.Skip("set MQTT_TEST_BROKER to opt in to a real broker test")
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(random)
	rootPrefix := "homeassistant/mqtt-raspberry-controller-test-" + id
	prefix := rootPrefix + "/old"
	newPrefix := rootPrefix + "/new"
	deviceID := "node-" + id
	newDeviceID := "renamed-node-" + id
	otherTopic := prefix + "/switch/other-" + id + "/keep/config"
	oldSwitch := prefix + "/switch/" + deviceID + "/relay/config"
	oldButton := prefix + "/button/" + deviceID + "/bell/config"
	renamedButton := prefix + "/button/" + deviceID + "/chime/config"
	typeChangedButton := prefix + "/button/" + deviceID + "/relay/config"
	newPrefixButton := newPrefix + "/button/" + deviceID + "/relay/config"
	newDeviceButton := newPrefix + "/button/" + newDeviceID + "/relay/config"
	statePath := filepath.Join(t.TempDir(), "discovery-state.json")

	live := make(chan mqttclient.Message, 64)
	observer := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID("discovery-observer-" + id).SetCleanSession(true))
	mustBrokerToken(t, observer.Connect(), "connect discovery observer")
	t.Cleanup(func() { observer.Disconnect(250) })
	mustBrokerToken(t, observer.Subscribe(rootPrefix+"/#", 1, func(_ paho.Client, message paho.Message) {
		live <- brokerMessage(message)
	}), "subscribe discovery observer")
	mustBrokerToken(t, observer.Publish(otherTopic, 1, true, `{"name":"unrelated"}`), "seed unrelated discovery")
	waitBrokerMessage(t, live, otherTopic, false)

	clientID := "discovery-owner-" + id
	initial := brokerDiscoveryConfig(broker, clientID, prefix, deviceID, statePath, true)
	initial.Entities = []config.EntityConfig{
		{ID: "relay", Type: "switch", Name: "Relay", State: config.StateConfig{Type: "memory"}, On: []config.Action{{Type: "delay", Duration: "1ms"}}, Off: []config.Action{{Type: "delay", Duration: "1ms"}}},
		{ID: "bell", Type: "button", Name: "Bell", Press: []config.Action{{Type: "delay", Duration: "1ms"}}},
	}
	publishDiscoveryWithBroker(t, initial)
	waitBrokerMessage(t, live, oldSwitch, false)
	waitBrokerMessage(t, live, oldButton, false)

	renamed := brokerDiscoveryConfig(broker, clientID, prefix, deviceID, statePath, true)
	renamed.Entities = []config.EntityConfig{
		initial.Entities[0],
		{ID: "chime", Type: "button", Name: "Renamed bell", Press: []config.Action{{Type: "delay", Duration: "1ms"}}},
	}
	publishDiscoveryWithBroker(t, renamed)
	if message := waitBrokerMessage(t, live, oldButton, true); len(message.Payload) != 0 {
		t.Fatalf("renamed entity cleanup payload = %q", message.Payload)
	}
	waitBrokerMessage(t, live, renamedButton, false)
	assertRetainedDiscovery(t, broker, rootPrefix, "after-rename-"+id, map[string]bool{otherTopic: true, oldSwitch: true, renamedButton: true})

	typeChanged := brokerDiscoveryConfig(broker, clientID, prefix, deviceID, statePath, true)
	typeChanged.Entities = []config.EntityConfig{{ID: "relay", Type: "button", Name: "Relay button", Press: []config.Action{{Type: "delay", Duration: "1ms"}}}}
	publishDiscoveryWithBroker(t, typeChanged)
	if message := waitBrokerMessage(t, live, renamedButton, true); len(message.Payload) != 0 {
		t.Fatalf("removed entity cleanup payload = %q", message.Payload)
	}
	if message := waitBrokerMessage(t, live, oldSwitch, true); len(message.Payload) != 0 {
		t.Fatalf("type-change cleanup payload = %q", message.Payload)
	}
	waitBrokerMessage(t, live, typeChangedButton, false)

	prefixChanged := brokerDiscoveryConfig(broker, clientID, newPrefix, deviceID, statePath, true)
	prefixChanged.Entities = typeChanged.Entities
	publishDiscoveryWithBroker(t, prefixChanged)
	waitBrokerMessage(t, live, typeChangedButton, true)
	waitBrokerMessage(t, live, newPrefixButton, false)

	deviceChanged := brokerDiscoveryConfig(broker, clientID, newPrefix, newDeviceID, statePath, true)
	deviceChanged.Entities = typeChanged.Entities
	publishDiscoveryWithBroker(t, deviceChanged)
	waitBrokerMessage(t, live, newPrefixButton, true)
	waitBrokerMessage(t, live, newDeviceButton, false)
	assertRetainedDiscovery(t, broker, rootPrefix, "after-migrations-"+id, map[string]bool{otherTopic: true, newDeviceButton: true})

	disabled := brokerDiscoveryConfig(broker, clientID, newPrefix, newDeviceID, statePath, false)
	disabled.Entities = typeChanged.Entities
	publishDiscoveryWithBroker(t, disabled)
	if message := waitBrokerMessage(t, live, newDeviceButton, true); len(message.Payload) != 0 {
		t.Fatalf("global-disable cleanup payload = %q", message.Payload)
	}
	assertRetainedDiscovery(t, broker, rootPrefix, "after-disable-"+id, map[string]bool{otherTopic: true})
	mustBrokerToken(t, observer.Publish(otherTopic, 1, true, []byte{}), "remove unrelated test discovery")
	mustBrokerToken(t, observer.Publish("mqtt-raspberry-controller/test/"+deviceID+"/status", 1, true, []byte{}), "remove old test availability")
	mustBrokerToken(t, observer.Publish("mqtt-raspberry-controller/test/"+newDeviceID+"/status", 1, true, []byte{}), "remove new test availability")
}

func brokerDiscoveryConfig(broker, clientID, prefix, deviceID, statePath string, enabled bool) *config.Config {
	qos := byte(1)
	return &config.Config{
		MQTT: config.MQTTConfig{
			Broker: broker, ClientID: clientID, BaseTopic: "mqtt-raspberry-controller/test/" + deviceID,
			QoS: &qos, AllowUnauthenticated: true,
			Discovery: config.DiscoveryConfig{Enabled: &enabled, Prefix: prefix, StateFile: statePath},
		},
		Device: config.DeviceConfig{ID: deviceID, Name: "Discovery integration"},
	}
}

func publishDiscoveryWithBroker(t *testing.T, cfg *config.Config) {
	t.Helper()
	client, err := mqttclient.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	c := New(context.Background(), cfg, &behaviorGPIO{}, client, &behaviorExecutor{})
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(2 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func assertRetainedDiscovery(t *testing.T, broker, prefix, clientID string, expected map[string]bool) {
	t.Helper()
	messages := make(chan mqttclient.Message, len(expected)+4)
	probe := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID(clientID).SetCleanSession(true))
	mustBrokerToken(t, probe.Connect(), "connect discovery retained probe")
	defer probe.Disconnect(250)
	mustBrokerToken(t, probe.Subscribe(prefix+"/#", 1, func(_ paho.Client, message paho.Message) {
		messages <- brokerMessage(message)
	}), "subscribe discovery retained probe")
	got := make(map[string]bool)
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case message := <-messages:
			if !message.Retained || len(message.Payload) == 0 {
				t.Fatalf("retained probe message = %#v", message)
			}
			got[message.Topic] = true
		case <-timer.C:
			if len(got) != len(expected) {
				t.Fatalf("retained discovery topics = %v, want %v", got, expected)
			}
			for topic := range expected {
				if !got[topic] {
					t.Fatalf("retained discovery topics = %v, missing %q", got, topic)
				}
			}
			return
		}
	}
}

func waitBrokerMessage(t *testing.T, messages <-chan mqttclient.Message, topic string, empty bool) mqttclient.Message {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-messages:
			if message.Topic == topic && (len(message.Payload) == 0) == empty {
				return message
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for MQTT topic %q empty=%v", topic, empty)
		}
	}
}

func mustBrokerToken(t *testing.T, token paho.Token, operation string) {
	t.Helper()
	if !token.WaitTimeout(5 * time.Second) {
		t.Fatalf("%s timed out", operation)
	}
	if err := token.Error(); err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

func brokerMessage(message paho.Message) mqttclient.Message {
	return mqttclient.Message{Topic: message.Topic(), Payload: append([]byte(nil), message.Payload()...), Retained: message.Retained(), QoS: message.Qos()}
}
