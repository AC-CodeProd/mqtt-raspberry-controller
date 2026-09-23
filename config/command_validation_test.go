package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandActionValidation(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "helper")
	if err := os.WriteFile(executable, []byte("helper"), 0700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(regular, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		action  Action
		wantErr string
	}{
		{name: "valid", action: Action{Type: "command", Command: executable, WorkingDir: dir, Env: map[string]string{"VALID_NAME": "value"}}},
		{name: "relative executable", action: Action{Type: "command", Command: "helper"}, wantErr: "absolute"},
		{name: "missing executable", action: Action{Type: "command", Command: filepath.Join(dir, "missing")}, wantErr: "cannot stat command"},
		{name: "directory executable", action: Action{Type: "command", Command: dir}, wantErr: "regular executable file"},
		{name: "not executable", action: Action{Type: "command", Command: regular}, wantErr: "regular executable file"},
		{name: "relative working directory", action: Action{Type: "command", Command: executable, WorkingDir: "relative"}, wantErr: "working_dir must be absolute"},
		{name: "missing working directory", action: Action{Type: "command", Command: executable, WorkingDir: filepath.Join(dir, "missing")}, wantErr: "cannot stat working_dir"},
		{name: "file working directory", action: Action{Type: "command", Command: executable, WorkingDir: regular}, wantErr: "working_dir must be an existing directory"},
		{name: "invalid environment name", action: Action{Type: "command", Command: executable, Env: map[string]string{"BAD-NAME": "value"}}, wantErr: "environment name"},
		{name: "empty environment name", action: Action{Type: "command", Command: executable, Env: map[string]string{"": "value"}}, wantErr: "environment name"},
		{name: "NUL environment value", action: Action{Type: "command", Command: executable, Env: map[string]string{"SECRET": "do-not-leak\x00tail"}}, wantErr: "environment value contains a NUL byte"},
		{name: "NUL argument", action: Action{Type: "command", Command: executable, Args: []string{"do-not-leak\x00tail"}}, wantErr: "argument contains a NUL byte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validSecurityConfig()
			cfg.Entities[0].Press = []Action{tt.action}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("validation error disclosed value: %v", err)
			}
		})
	}
}
