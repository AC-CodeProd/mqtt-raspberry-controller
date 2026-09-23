package main

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeControllerShutdown struct {
	events *[]string
	err    error
}

func (f *fakeControllerShutdown) Shutdown(timeout time.Duration) error {
	if timeout <= 0 || timeout > shutdownTimeout {
		panic("unexpected shutdown timeout")
	}
	*f.events = append(*f.events, "controller")
	return f.err
}

type fakeMQTTShutdown struct {
	events *[]string
	err    error
}

func (f *fakeMQTTShutdown) StopAccepting() { *f.events = append(*f.events, "mqtt-stop") }
func (f *fakeMQTTShutdown) Close(time.Duration) error {
	*f.events = append(*f.events, "mqtt-close")
	return f.err
}

type fakeGPIOCloser struct{ events *[]string }

func (f *fakeGPIOCloser) Close() error {
	*f.events = append(*f.events, "gpio-close")
	return nil
}

func TestShutdownServiceClosesResourcesInOrder(t *testing.T) {
	var events []string
	err := shutdownService(
		&fakeControllerShutdown{events: &events},
		&fakeMQTTShutdown{events: &events},
		&fakeGPIOCloser{events: &events},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mqtt-stop", "controller", "mqtt-close", "gpio-close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("shutdown events = %v, want %v", events, want)
	}
}

func TestShutdownServiceDoesNotCloseGPIOAfterMQTTTimeout(t *testing.T) {
	var events []string
	mqttErr := errors.New("MQTT callbacks still active")
	err := shutdownService(
		&fakeControllerShutdown{events: &events},
		&fakeMQTTShutdown{events: &events, err: mqttErr},
		&fakeGPIOCloser{events: &events},
	)
	if !errors.Is(err, mqttErr) {
		t.Fatalf("shutdown error = %v, want %v", err, mqttErr)
	}
	want := []string{"mqtt-stop", "controller", "mqtt-close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("shutdown events = %v, want %v", events, want)
	}
}

func TestShutdownServiceDoesNotCloseResourcesWhileWorkersRemain(t *testing.T) {
	var events []string
	shutdownErr := errors.New("workers still active")
	err := shutdownService(
		&fakeControllerShutdown{events: &events, err: shutdownErr},
		&fakeMQTTShutdown{events: &events},
		&fakeGPIOCloser{events: &events},
	)
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("shutdown error = %v, want %v", err, shutdownErr)
	}
	want := []string{"mqtt-stop", "controller"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("shutdown events = %v, want %v", events, want)
	}
}
