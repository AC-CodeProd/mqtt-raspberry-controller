package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultMQTTQoS            byte = 1
	DefaultMQTTKeepAlive           = 30 * time.Second
	DefaultMQTTConnectTimeout      = 10 * time.Second
	DefaultCommandTimeout          = 10 * time.Second
	DefaultDiscoveryPrefix         = "homeassistant"
	DefaultDiscoveryStateFile      = "/var/lib/mqtt-raspberry-controller/discovery-state.json"
)

type Config struct {
	MQTT     MQTTConfig     `yaml:"mqtt"`
	Device   DeviceConfig   `yaml:"device"`
	GPIO     GPIOConfig     `yaml:"gpio"`
	Entities []EntityConfig `yaml:"entities"`
}

type MQTTConfig struct {
	Broker                 string          `yaml:"broker"`
	Username               string          `yaml:"username"`
	Password               string          `yaml:"password"`
	PasswordFile           string          `yaml:"password_file"`
	AllowUnauthenticated   bool            `yaml:"allow_unauthenticated"`
	AllowInsecureTransport bool            `yaml:"allow_insecure_transport"`
	TLS                    MQTTTLSConfig   `yaml:"tls"`
	ClientID               string          `yaml:"client_id"`
	BaseTopic              string          `yaml:"base_topic"`
	QoS                    *byte           `yaml:"qos"`
	KeepAlive              string          `yaml:"keep_alive"`
	ConnectTimeout         string          `yaml:"connect_timeout"`
	Discovery              DiscoveryConfig `yaml:"discovery"`
}

// MQTTTLSConfig controls certificate verification for secure MQTT schemes.
// An empty CAFile uses the operating system certificate pool.
type MQTTTLSConfig struct {
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
	MinVersion string `yaml:"min_version"`
}

type DiscoveryConfig struct {
	Enabled   *bool  `yaml:"enabled"`
	Prefix    string `yaml:"prefix"`
	StateFile string `yaml:"state_file"`
}

type DeviceConfig struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	Manufacturer string `yaml:"manufacturer"`
	Model        string `yaml:"model"`
}

type GPIOConfig struct {
	Outputs map[string]GPIOOutputConfig `yaml:"outputs"`
	Inputs  map[string]GPIOInputConfig  `yaml:"inputs"`
}

type GPIOOutputConfig struct {
	Chip      string `yaml:"chip"`
	Line      int    `yaml:"line"`
	ActiveLow bool   `yaml:"active_low"`
	Initial   bool   `yaml:"initial"`
}

type GPIOInputConfig struct {
	Chip      string `yaml:"chip"`
	Line      int    `yaml:"line"`
	ActiveLow bool   `yaml:"active_low"`
	Bias      string `yaml:"bias"`
	Debounce  string `yaml:"debounce"`
}

type EntityConfig struct {
	ID           string       `yaml:"id"`
	Type         string       `yaml:"type"`
	Name         string       `yaml:"name"`
	Icon         string       `yaml:"icon"`
	DeviceClass  string       `yaml:"device_class"`
	InitialState *bool        `yaml:"initial_state"`
	State        StateConfig  `yaml:"state"`
	Source       SourceConfig `yaml:"source"`
	On           []Action     `yaml:"on"`
	Off          []Action     `yaml:"off"`
	Press        []Action     `yaml:"press"`
}

type StateConfig struct {
	Type string `yaml:"type"`
	GPIO string `yaml:"gpio"`
}

type SourceConfig struct {
	Type string `yaml:"type"`
	GPIO string `yaml:"gpio"`
}

type Action struct {
	Type        string            `yaml:"type"`
	GPIO        string            `yaml:"gpio"`
	Value       *bool             `yaml:"value"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	WorkingDir  string            `yaml:"working_dir"`
	Env         map[string]string `yaml:"env"`
	Timeout     string            `yaml:"timeout"`
	Duration    string            `yaml:"duration"`
	Topic       string            `yaml:"topic"`
	Payload     string            `yaml:"payload"`
	QoS         *byte             `yaml:"qos"`
	Retain      bool              `yaml:"retain"`
	IgnoreError bool              `yaml:"ignore_error"`
}

func Load(path string) (*Config, error) {
	return load(path, os.LookupEnv)
}

func load(path string, lookup lookupEnvFunc) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode YAML config: %w", err)
	}
	if err := cfg.expandEnvironment(lookup); err != nil {
		return nil, err
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = c.Device.ID
	}

	if c.MQTT.BaseTopic == "" {
		c.MQTT.BaseTopic = c.Device.ID
	}
	c.MQTT.BaseTopic = strings.Trim(c.MQTT.BaseTopic, "/")

	if c.MQTT.Discovery.Prefix == "" {
		c.MQTT.Discovery.Prefix = DefaultDiscoveryPrefix
	}
	c.MQTT.Discovery.Prefix = strings.Trim(c.MQTT.Discovery.Prefix, "/")
	if c.MQTT.Discovery.StateFile == "" {
		c.MQTT.Discovery.StateFile = DefaultDiscoveryStateFile
	}

	if c.Device.Manufacturer == "" {
		c.Device.Manufacturer = "mqtt-raspberry-controller"
	}

	if c.Device.Model == "" {
		c.Device.Model = "mqtt-raspberry-controller"
	}

	for name, output := range c.GPIO.Outputs {
		if output.Chip == "" {
			output.Chip = "gpiochip0"
		}
		c.GPIO.Outputs[name] = output
	}

	for name, input := range c.GPIO.Inputs {
		if input.Chip == "" {
			input.Chip = "gpiochip0"
		}
		if input.Bias == "" {
			input.Bias = "disabled"
		}
		c.GPIO.Inputs[name] = input
	}
}

func (c Config) MQTTQoS() byte {
	if c.MQTT.QoS == nil {
		return DefaultMQTTQoS
	}
	return *c.MQTT.QoS
}

func (c Config) DiscoveryEnabled() bool {
	if c.MQTT.Discovery.Enabled == nil {
		return true
	}
	return *c.MQTT.Discovery.Enabled
}

func (c Config) MQTTKeepAlive() time.Duration {
	if c.MQTT.KeepAlive == "" {
		return DefaultMQTTKeepAlive
	}

	d, err := time.ParseDuration(c.MQTT.KeepAlive)
	if err != nil {
		return DefaultMQTTKeepAlive
	}
	return d
}

func (c Config) MQTTConnectTimeout() time.Duration {
	if c.MQTT.ConnectTimeout == "" {
		return DefaultMQTTConnectTimeout
	}

	d, err := time.ParseDuration(c.MQTT.ConnectTimeout)
	if err != nil {
		return DefaultMQTTConnectTimeout
	}
	return d
}

func (a Action) CommandTimeout() time.Duration {
	if a.Timeout == "" {
		return DefaultCommandTimeout
	}

	d, err := time.ParseDuration(a.Timeout)
	if err != nil {
		return DefaultCommandTimeout
	}
	return d
}
