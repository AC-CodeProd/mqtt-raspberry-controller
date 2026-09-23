package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSecurityConfig() Config {
	return Config{
		MQTT:     MQTTConfig{Broker: "tcp://127.0.0.1:1883", ClientID: "test", BaseTopic: "test", AllowUnauthenticated: true, Discovery: DiscoveryConfig{Prefix: DefaultDiscoveryPrefix}},
		Device:   DeviceConfig{ID: "test", Name: "Test"},
		Entities: []EntityConfig{{ID: "button", Type: "button", Name: "Button", Press: []Action{{Type: "delay", Duration: "1ms"}}}},
	}
}

func TestBrokerTransportPolicy(t *testing.T) {
	tests := []struct {
		name, broker string
		allow        bool
		wantErr      string
	}{
		{"loopback tcp", "tcp://127.0.0.1:1883", false, ""},
		{"localhost ws", "ws://localhost:8080/mqtt", false, ""},
		{"unix", "unix:///run/mosquitto/mosquitto.sock", false, ""},
		{"remote tls", "tls://broker.example:8883", false, ""},
		{"remote plaintext refused", "tcp://192.0.2.1:1883", false, "plaintext"},
		{"remote plaintext acknowledged", "tcp://192.0.2.1:1883", true, ""},
		{"hostname is not resolved", "tcp://loopback.example:1883", false, "plaintext"},
		{"missing scheme", "broker.example:8883", false, "scheme"},
		{"missing port", "tls://broker.example", false, "host and port"},
		{"nonnumeric port", "tls://broker.example:mqtt", false, "valid URL"},
		{"zero port", "tls://broker.example:0", false, "port must be an integer"},
		{"port too large", "tls://broker.example:65536", false, "port must be an integer"},
		{"unsupported scheme", "http://broker.example:443", false, "unsupported"},
		{"userinfo", "tls://user:supersecret@broker.example:8883", false, "user information"},
		{"query", "tls://broker.example:8883?password=supersecret", false, "query or fragment"},
		{"tcp path", "tls://broker.example:8883/mqtt", false, "path"},
		{"websocket traversal", "wss://broker.example:443/a/../mqtt", false, "unsafe"},
		{"unix traversal", "unix:///run/../tmp/mqtt.sock", false, "clean absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validSecurityConfig()
			cfg.MQTT.Broker = tt.broker
			cfg.MQTT.AllowInsecureTransport = tt.allow
			err := cfg.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "supersecret") {
				t.Fatalf("error leaked URI credential: %v", err)
			}
		})
	}
}

func TestBrokerLogAddressOmitsWebsocketPath(t *testing.T) {
	cfg := validSecurityConfig()
	cfg.MQTT.Broker = "wss://[::1]:443/private/secret-path"
	got, err := cfg.BrokerLogAddress()
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://[::1]:443" {
		t.Fatalf("BrokerLogAddress() = %q", got)
	}
	cfg.MQTT.Broker = "unix:///run/private/mqtt.sock"
	got, err = cfg.BrokerLogAddress()
	if err != nil || got != "unix://local" {
		t.Fatalf("unix BrokerLogAddress() = %q, %v", got, err)
	}
}

func TestAuthenticationPolicy(t *testing.T) {
	tests := []struct {
		name string
		edit func(*MQTTConfig)
		ok   bool
	}{
		{"anonymous requires opt in", func(m *MQTTConfig) { m.AllowUnauthenticated = false }, false},
		{"username password", func(m *MQTTConfig) { m.AllowUnauthenticated = false; m.Username = "device"; m.Password = "secret" }, true},
		{"username only", func(m *MQTTConfig) { m.Username = "device" }, false},
		{"password only", func(m *MQTTConfig) { m.Password = "secret" }, false},
		{"two password sources", func(m *MQTTConfig) { m.Username = "device"; m.Password = "secret"; m.PasswordFile = "other" }, false},
		{"anonymous and password conflict", func(m *MQTTConfig) { m.Username = "device"; m.Password = "secret" }, false},
		{"client certificate pair required", func(m *MQTTConfig) { m.TLS.CertFile = "client.pem" }, false},
		{"TLS settings require TLS transport", func(m *MQTTConfig) { m.TLS.MinVersion = "1.3" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validSecurityConfig()
			tt.edit(&cfg.MQTT)
			err := cfg.Validate()
			if (err == nil) != tt.ok {
				t.Fatalf("Validate() error = %v, ok=%v", err, tt.ok)
			}
		})
	}
}

func TestPasswordFileSecurityAndNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := validSecurityConfig()
	cfg.MQTT.AllowUnauthenticated = false
	cfg.MQTT.Username = "device"
	cfg.MQTT.PasswordFile = path
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := cfg.MQTTPassword()
	if err != nil || got != "s3cret" {
		t.Fatalf("MQTTPassword() = %q, %v", got, err)
	}

	if err := os.WriteFile(path, []byte("line1\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.MQTTPassword(); err != nil || got != "line1\n" {
		t.Fatalf("only one newline should be trimmed: %q, %v", got, err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.MQTTPassword(); err != nil || got != "line1\n" {
		t.Fatalf("0400 systemd credential = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.MQTTPassword(); err != nil || got != "line1\n" {
		t.Fatalf("0640 password file = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.MQTTPassword(); err == nil || !strings.Contains(err.Error(), "0400, 0600 or 0640") {
		t.Fatalf("permissive file error = %v", err)
	}
}

func TestPasswordFileRejectsNULDirectoryAndOversize(t *testing.T) {
	dir := t.TempDir()
	cfg := validSecurityConfig()
	cfg.MQTT.PasswordFile = dir
	if _, err := cfg.MQTTPassword(); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("directory error = %v", err)
	}
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("bad\x00value"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.MQTT.PasswordFile = path
	if _, err := cfg.MQTTPassword(); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL error = %v", err)
	}
	if err := os.WriteFile(path, make([]byte, maxCredentialFileSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.MQTTPassword(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("size error = %v", err)
	}
}

func TestTLSMinVersionAndInvalidCA(t *testing.T) {
	cfg := validSecurityConfig()
	cfg.MQTT.Broker = "tls://broker.example:8883"
	cfg.MQTT.TLS.MinVersion = "1.1"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "1.2 or 1.3") {
		t.Fatalf("TLS floor error = %v", err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg.MQTT.TLS.MinVersion = "1.2"
	cfg.MQTT.TLS.CAFile = ca
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "no valid certificates") {
		t.Fatalf("CA parse error = %v", err)
	}
}

func TestKnownFieldsIncludesStrictTLS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `mqtt:
  broker: tcp://127.0.0.1:1883
  allow_unauthenticated: true
  tls:
    skip_verify: true
device:
  id: test
  name: Test
entities: []
`
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "skip_verify") {
		t.Fatalf("unknown TLS field error = %v", err)
	}
}
