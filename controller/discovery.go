package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	discoveryStateVersion = 1
	maxDiscoveryStateSize = 64 * 1024
)

type discoveryState struct {
	Version  int      `json:"version"`
	OwnerID  string   `json:"owner_id"`
	DeviceID string   `json:"device_id"`
	Topics   []string `json:"topics"`
}

type discoveryPublication struct {
	entityID string
	topic    string
	payload  []byte
}

func (c *Controller) reconcileDiscovery() error {
	publications, err := c.desiredDiscoveryPublications()
	if err != nil {
		return err
	}
	desired := make([]string, len(publications))
	for i := range publications {
		desired[i] = publications[i].topic
	}

	statePath := c.cfg.MQTT.Discovery.StateFile
	if statePath == "" {
		return c.publishDiscoveryChanges(nil, publications)
	}
	unlock, err := lockDiscoveryState(statePath)
	if err != nil {
		return err
	}
	defer unlock()

	previous, err := loadDiscoveryState(statePath)
	if err != nil {
		return err
	}
	if err := validateDiscoveryState(previous); err != nil {
		return fmt.Errorf("validate discovery state %q: %w", statePath, err)
	}
	if previous.OwnerID != "" && previous.OwnerID != c.cfg.MQTT.ClientID {
		return fmt.Errorf("discovery state %q belongs to MQTT client %q, not %q", statePath, previous.OwnerID, c.cfg.MQTT.ClientID)
	}
	if previous.DeviceID != "" && previous.DeviceID != c.cfg.Device.ID {
		// A device ID change crosses an ownership boundary. Remove only topics
		// bound to this stable MQTT client identity in the private manifest.
		if err := c.publishDiscoveryChanges(previous.Topics, nil); err != nil {
			return err
		}
		previous = discoveryState{}
		if err := writeDiscoveryState(statePath, discoveryState{Version: discoveryStateVersion, OwnerID: c.cfg.MQTT.ClientID, DeviceID: c.cfg.Device.ID}); err != nil {
			return err
		}
	}

	// Persist an over-approximation before publishing. If the process stops
	// mid-reconciliation, every topic it may have published remains tracked.
	pending := discoveryState{Version: discoveryStateVersion, OwnerID: c.cfg.MQTT.ClientID, DeviceID: c.cfg.Device.ID, Topics: unionTopics(previous.Topics, desired)}
	if err := writeDiscoveryState(statePath, pending); err != nil {
		return err
	}

	stale := differenceTopics(previous.Topics, desired)
	if err := c.publishDiscoveryChanges(stale, publications); err != nil {
		return err
	}
	return writeDiscoveryState(statePath, discoveryState{Version: discoveryStateVersion, OwnerID: c.cfg.MQTT.ClientID, DeviceID: c.cfg.Device.ID, Topics: desired})
}

func (c *Controller) desiredDiscoveryPublications() ([]discoveryPublication, error) {
	if !c.cfg.DiscoveryEnabled() {
		return nil, nil
	}
	result := make([]discoveryPublication, 0, len(c.cfg.Entities))
	for _, entity := range c.cfg.Entities {
		encoded, err := json.Marshal(c.discoveryPayload(entity))
		if err != nil {
			return nil, fmt.Errorf("marshal discovery for %q: %w", entity.ID, err)
		}
		result = append(result, discoveryPublication{
			entityID: entity.ID,
			topic:    fmt.Sprintf("%s/%s/%s/%s/config", c.cfg.MQTT.Discovery.Prefix, entity.Type, c.cfg.Device.ID, entity.ID),
			payload:  encoded,
		})
	}
	return result, nil
}

func (c *Controller) publishDiscoveryChanges(stale []string, desired []discoveryPublication) error {
	for _, topic := range stale {
		if err := c.mqtt.Publish(topic, []byte{}, c.discoveryQoS(), true); err != nil {
			return fmt.Errorf("remove stale discovery topic %q: %w", topic, err)
		}
	}
	for _, publication := range desired {
		if err := c.mqtt.Publish(publication.topic, publication.payload, c.discoveryQoS(), true); err != nil {
			return fmt.Errorf("publish discovery for %q to %q: %w", publication.entityID, publication.topic, err)
		}
	}
	return nil
}

func (c *Controller) discoveryQoS() byte {
	if qos := c.cfg.MQTTQoS(); qos > 0 {
		return qos
	}
	return 1
}

func lockDiscoveryState(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create discovery state directory %q: %w", dir, err)
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open discovery state lock %q: %w", lockPath, err)
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	if err := validatePrivateFile(lock, lockPath, 0); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock discovery state %q: another controller may be using it: %w", path, err)
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = lock.Close()
	}, nil
}

func validatePrivateDirectory(path string) error {
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("resolve discovery state directory %q: %w", clean, err)
	}
	if resolved != clean {
		return fmt.Errorf("discovery state directory %q must not contain symlinks", clean)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(clean, &stat); err != nil {
		return fmt.Errorf("stat discovery state directory %q: %w", clean, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0022 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("discovery state directory %q must be owned by uid %d and not writable by group or others", clean, os.Geteuid())
	}
	return nil
}

func validatePrivateFile(file *os.File, path string, maxSize int64) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("stat discovery state %q: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Uid != uint32(os.Geteuid()) || maxSize > 0 && stat.Size > maxSize {
		return fmt.Errorf("discovery state %q must be a regular 0600 file owned by uid %d and no larger than %d bytes", path, os.Geteuid(), maxSize)
	}
	return nil
}

func loadDiscoveryState(path string) (discoveryState, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return discoveryState{}, nil
	}
	if err != nil {
		return discoveryState{}, fmt.Errorf("open discovery state %q: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if err := validatePrivateFile(f, path, maxDiscoveryStateSize); err != nil {
		return discoveryState{}, err
	}
	var state discoveryState
	decoder := json.NewDecoder(io.LimitReader(f, maxDiscoveryStateSize+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return discoveryState{}, fmt.Errorf("decode discovery state %q: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return discoveryState{}, fmt.Errorf("decode discovery state %q: trailing data", path)
	}
	return state, nil
}

func validateDiscoveryState(state discoveryState) error {
	if state.Version == 0 && state.OwnerID == "" && state.DeviceID == "" && len(state.Topics) == 0 {
		return nil
	}
	if state.Version != discoveryStateVersion {
		return fmt.Errorf("unsupported version %d", state.Version)
	}
	if state.OwnerID == "" {
		return fmt.Errorf("missing owner_id")
	}
	if !validDiscoveryID(state.DeviceID) {
		return fmt.Errorf("invalid device_id")
	}
	seen := make(map[string]struct{}, len(state.Topics))
	for _, topic := range state.Topics {
		if !ownedDiscoveryTopic(topic, state.DeviceID) {
			return fmt.Errorf("topic %q does not belong to device_id %q", topic, state.DeviceID)
		}
		if _, ok := seen[topic]; ok {
			return fmt.Errorf("duplicate topic %q", topic)
		}
		seen[topic] = struct{}{}
	}
	return nil
}

func ownedDiscoveryTopic(topic, deviceID string) bool {
	parts := strings.Split(topic, "/")
	if len(parts) < 5 || parts[len(parts)-1] != "config" || parts[len(parts)-3] != deviceID || !validDiscoveryID(parts[len(parts)-2]) {
		return false
	}
	switch parts[len(parts)-4] {
	case "switch", "button", "binary_sensor":
	default:
		return false
	}
	prefix := strings.Join(parts[:len(parts)-4], "/")
	return prefix != "" && !strings.ContainsAny(prefix, "+#")
}

func validDiscoveryID(value string) bool {
	if value == "" || (value[0] < 'a' || value[0] > 'z') && (value[0] < '0' || value[0] > '9') {
		return false
	}
	for i := 1; i < len(value); i++ {
		char := value[i]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func unionTopics(first, second []string) []string {
	set := make(map[string]struct{}, len(first)+len(second))
	for _, topic := range append(append([]string(nil), first...), second...) {
		set[topic] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for topic := range set {
		result = append(result, topic)
	}
	sort.Strings(result)
	return result
}

func differenceTopics(previous, desired []string) []string {
	keep := make(map[string]struct{}, len(desired))
	for _, topic := range desired {
		keep[topic] = struct{}{}
	}
	var stale []string
	for _, topic := range previous {
		if _, ok := keep[topic]; !ok {
			stale = append(stale, topic)
		}
	}
	sort.Strings(stale)
	return stale
}

func writeDiscoveryState(path string, state discoveryState) error {
	state.Topics = append([]string(nil), state.Topics...)
	sort.Strings(state.Topics)
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode discovery state %q: %w", path, err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxDiscoveryStateSize {
		return fmt.Errorf("discovery state %q exceeds maximum size of %d bytes", path, maxDiscoveryStateSize)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create discovery state directory %q: %w", dir, err)
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".discovery-state-*")
	if err != nil {
		return fmt.Errorf("create temporary discovery state in %q: %w", dir, err)
	}
	tempPath := temp.Name()
	remove := true
	defer func() {
		_ = temp.Close()
		if remove {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0600); err != nil {
		return fmt.Errorf("secure temporary discovery state: %w", err)
	}
	if _, err := io.Copy(temp, bytes.NewReader(encoded)); err != nil {
		return fmt.Errorf("write temporary discovery state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary discovery state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary discovery state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace discovery state %q: %w", path, err)
	}
	remove = false
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open discovery state directory %q: %w", dir, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync discovery state directory %q: %w", dir, err)
	}
	return nil
}
