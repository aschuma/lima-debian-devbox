package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T, name string) Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project := filepath.Join(root, name)
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{SchemaVersion: 1, Project: name, Instance: "devbox", Root: project, File: filepath.Join(project, configName), Mounts: []Mount{{Host: project, Guest: "/work/" + name, Writable: true}}, Environment: Environment{TimeoutSeconds: 60}}
	return c
}
func TestConfigStrictAndRelative(t *testing.T) {
	c := testConfig(t, "project")
	text := "schema_version=1\nproject='project'\n[[mounts]]\nhost='.'\nguest='/work/project'\nwritable=true\n[environment]\nresolver=['./resolver']\n"
	if err := os.WriteFile(c.File, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(c.File)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mounts[0].Host != c.Root || got.Environment.Resolver[0] != filepath.Join(c.Root, "resolver") || got.Instance != "devbox" {
		t.Fatal("relative resolution/defaults incorrect")
	}
	if err := os.WriteFile(c.File, []byte("unknown=true\n"+text), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(c.File); err == nil {
		t.Fatal("accepted unknown field")
	}
}
func TestConfigRejectsUnsafeMountsAndForwards(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"home", func(c *Config) { c.Mounts[0].Host = os.Getenv("HOME") }},
		{"root", func(c *Config) { c.Mounts[0].Host = "/" }},
		{"guest-root", func(c *Config) { c.Mounts[0].Guest = "/" }},
		{"guest-traversal", func(c *Config) { c.Mounts[0].Guest = "/work/../etc" }},
		{"overlap", func(c *Config) { c.Mounts = append(c.Mounts, c.Mounts[0]) }},
		{"invalid-direction", func(c *Config) { c.Forwards = []Forward{{Direction: "out", ListenPort: 8080, TargetPort: 80}} }},
		{"privileged", func(c *Config) { c.Forwards = []Forward{{Direction: "host-to-guest", ListenPort: 80, TargetPort: 80}} }},
		{"target-injection", func(c *Config) {
			c.Forwards = []Forward{{Direction: "host-to-guest", ListenPort: 8080, TargetHost: "host;evil", TargetPort: 80}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t, "project")
			tt.change(&c)
			if err := c.validate(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestHomeSymlinkRejected(t *testing.T) {
	c := testConfig(t, "p")
	link := filepath.Join(c.Root, "home-link")
	if err := os.Symlink(os.Getenv("HOME"), link); err != nil {
		t.Fatal(err)
	}
	c.Mounts[0].Host = link
	if err := c.validate(); err == nil {
		t.Fatal("symlink bypassed home restriction")
	}
}
func TestMountExpressionEscapesPaths(t *testing.T) {
	c := testConfig(t, "project")
	c.Mounts[0].Host += ` with "quotes"`
	text := jsonString(c.Mounts)
	if !strings.Contains(text, `\"quotes\"`) {
		t.Fatal("path not JSON escaped")
	}
}
