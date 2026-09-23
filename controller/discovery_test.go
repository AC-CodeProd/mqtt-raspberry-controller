package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

func TestDiscoveryReconcileCleansDeletionRenameTypeAndDisable(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "discovery.json")
	mqtt := newBehaviorMQTT()
	cfg := fullControllerConfig(true)
	cfg.MQTT.Discovery.StateFile = statePath
	c := New(context.Background(), cfg, &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer c.Shutdown(time.Second)
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}

	mqtt.published = nil
	next := fullControllerConfig(true)
	next.MQTT.Discovery.StateFile = statePath
	next.Entities = []config.EntityConfig{
		{ID: "relay", Type: "button", Name: "Relay button"},
		{ID: "chime", Type: "button", Name: "Renamed bell"},
	}
	c.cfg = next
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	got := mqtt.snapshot()
	wantTopics := []string{
		"ha/binary_sensor/node/door/config",
		"ha/button/node/bell/config",
		"ha/switch/node/relay/config",
		"ha/button/node/relay/config",
		"ha/button/node/chime/config",
	}
	if len(got) != len(wantTopics) {
		t.Fatalf("publications = %#v", got)
	}
	for i, want := range wantTopics {
		if got[i].topic != want || !got[i].retain || got[i].qos != 1 {
			t.Fatalf("publication[%d] = %#v, want topic %q retained QoS 1", i, got[i], want)
		}
		payload := got[i].payload.([]byte)
		if (i < 3) != (len(payload) == 0) {
			t.Fatalf("publication[%d] payload = %q", i, payload)
		}
	}
	state, err := loadDiscoveryState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ha/button/node/chime/config", "ha/button/node/relay/config"}; state.OwnerID != "node-client" || state.DeviceID != "node" || !reflect.DeepEqual(state.Topics, want) {
		t.Fatalf("state = %#v, want topics %v", state, want)
	}

	mqtt.published = nil
	disabled := false
	c.cfg.MQTT.Discovery.Enabled = &disabled
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	got = mqtt.snapshot()
	if len(got) != 2 || len(got[0].payload.([]byte)) != 0 || len(got[1].payload.([]byte)) != 0 {
		t.Fatalf("disable cleanup = %#v", got)
	}
	state, err = loadDiscoveryState(statePath)
	if err != nil || len(state.Topics) != 0 {
		t.Fatalf("disabled state = %#v, %v", state, err)
	}
}

func TestDiscoveryDeviceIDMigrationCleansOldOwnershipFirst(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "discovery.json")
	mqtt := newBehaviorMQTT()
	cfg := fullControllerConfig(true)
	cfg.MQTT.Discovery.StateFile = statePath
	c := New(context.Background(), cfg, &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer c.Shutdown(time.Second)
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	mqtt.published = nil
	c.cfg.Device.ID = "node-new"
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	got := mqtt.snapshot()
	if len(got) != 6 {
		t.Fatalf("migration publications = %#v", got)
	}
	for i := 0; i < 3; i++ {
		if len(got[i].payload.([]byte)) != 0 || !ownedDiscoveryTopic(got[i].topic, "node") {
			t.Fatalf("old cleanup[%d] = %#v", i, got[i])
		}
	}
	for i := 3; i < 6; i++ {
		if len(got[i].payload.([]byte)) == 0 || !ownedDiscoveryTopic(got[i].topic, "node-new") {
			t.Fatalf("new publication[%d] = %#v", i, got[i])
		}
	}
}

func TestDiscoveryRejectsStateTopicForAnotherDeviceWithoutPublishing(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "discovery.json")
	encoded, err := json.Marshal(discoveryState{
		Version: discoveryStateVersion, OwnerID: "node-client", DeviceID: "node",
		Topics: []string{"ha/switch/other-device/relay/config"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := fullControllerConfig(true)
	cfg.MQTT.Discovery.StateFile = statePath
	mqtt := newBehaviorMQTT()
	c := New(context.Background(), cfg, &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer c.Shutdown(time.Second)
	if err := c.PublishDiscovery(); err == nil {
		t.Fatal("unsafe state was accepted")
	}
	if got := mqtt.snapshot(); len(got) != 0 {
		t.Fatalf("published from unsafe state: %#v", got)
	}
}

func TestDiscoveryFailureRetainsPendingTopicsForRetry(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "discovery.json")
	mqtt := newBehaviorMQTT()
	cfg := fullControllerConfig(true)
	cfg.MQTT.Discovery.StateFile = statePath
	c := New(context.Background(), cfg, &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer c.Shutdown(time.Second)
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	c.cfg.Entities = c.cfg.Entities[:1]
	failure := errors.New("broker denied delete")
	mqtt.publishErr["ha/button/node/bell/config"] = failure
	if err := c.PublishDiscovery(); !errors.Is(err, failure) {
		t.Fatalf("cleanup error = %v", err)
	}
	state, err := loadDiscoveryState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Topics) != 3 {
		t.Fatalf("pending state lost retry topics: %#v", state)
	}

	delete(mqtt.publishErr, "ha/button/node/bell/config")
	mqtt.published = nil
	if err := c.PublishDiscovery(); err != nil {
		t.Fatalf("retry reconciliation: %v", err)
	}
	state, err = loadDiscoveryState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ha/switch/node/relay/config"}; !reflect.DeepEqual(state.Topics, want) {
		t.Fatalf("converged state = %#v, want topics %v", state, want)
	}
}

func TestDiscoveryRejectsManifestOwnedByAnotherMQTTClient(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "discovery.json")
	state := discoveryState{
		Version:  discoveryStateVersion,
		OwnerID:  "other-client",
		DeviceID: "other-device",
		Topics:   []string{"ha/switch/other-device/relay/config"},
	}
	if err := writeDiscoveryState(statePath, state); err != nil {
		t.Fatal(err)
	}
	cfg := fullControllerConfig(true)
	cfg.MQTT.Discovery.StateFile = statePath
	mqtt := newBehaviorMQTT()
	c := New(context.Background(), cfg, &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer c.Shutdown(time.Second)
	if err := c.PublishDiscovery(); err == nil {
		t.Fatal("manifest owned by another MQTT client was accepted")
	}
	if got := mqtt.snapshot(); len(got) != 0 {
		t.Fatalf("foreign manifest triggered publications: %#v", got)
	}
}

func TestDiscoveryAlwaysUsesAcknowledgedQoS(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "discovery.json")
	qos := byte(0)
	cfg := fullControllerConfig(true)
	cfg.MQTT.QoS = &qos
	cfg.MQTT.Discovery.StateFile = statePath
	mqtt := newBehaviorMQTT()
	c := New(context.Background(), cfg, &behaviorGPIO{}, mqtt, &behaviorExecutor{})
	defer c.Shutdown(time.Second)
	if err := c.PublishDiscovery(); err != nil {
		t.Fatal(err)
	}
	for i, publication := range mqtt.snapshot() {
		if publication.qos != 1 || !publication.retain {
			t.Fatalf("publication[%d] qos=%d retain=%t", i, publication.qos, publication.retain)
		}
	}
}

func TestDiscoveryStateRejectsSymlinksAndUnsafeDirectories(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDiscoveryState(link); err == nil {
		t.Fatal("symlinked discovery state was accepted")
	}

	fifo := filepath.Join(root, "state.fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDiscoveryState(fifo); err == nil {
		t.Fatal("FIFO discovery state was accepted")
	}

	wrongMode := filepath.Join(root, "wrong-mode.json")
	if err := os.WriteFile(wrongMode, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wrongMode, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDiscoveryState(wrongMode); err == nil {
		t.Fatal("non-private discovery state file was accepted")
	}

	unsafeDir := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDir, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := lockDiscoveryState(filepath.Join(unsafeDir, "state.json")); err == nil {
		t.Fatal("group-writable discovery state directory was accepted")
	}

	lockedPath := filepath.Join(root, "locked.json")
	unlock, err := lockDiscoveryState(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDiscoveryState(lockedPath); err == nil {
		unlock()
		t.Fatal("concurrent discovery state lock was accepted")
	}
	unlock()
	unlockAgain, err := lockDiscoveryState(lockedPath)
	if err != nil {
		t.Fatalf("released discovery state lock could not be reacquired: %v", err)
	}
	unlockAgain()
}

func TestDiscoveryStateSizeBoundaryPreservesLastValidManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovery.json")
	state := discoveryState{Version: discoveryStateVersion, DeviceID: "node"}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	padding := maxDiscoveryStateSize - len(encoded) - 1
	if padding < 1 {
		t.Fatalf("unexpected discovery state encoding size %d", len(encoded))
	}
	state.OwnerID = strings.Repeat("a", padding)
	if err := writeDiscoveryState(path, state); err != nil {
		t.Fatalf("write exact-size manifest: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxDiscoveryStateSize {
		t.Fatalf("manifest size = %d, want %d", info.Size(), maxDiscoveryStateSize)
	}
	if _, err := loadDiscoveryState(path); err != nil {
		t.Fatalf("load exact-size manifest: %v", err)
	}

	state.OwnerID += "a"
	if err := writeDiscoveryState(path, state); err == nil {
		t.Fatal("oversized discovery state was accepted")
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxDiscoveryStateSize {
		t.Fatalf("last valid manifest was replaced; size = %d", info.Size())
	}
}
