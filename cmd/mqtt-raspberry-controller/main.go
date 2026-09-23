package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/action"
	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
	"github.com/AC-CodeProd/mqtt-raspberry-controller/controller"
	"github.com/AC-CodeProd/mqtt-raspberry-controller/gpio"
	mqttclient "github.com/AC-CodeProd/mqtt-raspberry-controller/mqtt"
)

var version = "dev"

const shutdownTimeout = 10 * time.Second

type controllerShutdowner interface {
	Shutdown(time.Duration) error
}

type mqttShutdowner interface {
	StopAccepting()
	Close(time.Duration) error
}

type gpioCloser interface {
	Close() error
}

func shutdownService(controller controllerShutdowner, mqtt mqttShutdowner, gpio gpioCloser) error {
	deadline := time.Now().Add(shutdownTimeout)
	mqtt.StopAccepting()
	if err := controller.Shutdown(time.Until(deadline)); err != nil {
		// Do not close shared resources while a worker may still be using them.
		// Returning lets the service manager terminate the failed process.
		return err
	}
	if err := mqtt.Close(time.Until(deadline)); err != nil {
		// A callback may still be using MQTT or the controller, so GPIO must also
		// remain open until the service manager terminates the process.
		return err
	}
	if err := gpio.Close(); err != nil {
		return fmt.Errorf("close GPIO: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	configPath := flag.String("config", "/etc/mqtt-raspberry-controller/config.yaml", "Path to the YAML configuration file")
	checkConfig := flag.Bool("check", false, "Validate the configuration and exit")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	mqttClient, err := mqttclient.New(cfg)
	if err != nil {
		return fmt.Errorf("initialize MQTT client: %w", err)
	}

	if *checkConfig {
		log.Printf("configuration %s is valid", *configPath)
		return nil
	}

	gpioManager := gpio.New(cfg.GPIO)
	if err := gpioManager.OpenOutputs(); err != nil {
		return fmt.Errorf("initialize GPIO outputs: %w", err)
	}
	actionExecutor := action.New(cfg, gpioManager, mqttClient)
	entityController := controller.New(ctx, cfg, gpioManager, mqttClient, actionExecutor)

	if err := entityController.Register(); err != nil {
		return errors.Join(fmt.Errorf("register entities: %w", err), shutdownService(entityController, mqttClient, gpioManager))
	}

	if err := mqttClient.Connect(); err != nil {
		return errors.Join(fmt.Errorf("connect MQTT: %w", err), shutdownService(entityController, mqttClient, gpioManager))
	}

	if err := gpioManager.OpenInputs(entityController.HandleGPIOInput); err != nil {
		return errors.Join(fmt.Errorf("initialize GPIO inputs: %w", err), shutdownService(entityController, mqttClient, gpioManager))
	}

	// Inputs are opened after MQTT so publish their initial state now.
	entityController.PublishAllStates()

	log.Printf("mqtt-raspberry-controller %s started", version)

	<-ctx.Done()

	log.Printf("shutting down mqtt-raspberry-controller")
	return shutdownService(entityController, mqttClient, gpioManager)
}
