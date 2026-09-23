package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (c Config) Validate() error {
	secure, err := c.validateMQTT()
	if err != nil {
		return err
	}
	if secure {
		if _, err := c.MQTTTLSConfig(); err != nil {
			return err
		}
	}

	if !idPattern.MatchString(c.Device.ID) {
		return fmt.Errorf("device.id %q is invalid: use lowercase letters, digits, '_' or '-'", c.Device.ID)
	}

	if strings.TrimSpace(c.Device.Name) == "" {
		return fmt.Errorf("device.name is required")
	}

	if c.MQTT.ClientID == "" {
		return fmt.Errorf("mqtt.client_id cannot be empty")
	}

	if err := validateTopicPrefix("mqtt.base_topic", c.MQTT.BaseTopic); err != nil {
		return err
	}

	if c.DiscoveryEnabled() {
		if err := validateTopicPrefix("mqtt.discovery.prefix", c.MQTT.Discovery.Prefix); err != nil {
			return err
		}
	}
	if c.MQTT.Discovery.StateFile != "" {
		if strings.IndexByte(c.MQTT.Discovery.StateFile, 0) >= 0 || !filepath.IsAbs(c.MQTT.Discovery.StateFile) || filepath.Clean(c.MQTT.Discovery.StateFile) != c.MQTT.Discovery.StateFile {
			return fmt.Errorf("mqtt.discovery.state_file must be a clean absolute path")
		}
	}

	if c.MQTTQoS() > 2 {
		return fmt.Errorf("mqtt.qos must be 0, 1 or 2")
	}

	if c.MQTT.KeepAlive != "" {
		if err := validatePositiveDuration("mqtt.keep_alive", c.MQTT.KeepAlive); err != nil {
			return err
		}
	}

	if c.MQTT.ConnectTimeout != "" {
		if err := validatePositiveDuration("mqtt.connect_timeout", c.MQTT.ConnectTimeout); err != nil {
			return err
		}
	}

	if err := c.validateGPIO(); err != nil {
		return err
	}

	if len(c.Entities) == 0 {
		return fmt.Errorf("at least one entity is required")
	}

	seenEntities := make(map[string]struct{}, len(c.Entities))
	for i := range c.Entities {
		entity := c.Entities[i]

		if !idPattern.MatchString(entity.ID) {
			return fmt.Errorf("entities[%d].id %q is invalid", i, entity.ID)
		}

		if _, exists := seenEntities[entity.ID]; exists {
			return fmt.Errorf("duplicate entity id %q", entity.ID)
		}
		seenEntities[entity.ID] = struct{}{}

		if strings.TrimSpace(entity.Name) == "" {
			return fmt.Errorf("entity %q: name is required", entity.ID)
		}

		switch entity.Type {
		case "switch":
			if err := c.validateSwitch(entity); err != nil {
				return err
			}
		case "button":
			if err := c.validateButton(entity); err != nil {
				return err
			}
		case "binary_sensor":
			if err := c.validateBinarySensor(entity); err != nil {
				return err
			}
		default:
			return fmt.Errorf("entity %q: unsupported type %q", entity.ID, entity.Type)
		}
	}

	return nil
}

const maxCredentialFileSize = 64 * 1024

var secureMQTTSchemes = map[string]bool{
	"ssl": true, "tls": true, "mqtts": true, "mqtt+ssl": true, "tcps": true, "wss": true,
}

// BrokerURL returns the broker URL. Its error deliberately omits the input so
// malformed URI credentials cannot leak through logs.
func (c Config) BrokerURL() (*url.URL, error) {
	u, err := url.Parse(c.MQTT.Broker)
	if err != nil {
		return nil, fmt.Errorf("mqtt.broker is not a valid URL")
	}
	return u, nil
}

// BrokerLogAddress returns a path- and credential-free broker identifier for
// logs. Network transports are always rendered as scheme://host:port.
func (c Config) BrokerLogAddress() (string, error) {
	u, err := c.BrokerURL()
	if err != nil {
		return "", err
	}
	if strings.EqualFold(u.Scheme, "unix") {
		return "unix://local", nil
	}
	if u.Hostname() == "" || u.Port() == "" {
		return "", fmt.Errorf("mqtt.broker must include an explicit host and port")
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(u.Hostname(), u.Port()), nil
}

func (c Config) validateMQTT() (bool, error) {
	if strings.TrimSpace(c.MQTT.Broker) == "" {
		return false, fmt.Errorf("mqtt.broker is required")
	}
	u, err := c.BrokerURL()
	if err != nil {
		return false, err
	}
	if u.User != nil {
		return false, fmt.Errorf("mqtt.broker must not contain user information")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false, fmt.Errorf("mqtt.broker must not contain a query or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	secure := secureMQTTSchemes[scheme]
	switch scheme {
	case "unix":
		if u.Host != "" || !filepath.IsAbs(u.Path) || filepath.Clean(u.Path) != u.Path || u.RawPath != "" {
			return false, fmt.Errorf("mqtt.broker unix URL must contain one clean absolute socket path")
		}
	case "mqtt", "tcp", "ws", "ssl", "tls", "mqtts", "mqtt+ssl", "tcps", "wss":
		if u.Hostname() == "" || u.Port() == "" {
			return false, fmt.Errorf("mqtt.broker must include an explicit host and port")
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false, fmt.Errorf("mqtt.broker port must be an integer from 1 to 65535")
		}
		if scheme == "ws" || scheme == "wss" {
			if u.Path != "" && (!strings.HasPrefix(u.Path, "/") || filepath.Clean(u.Path) != u.Path || u.RawPath != "") {
				return false, fmt.Errorf("mqtt.broker websocket path is unsafe")
			}
		} else if u.Path != "" {
			return false, fmt.Errorf("mqtt.broker must not contain a path for this scheme")
		}
		if !secure && !c.MQTT.AllowInsecureTransport && !isLiteralLoopback(u.Hostname()) {
			return false, fmt.Errorf("mqtt.broker plaintext transport is permitted only for literal loopback/localhost; set mqtt.allow_insecure_transport to acknowledge remote plaintext")
		}
	default:
		return false, fmt.Errorf("mqtt.broker scheme is unsupported")
	}

	hasPassword := c.MQTT.Password != "" || c.MQTT.PasswordFile != ""
	if c.MQTT.Password != "" && c.MQTT.PasswordFile != "" {
		return false, fmt.Errorf("mqtt.password and mqtt.password_file are mutually exclusive")
	}
	if (c.MQTT.Username == "") != !hasPassword {
		return false, fmt.Errorf("mqtt.username and password/password_file must be configured together")
	}
	hasCert := c.MQTT.TLS.CertFile != "" || c.MQTT.TLS.KeyFile != ""
	if (c.MQTT.TLS.CertFile == "") != (c.MQTT.TLS.KeyFile == "") {
		return false, fmt.Errorf("mqtt.tls.cert_file and mqtt.tls.key_file must be configured together")
	}
	if hasCert && !secure {
		return false, fmt.Errorf("mqtt.tls client certificate requires a secure broker scheme")
	}
	hasTLSOption := c.MQTT.TLS.CAFile != "" || hasCert || c.MQTT.TLS.ServerName != "" || c.MQTT.TLS.MinVersion != ""
	if hasTLSOption && !secure {
		return false, fmt.Errorf("mqtt.tls settings require a secure broker scheme")
	}
	if c.MQTT.AllowUnauthenticated && (hasPassword || hasCert) {
		return false, fmt.Errorf("mqtt.allow_unauthenticated cannot be combined with configured authentication")
	}
	if !hasPassword && !hasCert && !c.MQTT.AllowUnauthenticated {
		return false, fmt.Errorf("MQTT authentication is required; configure username/password or mTLS, or explicitly set mqtt.allow_unauthenticated")
	}
	if c.MQTT.PasswordFile != "" {
		if _, err := c.MQTTPassword(); err != nil {
			return false, err
		}
	}
	return secure, nil
}

func isLiteralLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// MQTTPassword reads a password file and removes at most one final newline.
func (c Config) MQTTPassword() (string, error) {
	if c.MQTT.PasswordFile == "" {
		return c.MQTT.Password, nil
	}
	b, err := readBoundedRegularFile(c.MQTT.PasswordFile, maxCredentialFileSize, true)
	if err != nil {
		return "", fmt.Errorf("mqtt.password_file: %w", err)
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
		if len(b) > 0 && b[len(b)-1] == '\r' {
			b = b[:len(b)-1]
		}
	}
	if len(b) == 0 {
		return "", fmt.Errorf("mqtt.password_file is empty")
	}
	if strings.IndexByte(string(b), 0) >= 0 {
		return "", fmt.Errorf("mqtt.password_file contains a NUL byte")
	}
	return string(b), nil
}

func readBoundedRegularFile(path string, limit int64, restrictive bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if restrictive && (info.Mode().Perm() != 0400 && info.Mode().Perm() != 0600 && info.Mode().Perm() != 0640 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0) {
		return nil, fmt.Errorf("permissions must be exactly 0400, 0600 or 0640")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return b, nil
}

// MQTTTLSConfig constructs and validates TLS without connecting. This is used
// by --check to prove all configured certificate files are readable and parse.
func (c Config) MQTTTLSConfig() (*tls.Config, error) {
	minVersion := uint16(tls.VersionTLS12)
	switch c.MQTT.TLS.MinVersion {
	case "", "1.2":
	case "1.3":
		minVersion = tls.VersionTLS13
	default:
		return nil, fmt.Errorf("mqtt.tls.min_version must be 1.2 or 1.3")
	}
	u, err := c.BrokerURL()
	if err != nil {
		return nil, err
	}
	serverName := c.MQTT.TLS.ServerName
	if serverName == "" {
		serverName = u.Hostname()
	} else if !validTLSServerName(serverName) {
		return nil, fmt.Errorf("mqtt.tls.server_name is invalid")
	}
	tlsConfig := &tls.Config{MinVersion: minVersion, ServerName: serverName}
	if c.MQTT.TLS.CAFile != "" {
		pem, err := readBoundedRegularFile(c.MQTT.TLS.CAFile, 4*1024*1024, false)
		if err != nil {
			return nil, fmt.Errorf("mqtt.tls.ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mqtt.tls.ca_file contains no valid certificates")
		}
		tlsConfig.RootCAs = pool
	}
	if c.MQTT.TLS.CertFile != "" {
		certPEM, err := readBoundedRegularFile(c.MQTT.TLS.CertFile, 4*1024*1024, false)
		if err != nil {
			return nil, fmt.Errorf("mqtt.tls.cert_file: %w", err)
		}
		keyPEM, err := readBoundedRegularFile(c.MQTT.TLS.KeyFile, 4*1024*1024, true)
		if err != nil {
			return nil, fmt.Errorf("mqtt.tls.key_file: %w", err)
		}
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("mqtt.tls client certificate/key is invalid")
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}

func validTLSServerName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/@?#[]") {
		return false
	}
	if net.ParseIP(name) != nil {
		return true
	}
	if len(name) > 253 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func (c Config) validateGPIO() error {
	usedLines := make(map[string]string)

	for name, output := range c.GPIO.Outputs {
		if !idPattern.MatchString(name) {
			return fmt.Errorf("gpio.outputs key %q is invalid", name)
		}
		if output.Line < 0 {
			return fmt.Errorf("gpio.outputs.%s.line must be >= 0", name)
		}

		key := fmt.Sprintf("%s:%d", output.Chip, output.Line)
		if previous, exists := usedLines[key]; exists {
			return fmt.Errorf("GPIO %s is declared more than once (%s and output %s)", key, previous, name)
		}
		usedLines[key] = "output " + name
	}

	for name, input := range c.GPIO.Inputs {
		if !idPattern.MatchString(name) {
			return fmt.Errorf("gpio.inputs key %q is invalid", name)
		}
		if input.Line < 0 {
			return fmt.Errorf("gpio.inputs.%s.line must be >= 0", name)
		}

		switch input.Bias {
		case "disabled", "pull_up", "pull_down":
		default:
			return fmt.Errorf("gpio.inputs.%s.bias must be disabled, pull_up or pull_down", name)
		}

		if input.Debounce != "" {
			if err := validatePositiveDuration("gpio.inputs."+name+".debounce", input.Debounce); err != nil {
				return err
			}
		}

		key := fmt.Sprintf("%s:%d", input.Chip, input.Line)
		if previous, exists := usedLines[key]; exists {
			return fmt.Errorf("GPIO %s is declared more than once (%s and input %s)", key, previous, name)
		}
		usedLines[key] = "input " + name
	}

	return nil
}

func (c Config) validateSwitch(entity EntityConfig) error {
	if entity.State.Type == "" {
		return fmt.Errorf("switch %q: state.type is required", entity.ID)
	}

	switch entity.State.Type {
	case "memory":
		if entity.State.GPIO != "" {
			return fmt.Errorf("switch %q: state.gpio is not valid for memory state", entity.ID)
		}
	case "gpio":
		if _, exists := c.GPIO.Outputs[entity.State.GPIO]; !exists {
			return fmt.Errorf("switch %q: unknown GPIO output %q", entity.ID, entity.State.GPIO)
		}
	default:
		return fmt.Errorf("switch %q: unsupported state.type %q", entity.ID, entity.State.Type)
	}

	if len(entity.On) == 0 {
		return fmt.Errorf("switch %q: at least one on action is required", entity.ID)
	}
	if len(entity.Off) == 0 {
		return fmt.Errorf("switch %q: at least one off action is required", entity.ID)
	}

	if err := c.validateActions(entity.ID+".on", entity.On); err != nil {
		return err
	}
	return c.validateActions(entity.ID+".off", entity.Off)
}

func (c Config) validateButton(entity EntityConfig) error {
	if len(entity.Press) == 0 {
		return fmt.Errorf("button %q: at least one press action is required", entity.ID)
	}
	return c.validateActions(entity.ID+".press", entity.Press)
}

func (c Config) validateBinarySensor(entity EntityConfig) error {
	if entity.Source.Type != "gpio" {
		return fmt.Errorf("binary_sensor %q: source.type must be gpio", entity.ID)
	}
	if _, exists := c.GPIO.Inputs[entity.Source.GPIO]; !exists {
		return fmt.Errorf("binary_sensor %q: unknown GPIO input %q", entity.ID, entity.Source.GPIO)
	}
	return nil
}

func (c Config) validateActions(context string, actions []Action) error {
	for i, action := range actions {
		prefix := fmt.Sprintf("entity %s action[%d]", context, i)

		switch action.Type {
		case "gpio":
			if _, exists := c.GPIO.Outputs[action.GPIO]; !exists {
				return fmt.Errorf("%s: unknown GPIO output %q", prefix, action.GPIO)
			}
			if action.Value == nil {
				return fmt.Errorf("%s: gpio action requires value", prefix)
			}

		case "command":
			if err := validateCommandAction(prefix, action); err != nil {
				return err
			}
			if action.Timeout != "" {
				if err := validatePositiveDuration(prefix+".timeout", action.Timeout); err != nil {
					return err
				}
			}

		case "delay":
			if action.Duration == "" {
				return fmt.Errorf("%s: duration is required", prefix)
			}
			if err := validatePositiveDuration(prefix+".duration", action.Duration); err != nil {
				return err
			}

		case "mqtt":
			if err := validatePublishTopic(prefix+".topic", action.Topic); err != nil {
				return err
			}
			if action.QoS != nil && *action.QoS > 2 {
				return fmt.Errorf("%s: qos must be 0, 1 or 2", prefix)
			}

		default:
			return fmt.Errorf("%s: unsupported action type %q", prefix, action.Type)
		}
	}

	return nil
}

func validateCommandAction(prefix string, action Action) error {
	if strings.TrimSpace(action.Command) == "" {
		return fmt.Errorf("%s: command is required", prefix)
	}
	if strings.IndexByte(action.Command, 0) >= 0 {
		return fmt.Errorf("%s: command contains a NUL byte", prefix)
	}
	if !filepath.IsAbs(action.Command) {
		return fmt.Errorf("%s: command must be an absolute executable path", prefix)
	}
	info, err := os.Stat(action.Command)
	if err != nil {
		return fmt.Errorf("%s: cannot stat command: %v", prefix, pathErrorCause(err))
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("%s: command must name an existing regular executable file", prefix)
	}
	for i, argument := range action.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("%s: argument contains a NUL byte at args[%d]", prefix, i)
		}
	}
	if action.WorkingDir != "" {
		if strings.IndexByte(action.WorkingDir, 0) >= 0 {
			return fmt.Errorf("%s: working_dir contains a NUL byte", prefix)
		}
		if !filepath.IsAbs(action.WorkingDir) {
			return fmt.Errorf("%s: working_dir must be absolute", prefix)
		}
		info, err := os.Stat(action.WorkingDir)
		if err != nil {
			return fmt.Errorf("%s: cannot stat working_dir: %v", prefix, pathErrorCause(err))
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: working_dir must be an existing directory", prefix)
		}
	}
	for _, name := range sortedKeys(action.Env) {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("%s: environment name %q is invalid", prefix, name)
		}
		if strings.IndexByte(action.Env[name], 0) >= 0 {
			return fmt.Errorf("%s: environment value contains a NUL byte for %q", prefix, name)
		}
	}
	return nil
}

func pathErrorCause(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

func validatePositiveDuration(name, raw string) error {
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid duration: %w", name, raw, err)
	}
	if duration <= 0 {
		return fmt.Errorf("%s must be greater than zero", name)
	}
	return nil
}

func validateTopicPrefix(name, topic string) error {
	if topic == "" {
		return fmt.Errorf("%s cannot be empty", name)
	}
	if strings.ContainsAny(topic, "+#") {
		return fmt.Errorf("%s cannot contain MQTT wildcards '+' or '#'", name)
	}
	return nil
}

func validatePublishTopic(name, topic string) error {
	if strings.TrimSpace(topic) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return validateTopicPrefix(name, topic)
}
