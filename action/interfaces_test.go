package action

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type gpioCall struct {
	name  string
	state bool
}

type fakeGPIOOutput struct {
	calls []gpioCall
	err   error
}

func (f *fakeGPIOOutput) SetOutput(name string, state bool) error {
	f.calls = append(f.calls, gpioCall{name: name, state: state})
	return f.err
}

type publishCall struct {
	topic   string
	payload any
	qos     byte
	retain  bool
}

type fakePublisher struct {
	calls []publishCall
	err   error
}

func (f *fakePublisher) Publish(topic string, payload any, qos byte, retain bool) error {
	f.calls = append(f.calls, publishCall{topic: topic, payload: payload, qos: qos, retain: retain})
	return f.err
}

func boolPointer(value bool) *bool { return &value }
func bytePointer(value byte) *byte { return &value }

func TestGPIOAndMQTTActionsPassExactParameters(t *testing.T) {
	gpio := &fakeGPIOOutput{}
	mqtt := &fakePublisher{}
	configuredQoS := byte(2)
	executor := New(&config.Config{MQTT: config.MQTTConfig{QoS: &configuredQoS}}, gpio, mqtt)
	actions := []config.Action{
		{Type: "gpio", GPIO: "relay", Value: boolPointer(true)},
		{Type: "mqtt", Topic: "events/exact", Payload: "payload", Retain: true},
		{Type: "mqtt", Topic: "events/override", Payload: "other", QoS: bytePointer(0)},
	}
	if err := executor.Execute(context.Background(), actions); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gpio.calls, []gpioCall{{name: "relay", state: true}}) {
		t.Fatalf("GPIO calls = %#v", gpio.calls)
	}
	want := []publishCall{
		{topic: "events/exact", payload: "payload", qos: 2, retain: true},
		{topic: "events/override", payload: "other", qos: 0, retain: false},
	}
	if !reflect.DeepEqual(mqtt.calls, want) {
		t.Fatalf("publish calls = %#v, want %#v", mqtt.calls, want)
	}
}

func TestActionErrorsStopSequenceUnlessIgnored(t *testing.T) {
	failure := errors.New("GPIO unavailable")
	gpio := &fakeGPIOOutput{err: failure}
	mqtt := &fakePublisher{}
	executor := New(&config.Config{}, gpio, mqtt)
	actions := []config.Action{
		{Type: "gpio", GPIO: "relay", Value: boolPointer(false)},
		{Type: "mqtt", Topic: "must/not/run", Payload: "x"},
	}
	err := executor.Execute(context.Background(), actions)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "action[0] type=gpio") {
		t.Fatalf("Execute error = %v", err)
	}
	if len(mqtt.calls) != 0 {
		t.Fatalf("publish ran after failure: %#v", mqtt.calls)
	}

	actions[0].IgnoreError = true
	if err := executor.Execute(context.Background(), actions); err != nil {
		t.Fatal(err)
	}
	if len(mqtt.calls) != 1 || mqtt.calls[0].topic != "must/not/run" {
		t.Fatalf("ignored error did not preserve order: %#v", mqtt.calls)
	}
}

func TestMQTTActionErrorAndUnsupportedActionAreWrapped(t *testing.T) {
	publishErr := errors.New("publish rejected")
	executor := New(&config.Config{}, &fakeGPIOOutput{}, &fakePublisher{err: publishErr})
	err := executor.Execute(context.Background(), []config.Action{{Type: "mqtt", Topic: "events", Payload: "x"}})
	if !errors.Is(err, publishErr) || !strings.Contains(err.Error(), "action[0] type=mqtt") {
		t.Fatalf("MQTT error = %v", err)
	}
	err = executor.Execute(context.Background(), []config.Action{{Type: "unknown"}})
	if err == nil || !strings.Contains(err.Error(), `unsupported action type "unknown"`) {
		t.Fatalf("unsupported action error = %v", err)
	}
}
