package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const configName = ".devvm.toml"

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

type Mount struct {
	Host     string `toml:"host" json:"location"`
	Guest    string `toml:"guest" json:"mountPoint"`
	Writable bool   `toml:"writable" json:"writable"`
}
type Forward struct {
	Direction  string `toml:"direction" json:"direction"`
	ListenPort int    `toml:"listen_port" json:"listen_port"`
	TargetHost string `toml:"target_host" json:"target_host"`
	TargetPort int    `toml:"target_port" json:"target_port"`
}
type Environment struct {
	Resolver       []string `toml:"resolver" json:"resolver"`
	TimeoutSeconds int      `toml:"timeout_seconds" json:"timeout_seconds"`
}
type Config struct {
	SchemaVersion int         `toml:"schema_version" json:"schema_version"`
	Project       string      `toml:"project" json:"project"`
	Instance      string      `toml:"instance" json:"instance"`
	Mounts        []Mount     `toml:"mounts" json:"mounts"`
	Environment   Environment `toml:"environment" json:"environment"`
	Forwards      []Forward   `toml:"forwards" json:"forwards"`
	Root          string      `toml:"-" json:"root"`
	File          string      `toml:"-" json:"file"`
}

func loadConfig(filename string) (Config, error) {
	var c Config
	if filename == "" {
		dir, err := os.Getwd()
		if err != nil {
			return c, err
		}
		for {
			candidate := filepath.Join(dir, configName)
			if _, err := os.Stat(candidate); err == nil {
				filename = candidate
				break
			} else if !os.IsNotExist(err) {
				return c, err
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return c, fmt.Errorf("no %s found; run init or pass --config", configName)
			}
			dir = parent
		}
	}
	filename, err := filepath.Abs(filename)
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return c, err
	}
	if err := toml.NewDecoder(strings.NewReader(string(data))).DisallowUnknownFields().Decode(&c); err != nil {
		return c, fmt.Errorf("invalid project config: %w", err)
	}
	c.Root, err = filepath.EvalSymlinks(filepath.Dir(filename))
	if err != nil {
		return c, err
	}
	c.File = filepath.Join(c.Root, filepath.Base(filename))
	if c.Instance == "" {
		c.Instance = "devbox"
	}
	if c.Environment.TimeoutSeconds == 0 {
		c.Environment.TimeoutSeconds = 60
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if !identifier.MatchString(c.Project) || !identifier.MatchString(c.Instance) {
		return fmt.Errorf("project and instance must be simple names (letters, digits, hyphens, underscores)")
	}
	if len(c.Mounts) == 0 {
		return fmt.Errorf("at least one project mount is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	for i := range c.Mounts {
		m := &c.Mounts[i]
		if !filepath.IsAbs(m.Host) {
			m.Host = filepath.Join(c.Root, m.Host)
		}
		m.Host, err = filepath.EvalSymlinks(m.Host)
		if err != nil {
			return fmt.Errorf("mount %d: %w", i+1, err)
		}
		info, err := os.Stat(m.Host)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("mount %d must be an existing directory", i+1)
		}
		// Never expose the host home or an ancestor of it, even via a symlink.
		if within(m.Host, home) {
			return fmt.Errorf("mount %d exposes the host home or an ancestor", i+1)
		}
		if !strings.HasPrefix(m.Guest, "/work/") || path.Clean(m.Guest) != m.Guest || strings.ContainsAny(m.Guest, "\x00\r\n") {
			return fmt.Errorf("mount %d guest path must be a clean absolute path beneath /work/", i+1)
		}
		if strings.ContainsAny(m.Host, "\x00\r\n") {
			return fmt.Errorf("mount %d has an unsupported host path", i+1)
		}
		for j := 0; j < i; j++ {
			if within(c.Mounts[j].Host, m.Host) || within(m.Host, c.Mounts[j].Host) || within(c.Mounts[j].Guest, m.Guest) || within(m.Guest, c.Mounts[j].Guest) {
				return fmt.Errorf("mounts must not overlap")
			}
		}
	}
	if c.Environment.TimeoutSeconds < 1 || c.Environment.TimeoutSeconds > 600 {
		return fmt.Errorf("resolver timeout must be between 1 and 600 seconds")
	}
	for _, arg := range c.Environment.Resolver {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("resolver arguments must not contain NUL")
		}
	}
	if len(c.Environment.Resolver) > 0 {
		if c.Environment.Resolver[0] == "" {
			return fmt.Errorf("resolver executable is empty")
		}
		if strings.Contains(c.Environment.Resolver[0], string(filepath.Separator)) && !filepath.IsAbs(c.Environment.Resolver[0]) {
			c.Environment.Resolver[0] = filepath.Join(c.Root, c.Environment.Resolver[0])
		}
	}
	seen := map[string]bool{}
	for i := range c.Forwards {
		f := &c.Forwards[i]
		if f.Direction != "guest-to-host" && f.Direction != "host-to-guest" {
			return fmt.Errorf("forward direction must be guest-to-host or host-to-guest")
		}
		if f.ListenPort < 1024 || f.ListenPort > 65535 || f.TargetPort < 1 || f.TargetPort > 65535 {
			return fmt.Errorf("forward listener must be 1024..65535 and target 1..65535")
		}
		if f.TargetHost == "" {
			f.TargetHost = "127.0.0.1"
		}
		if ip := net.ParseIP(f.TargetHost); ip == nil && !hostName.MatchString(f.TargetHost) {
			return fmt.Errorf("invalid forward target host")
		}
		key := fmt.Sprintf("%s:%d", f.Direction, f.ListenPort)
		if seen[key] {
			return fmt.Errorf("duplicate forward listener %s", key)
		}
		seen[key] = true
	}
	return nil
}
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (c Config) resolverTimeout() time.Duration {
	return time.Duration(c.Environment.TimeoutSeconds) * time.Second
}
func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
