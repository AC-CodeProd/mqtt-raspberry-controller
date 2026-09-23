package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mapLookup(values map[string]string) lookupEnvFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestExpandValueStrictOnePassGrammar(t *testing.T) {
	lookup := mapLookup(map[string]string{
		"NAME":   "value",
		"_VALID": "under",
		"A1_b":   "mixed",
		"FIRST":  "${SECOND}",
		"SECOND": "rescanned",
	})
	tests := []struct {
		name, input, want, missing string
	}{
		{"valid", "before ${NAME} after", "before value after", ""},
		{"underscore", "${_VALID}", "under", ""},
		{"letters digits underscore", "${A1_b}", "mixed", ""},
		{"literal dollar", "cost $$5", "cost $5", ""},
		{"escaped expression", "$${NAME}", "${NAME}", ""},
		{"escaped then expanded", "$$${NAME}", "$value", ""},
		{"bare dollar", "$", "$", ""},
		{"bare name", "$NAME", "$NAME", ""},
		{"empty", "${}", "${}", ""},
		{"starts with digit", "${1NAME}", "${1NAME}", ""},
		{"invalid punctuation", "${BAD-NAME}", "${BAD-NAME}", ""},
		{"nested malformed", "${BAD${NAME}}", "${BAD${NAME}}", ""},
		{"malformed then valid", "${BAD-NAME}${NAME}", "${BAD-NAME}value", ""},
		{"missing brace", "${NAME", "${NAME", ""},
		{"extra brace", "${NAME}}", "value}", ""},
		{"multiple dollars", "$$$", "$$", ""},
		{"single pass", "${FIRST}", "${SECOND}", ""},
		{"missing", "prefix ${ABSENT} secret-suffix", "", "ABSENT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, missing := expandValue(tt.input, lookup)
			if got != tt.want || missing != tt.missing {
				t.Fatalf("expandValue(%q) = (%q, %q), want (%q, %q)", tt.input, got, missing, tt.want, tt.missing)
			}
		})
	}
}

func TestExpandEnvironmentVisitsEveryModeledStringValue(t *testing.T) {
	const placeholder = "${VALUE}"
	action := Action{
		Type: placeholder, GPIO: placeholder, Command: placeholder,
		Args: []string{placeholder}, WorkingDir: placeholder,
		Env: map[string]string{"${ENV_KEY}": placeholder}, Timeout: placeholder,
		Duration: placeholder, Topic: placeholder, Payload: placeholder,
	}
	cfg := Config{
		MQTT: MQTTConfig{
			Broker: placeholder, Username: placeholder, Password: placeholder,
			PasswordFile: placeholder, ClientID: placeholder, BaseTopic: placeholder,
			KeepAlive: placeholder, ConnectTimeout: placeholder,
			TLS: MQTTTLSConfig{
				CAFile: placeholder, CertFile: placeholder, KeyFile: placeholder,
				ServerName: placeholder, MinVersion: placeholder,
			},
			Discovery: DiscoveryConfig{Prefix: placeholder, StateFile: placeholder},
		},
		Device: DeviceConfig{ID: placeholder, Name: placeholder, Manufacturer: placeholder, Model: placeholder},
		GPIO: GPIOConfig{
			Outputs: map[string]GPIOOutputConfig{"${OUTPUT_KEY}": {Chip: placeholder}},
			Inputs:  map[string]GPIOInputConfig{"${INPUT_KEY}": {Chip: placeholder, Bias: placeholder, Debounce: placeholder}},
		},
		Entities: []EntityConfig{{
			ID: placeholder, Type: placeholder, Name: placeholder, Icon: placeholder,
			DeviceClass: placeholder,
			State:       StateConfig{Type: placeholder, GPIO: placeholder},
			Source:      SourceConfig{Type: placeholder, GPIO: placeholder},
			On:          []Action{action}, Off: []Action{action}, Press: []Action{action},
		}},
	}

	if err := cfg.expandEnvironment(mapLookup(map[string]string{"VALUE": "expanded", "ENV_KEY": "changed", "OUTPUT_KEY": "changed", "INPUT_KEY": "changed"})); err != nil {
		t.Fatal(err)
	}
	assertAllStringValues(t, reflect.ValueOf(cfg), placeholder, "expanded")
	if _, ok := cfg.GPIO.Outputs["${OUTPUT_KEY}"]; !ok {
		t.Fatal("gpio output map key was expanded")
	}
	if _, ok := cfg.GPIO.Inputs["${INPUT_KEY}"]; !ok {
		t.Fatal("gpio input map key was expanded")
	}
	if _, ok := cfg.Entities[0].Press[0].Env["${ENV_KEY}"]; !ok {
		t.Fatal("action environment map key was expanded")
	}
}

func assertAllStringValues(t *testing.T, value reflect.Value, old, want string) {
	t.Helper()
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		assertAllStringValues(t, value.Elem(), old, want)
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			assertAllStringValues(t, value.Field(i), old, want)
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			assertAllStringValues(t, value.Index(i), old, want)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			assertAllStringValues(t, iter.Value(), old, want)
		}
	case reflect.String:
		if value.String() == old {
			t.Errorf("modeled string value was not expanded")
		} else if value.String() != "" && value.String() != want {
			t.Errorf("modeled string value = %q, want %q", value.String(), want)
		}
	}
}

func TestLoadExpandsAfterYAMLDecode(t *testing.T) {
	path := writeExpansionConfig(t, `mqtt:
  broker: tcp://127.0.0.1:1883
  username: device
  password: ${PASSWORD}
  discovery:
    enabled: false
device:
  id: test
  name: Test
entities:
  - id: button
    type: button
    name: Button
    press:
      - type: mqtt
        topic: events
        payload: $${LITERAL} $$ ${PAYLOAD}
`)
	secret := "colon: hash# quotes\"'\nnew: mapping"
	payload := "line 1: # quoted \"value\"\nentities:\n  - injected"
	cfg, err := load(path, mapLookup(map[string]string{"PASSWORD": secret, "PAYLOAD": payload}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != secret {
		t.Fatalf("password was changed: got %q", cfg.MQTT.Password)
	}
	if got := cfg.Entities[0].Press[0].Payload; got != "${LITERAL} $ "+payload {
		t.Fatalf("payload = %q", got)
	}
	if len(cfg.Entities) != 1 {
		t.Fatalf("substitution changed YAML structure: %d entities", len(cfg.Entities))
	}
}

func TestMissingEnvironmentErrorsAreLocalizedSafeAndDeterministic(t *testing.T) {
	secret := "do-not-leak: #\nsecret"
	t.Run("sorted GPIO names", func(t *testing.T) {
		cfg := Config{GPIO: GPIOConfig{Outputs: map[string]GPIOOutputConfig{
			"z-output": {Chip: "${Z_MISSING}"},
			"a-output": {Chip: "${A_MISSING}"},
		}}}
		err := cfg.expandEnvironment(mapLookup(nil))
		assertSafeExpansionError(t, err, `gpio.outputs["a-output"].chip`, "A_MISSING", secret)
	})
	t.Run("sorted action environment names", func(t *testing.T) {
		cfg := Config{Entities: []EntityConfig{{Press: []Action{{Env: map[string]string{
			"Z_ENV": "${Z_MISSING}",
			"A_ENV": "${A_MISSING}" + secret,
		}}}}}}
		err := cfg.expandEnvironment(mapLookup(nil))
		assertSafeExpansionError(t, err, `entities[0].press[0].env["A_ENV"]`, "A_MISSING", secret)
	})
}

func assertSafeExpansionError(t *testing.T, err error, path, variable, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected missing environment error")
	}
	message := err.Error()
	if !strings.Contains(message, path) || !strings.Contains(message, variable) {
		t.Fatalf("error %q does not contain path %q and variable %q", message, path, variable)
	}
	if strings.Contains(message, secret) || strings.Contains(message, "do-not-leak") {
		t.Fatalf("error leaked field contents: %q", message)
	}
}

func TestUnknownFieldsFailBeforeExpansion(t *testing.T) {
	path := writeExpansionConfig(t, `mqtt:
  broker: ${BROKER}
  allow_unauthenticated: true
  unknown: ${VALUE}
device:
  id: test
  name: Test
entities: []
`)
	calls := 0
	_, err := load(path, func(name string) (string, bool) {
		calls++
		return "replacement", true
	})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("environment lookup ran %d times before strict YAML decoding", calls)
	}
}

func writeExpansionConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
