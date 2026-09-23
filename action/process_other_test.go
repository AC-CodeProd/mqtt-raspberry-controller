//go:build !linux

package action

import "testing"

func ignoreTerminationSignal()           {}
func waitForProcessExit(*testing.T, int) {}
func killPID(int)                        {}
