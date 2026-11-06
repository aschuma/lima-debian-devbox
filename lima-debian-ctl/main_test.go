package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestCLIInitAndValidation(t *testing.T) {
	file := filepath.Join(t.TempDir(), configName)
	var out, stderr bytes.Buffer
	code, err := runCLI(context.Background(), []string{"init", "--config", file}, nil, &out, &stderr)
	if err != nil || code != 0 {
		t.Fatal(err)
	}
	c, err := loadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if c.Instance != "devbox" || len(c.Mounts) != 1 {
		t.Fatal("invalid initialized config")
	}
	if _, err := runCLI(context.Background(), []string{"init", "--config", file}, nil, &out, &stderr); err == nil {
		t.Fatal("init overwrote config")
	}
	for _, args := range [][]string{{"unknown"}, {"exec"}, {"shell", "--json"}, {"status", "unexpected"}} {
		if _, err := runCLI(context.Background(), args, nil, &out, &stderr); err == nil {
			t.Fatalf("accepted invalid CLI args %v", args)
		}
	}
	for _, args := range [][]string{nil, {"--help"}, {"plan", "--help"}} {
		if _, err := runCLI(context.Background(), args, nil, &out, &stderr); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCLIPlanActivateProvisionStatusStop(t *testing.T) {
	_, c, f := testController(t)
	// OpenSSH has a small Unix socket path limit; use a short isolated HOME.
	home, err := os.MkdirTemp("/tmp", "ldc-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	data, err := toml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.File, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	run := func(command string, extra ...string) {
		t.Helper()
		out.Reset()
		args := append([]string{command, "--config", c.File, "--json"}, extra...)
		if _, err := runCLI(context.Background(), args, f, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "secret") {
			t.Fatal("CLI output leaked credentials")
		}
	}
	run("plan")
	if _, err := runCLI(context.Background(), []string{"activate", "--config", c.File, "--json"}, f, &out, &stderr); err == nil {
		t.Fatal("JSON activation did not require explicit approval")
	}
	run("activate", "--yes")
	run("status")
	if !strings.Contains(out.String(), `"ready":true`) {
		t.Fatal("status did not report readiness")
	}
	run("provision")
	run("deactivate")
}
