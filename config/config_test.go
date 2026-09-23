package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesAllDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `mqtt:
  broker: tcp://127.0.0.1:1883
  allow_unauthenticated: true
device:
  id: test-device
  name: Test Device
gpio:
  outputs:
    relay: {line: 4}
  inputs:
    contact: {line: 5}
entities:
  - id: relay
    type: switch
    name: Relay
    state: {type: gpio, gpio: relay}
    on: [{type: gpio, gpio: relay, value: true}]
    off: [{type: gpio, gpio: relay, value: false}]
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.ClientID != "test-device" || cfg.MQTT.BaseTopic != "test-device" {
		t.Fatalf("MQTT defaults = client_id %q base_topic %q", cfg.MQTT.ClientID, cfg.MQTT.BaseTopic)
	}
	if cfg.MQTT.Discovery.Prefix != DefaultDiscoveryPrefix || !cfg.DiscoveryEnabled() || cfg.MQTTQoS() != DefaultMQTTQoS {
		t.Fatalf("discovery/QoS defaults not applied: %+v", cfg.MQTT)
	}
	if cfg.MQTT.Discovery.StateFile != DefaultDiscoveryStateFile {
		t.Fatalf("discovery state file default = %q", cfg.MQTT.Discovery.StateFile)
	}
	if cfg.MQTTKeepAlive() != DefaultMQTTKeepAlive || cfg.MQTTConnectTimeout() != DefaultMQTTConnectTimeout {
		t.Fatalf("duration defaults = %s, %s", cfg.MQTTKeepAlive(), cfg.MQTTConnectTimeout())
	}
	if cfg.Device.Manufacturer != "mqtt-raspberry-controller" || cfg.Device.Model != "mqtt-raspberry-controller" {
		t.Fatalf("device defaults = %+v", cfg.Device)
	}
	if cfg.GPIO.Outputs["relay"].Chip != "gpiochip0" || cfg.GPIO.Inputs["contact"].Chip != "gpiochip0" || cfg.GPIO.Inputs["contact"].Bias != "disabled" {
		t.Fatalf("GPIO defaults = %+v", cfg.GPIO)
	}
	if got := (Action{}).CommandTimeout(); got != DefaultCommandTimeout {
		t.Fatalf("command timeout default = %s", got)
	}
}

func TestValidationCriticalInvalidConfigurations(t *testing.T) {
	qos := byte(3)
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"invalid device id", func(c *Config) { c.Device.ID = "Bad ID" }, "device.id"},
		{"missing device name", func(c *Config) { c.Device.Name = " " }, "device.name"},
		{"invalid qos", func(c *Config) { c.MQTT.QoS = &qos }, "mqtt.qos"},
		{"invalid keep alive", func(c *Config) { c.MQTT.KeepAlive = "0s" }, "keep_alive"},
		{"duplicate entity", func(c *Config) { c.Entities = append(c.Entities, c.Entities[0]) }, "duplicate entity"},
		{"wildcard base", func(c *Config) { c.MQTT.BaseTopic = "bad/#" }, "wildcards"},
		{"relative discovery state", func(c *Config) { c.MQTT.Discovery.StateFile = "state.json" }, "clean absolute"},
		{"duplicate GPIO", func(c *Config) {
			c.GPIO.Inputs = map[string]GPIOInputConfig{"same": {Chip: "gpiochip0", Line: 1, Bias: "disabled"}}
		}, "declared more than once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validSecurityConfig()
			cfg.GPIO.Outputs = map[string]GPIOOutputConfig{"relay": {Chip: "gpiochip0", Line: 1}}
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExampleConfigurationLoadsAndValidates(t *testing.T) {
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("test-only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MQTT_PASSWORD_FILE", password)
	cfg, err := Load(filepath.Join("..", "configs", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Entities) != 3 || len(cfg.GPIO.Outputs) != 1 || len(cfg.GPIO.Inputs) != 1 {
		t.Fatalf("example shape = entities %d outputs %d inputs %d", len(cfg.Entities), len(cfg.GPIO.Outputs), len(cfg.GPIO.Inputs))
	}
	if cfg.MQTTKeepAlive() != 30*time.Second || cfg.MQTTConnectTimeout() != 10*time.Second {
		t.Fatalf("example MQTT durations invalid")
	}
}
