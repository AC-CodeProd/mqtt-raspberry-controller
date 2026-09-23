package config

import (
	"fmt"
	"sort"
	"strings"
)

type lookupEnvFunc func(string) (string, bool)

func (c *Config) expandEnvironment(lookup lookupEnvFunc) error {
	fields := []struct {
		path  string
		value *string
	}{
		{"mqtt.broker", &c.MQTT.Broker},
		{"mqtt.username", &c.MQTT.Username},
		{"mqtt.password", &c.MQTT.Password},
		{"mqtt.password_file", &c.MQTT.PasswordFile},
		{"mqtt.tls.ca_file", &c.MQTT.TLS.CAFile},
		{"mqtt.tls.cert_file", &c.MQTT.TLS.CertFile},
		{"mqtt.tls.key_file", &c.MQTT.TLS.KeyFile},
		{"mqtt.tls.server_name", &c.MQTT.TLS.ServerName},
		{"mqtt.tls.min_version", &c.MQTT.TLS.MinVersion},
		{"mqtt.client_id", &c.MQTT.ClientID},
		{"mqtt.base_topic", &c.MQTT.BaseTopic},
		{"mqtt.keep_alive", &c.MQTT.KeepAlive},
		{"mqtt.connect_timeout", &c.MQTT.ConnectTimeout},
		{"mqtt.discovery.prefix", &c.MQTT.Discovery.Prefix},
		{"mqtt.discovery.state_file", &c.MQTT.Discovery.StateFile},
		{"device.id", &c.Device.ID},
		{"device.name", &c.Device.Name},
		{"device.manufacturer", &c.Device.Manufacturer},
		{"device.model", &c.Device.Model},
	}
	for _, field := range fields {
		if err := expandField(field.path, field.value, lookup); err != nil {
			return err
		}
	}

	for _, name := range sortedKeys(c.GPIO.Outputs) {
		output := c.GPIO.Outputs[name]
		if err := expandField(fmt.Sprintf("gpio.outputs[%q].chip", name), &output.Chip, lookup); err != nil {
			return err
		}
		c.GPIO.Outputs[name] = output
	}
	for _, name := range sortedKeys(c.GPIO.Inputs) {
		input := c.GPIO.Inputs[name]
		prefix := fmt.Sprintf("gpio.inputs[%q]", name)
		for _, field := range []struct {
			name  string
			value *string
		}{
			{"chip", &input.Chip},
			{"bias", &input.Bias},
			{"debounce", &input.Debounce},
		} {
			if err := expandField(prefix+"."+field.name, field.value, lookup); err != nil {
				return err
			}
		}
		c.GPIO.Inputs[name] = input
	}

	for i := range c.Entities {
		entity := &c.Entities[i]
		prefix := fmt.Sprintf("entities[%d]", i)
		for _, field := range []struct {
			name  string
			value *string
		}{
			{"id", &entity.ID},
			{"type", &entity.Type},
			{"name", &entity.Name},
			{"icon", &entity.Icon},
			{"device_class", &entity.DeviceClass},
			{"state.type", &entity.State.Type},
			{"state.gpio", &entity.State.GPIO},
			{"source.type", &entity.Source.Type},
			{"source.gpio", &entity.Source.GPIO},
		} {
			if err := expandField(prefix+"."+field.name, field.value, lookup); err != nil {
				return err
			}
		}
		for _, actions := range []struct {
			name   string
			values []Action
		}{
			{"on", entity.On},
			{"off", entity.Off},
			{"press", entity.Press},
		} {
			for j := range actions.values {
				if err := expandAction(&actions.values[j], fmt.Sprintf("%s.%s[%d]", prefix, actions.name, j), lookup); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func expandAction(action *Action, path string, lookup lookupEnvFunc) error {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"type", &action.Type},
		{"gpio", &action.GPIO},
		{"command", &action.Command},
	} {
		if err := expandField(path+"."+field.name, field.value, lookup); err != nil {
			return err
		}
	}
	for i := range action.Args {
		if err := expandField(fmt.Sprintf("%s.args[%d]", path, i), &action.Args[i], lookup); err != nil {
			return err
		}
	}
	if err := expandField(path+".working_dir", &action.WorkingDir, lookup); err != nil {
		return err
	}
	for _, name := range sortedKeys(action.Env) {
		value := action.Env[name]
		if err := expandField(fmt.Sprintf("%s.env[%q]", path, name), &value, lookup); err != nil {
			return err
		}
		action.Env[name] = value
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"timeout", &action.Timeout},
		{"duration", &action.Duration},
		{"topic", &action.Topic},
		{"payload", &action.Payload},
	} {
		if err := expandField(path+"."+field.name, field.value, lookup); err != nil {
			return err
		}
	}
	return nil
}

func expandField(path string, value *string, lookup lookupEnvFunc) error {
	expanded, missing := expandValue(*value, lookup)
	if missing != "" {
		return fmt.Errorf("expand config field %s: missing environment variable %q", path, missing)
	}
	*value = expanded
	return nil
}

// expandValue performs one pass over value. Only ${NAME} is substituted;
// $$ escapes a literal dollar and substituted values are never scanned again.
func expandValue(value string, lookup lookupEnvFunc) (string, string) {
	var expanded strings.Builder
	expanded.Grow(len(value))

	for i := 0; i < len(value); {
		if value[i] != '$' {
			expanded.WriteByte(value[i])
			i++
			continue
		}
		if i+1 < len(value) && value[i+1] == '$' {
			expanded.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(value) || value[i+1] != '{' {
			expanded.WriteByte('$')
			i++
			continue
		}

		endOffset := strings.IndexByte(value[i+2:], '}')
		if endOffset < 0 {
			expanded.WriteString(value[i:])
			break
		}
		end := i + 2 + endOffset
		name := value[i+2 : end]
		if !validEnvironmentName(name) {
			expanded.WriteString(value[i : end+1])
			i = end + 1
			continue
		}
		replacement, ok := lookup(name)
		if !ok {
			return "", name
		}
		expanded.WriteString(replacement)
		i = end + 1
	}

	return expanded.String(), ""
}

func validEnvironmentName(name string) bool {
	if name == "" || !isASCIILetter(name[0]) && name[0] != '_' {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isASCIILetter(name[i]) && (name[i] < '0' || name[i] > '9') && name[i] != '_' {
			return false
		}
	}
	return true
}

func isASCIILetter(char byte) bool {
	return char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
