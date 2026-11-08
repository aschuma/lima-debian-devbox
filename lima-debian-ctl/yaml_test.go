package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProject(t *testing.T, c Config, text string) {
	t.Helper()
	if err := os.WriteFile(c.File, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

const yamlProject = "schemaVersion: 2\nproject: project\nportForwarding: {backend: none}\nlima:\n  mounts:\n    - location: .\n      mountPoint: /work/project\n"

func TestYAMLDefaults(t *testing.T) {
	c := testConfig(t, "project")
	writeProject(t, c, yamlProject)
	got, err := loadConfig(c.File)
	if err != nil {
		t.Fatal(err)
	}
	if got.forwardAgent() || got.mountInotify() || got.Mounts[0].Writable || got.Backend != Disabled || got.Environment.TimeoutSeconds != 60 {
		t.Fatalf("incorrect defaults: %+v", got)
	}
	if !got.forwardPolicy()[0].isIgnoreAll() {
		t.Fatal("disabled mode did not generate deny-all policy")
	}
}

func TestYAMLRejectsInvalidInputBeforeExecution(t *testing.T) {
	tests := map[string]string{
		"toml":             "schema_version=1\nproject='project'\n",
		"schema":           strings.Replace(yamlProject, "schemaVersion: 2", "schemaVersion: 1", 1),
		"numeric-name":     strings.Replace(yamlProject, "project: project", "project: 42", 1),
		"legacy-boolean":   yamlProject + "  ssh: {forwardAgent: yes}\n",
		"numeric-resolver": yamlProject + "environment: {resolver: [42]}\n",
		"missing-backend":  strings.Replace(yamlProject, "portForwarding: {backend: none}\n", "", 1),
		"duplicate":        "project: other\n" + yamlProject,
		"documents":        yamlProject + "---\n{}\n",
		"tag":              strings.Replace(yamlProject, "project: project", "project: !custom project", 1),
		"anchor":           strings.Replace(yamlProject, "project: project", "project: &name project", 1),
		"unknown-native":   yamlProject + "  cpus: 4\n",
		"unknown-ssh":      yamlProject + "  ssh: {forwardX11: true}\n",
		"containerd":       yamlProject + "  containerd: {user: true}\n",
		"timeout-zero":     yamlProject + "environment: {timeoutSeconds: 0}\n",
		"timeout-high":     yamlProject + "environment: {timeoutSeconds: 601}\n",
		"template":         strings.Replace(yamlProject, "location: .", "location: '{{.Dir}}'", 1),
		"socket":           yamlProject + "  portForwards: [{guestSocket: /run/docker.sock, hostSocket: docker.sock}]\n",
		"inactive-native":  yamlProject + "  portForwards: [{guestPort: 3000, hostPort: 8080}]\n",
		"inactive-local":   yamlProject + "localTunnels: {backend: ssh, forwards: [{listenPort: 8080, targetPort: 3000}]}\n",
		"custom-udp":       strings.Replace(yamlProject, "backend: none", "backend: ssh", 1) + "localTunnels: {backend: ssh, forwards: [{listenPort: 8080, targetPort: 3000, proto: udp}]}\n",
		"custom-range":     strings.Replace(yamlProject, "backend: none", "backend: ssh", 1) + "localTunnels: {backend: ssh, forwards: [{listenPortRange: [8080, 8090], targetPort: 3000}]}\n",
		"custom-socket":    yamlProject + "reverseTunnels: {backend: ssh, forwards: [{listenSocket: socket, targetSocket: other}]}\n",
		"reverse-none":     yamlProject + "reverseTunnels: {backend: none, forwards: [{listenPort: 1080, targetPort: 1080}]}\n",
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			c := testConfig(t, "project")
			writeProject(t, c, text)
			r := runnerFunc(func(context.Context, Command) (Result, error) {
				t.Fatal("executed command for invalid YAML")
				return Result{}, nil
			})
			_, err := runCLI(context.Background(), []string{"activate", "--yes", "--config", c.File}, r, &strings.Builder{}, &strings.Builder{})
			if err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestYAMLCustomForwardVariants(t *testing.T) {
	c := testConfig(t, "project")
	text := strings.Replace(yamlProject, "backend: none", "backend: ssh", 1) +
		"localTunnels: {backend: ssh, forwards: [{listenPort: 8080, targetPort: 3000}]}\nreverseTunnels: {backend: ssh, forwards: [{listenPort: 1080, targetHost: registry.example, targetPort: 80}]}\n"
	writeProject(t, c, text)
	got, err := loadConfig(c.File)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Forwards) != 2 || got.Forwards[0].Direction != HostToGuest || got.Forwards[1].Direction != GuestToHost || got.Forwards[0].TargetHost != "127.0.0.1" {
		t.Fatalf("incorrect custom forwarding: %+v", got.Forwards)
	}
}

func TestYAMLCanonicalHashAndNull(t *testing.T) {
	c := testConfig(t, "project")
	writeProject(t, c, yamlProject)
	a, err := loadConfig(c.File)
	if err != nil {
		t.Fatal(err)
	}
	writeProject(t, c, "# comment\n"+yamlProject+"  ssh: {forwardAgent: null}\n  mountInotify: null\n  containerd: {user: null, system: null}\n")
	b, err := loadConfig(c.File)
	if err != nil {
		t.Fatal(err)
	}
	if configHash(a) != configHash(b) {
		t.Fatal("comments or native null defaults changed effective hash")
	}
}

func TestYAMLHomeExpansionAndSizeBound(t *testing.T) {
	c := testConfig(t, "project")
	homeProject := filepath.Join(os.Getenv("HOME"), "src")
	if err := os.Mkdir(homeProject, 0700); err != nil {
		t.Fatal(err)
	}
	writeProject(t, c, strings.Replace(yamlProject, "location: .", "location: ~/src", 1))
	got, err := loadConfig(c.File)
	if err != nil || got.Mounts[0].Host != homeProject {
		t.Fatalf("home expansion: %+v, %v", got, err)
	}
	writeProject(t, c, strings.Repeat("#", configLimit+1))
	if _, err := loadConfig(c.File); err == nil {
		t.Fatal("accepted oversized config")
	}
}
