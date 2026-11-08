package main

import (
	"encoding/json"
	"errors"
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

const (
	configName          = ".devvm.toml"
	defaultInstance     = "devbox"
	defaultTimeoutSecs  = 60
	maxTimeoutSecs      = 600
	guestMountRoot      = "/work/"
	minUnprivilegedPort = 1024
	maxPort             = 65535
)

// Direction of a managed SSH port forward.
type Direction string

const (
	GuestToHost Direction = "guest-to-host" // ssh -R: guest listener → host target
	HostToGuest Direction = "host-to-guest" // ssh -L: host listener → guest target
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	hostName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

// Mount is one host directory exposed to the guest. JSON tags match Lima's
// mount schema so the slice can be passed straight to `limactl edit --set`.
type Mount struct {
	Host     string `toml:"host" json:"location"`
	Guest    string `toml:"guest" json:"mountPoint"`
	Writable bool   `toml:"writable" json:"writable"`
}

// Forward is one loopback-bound TCP tunnel owned by the active project.
type Forward struct {
	Direction  Direction `toml:"direction" json:"direction"`
	ListenPort int       `toml:"listen_port" json:"listen_port"`
	TargetHost string    `toml:"target_host" json:"target_host"`
	TargetPort int       `toml:"target_port" json:"target_port"`
}

// Environment configures the host-side credential resolver.
type Environment struct {
	Resolver       []string `toml:"resolver" json:"resolver"`
	TimeoutSeconds int      `toml:"timeout_seconds" json:"timeout_seconds"`
}

// Config is a validated .devvm.toml. Root and File are derived, never read.
type Config struct {
	SchemaVersion int         `toml:"schema_version" json:"schema_version"`
	Project       string      `toml:"project" json:"project"`
	Instance      string      `toml:"instance" json:"instance"`
	Mounts        []Mount     `toml:"mounts" json:"mounts"`
	Environment   Environment `toml:"environment" json:"environment"`
	Forwards      []Forward   `toml:"forwards" json:"forwards"`
	// ForwardAgent enables Lima's ssh.forwardAgent so guest git/ssh use the
	// host agent through /run/host-services/ssh-auth.sock; no private key is
	// ever written to the guest. nil means unset and defaults to true.
	ForwardAgent *bool `toml:"forward_agent" json:"forward_agent"`
	// MountInotify enables Lima's mountInotify so host file changes on
	// writable mounts raise inotify events in the guest (IDE/build watchers).
	// nil means unset and defaults to true.
	MountInotify *bool  `toml:"mount_inotify" json:"mount_inotify"`
	Root         string `toml:"-" json:"root"`
	File         string `toml:"-" json:"file"`
}

// forwardAgent returns the effective agent-forwarding setting (default on).
func (c Config) forwardAgent() bool { return c.ForwardAgent == nil || *c.ForwardAgent }

// mountInotify returns the effective inotify-forwarding setting (default on).
func (c Config) mountInotify() bool { return c.MountInotify == nil || *c.MountInotify }

func (c Config) resolverTimeout() time.Duration {
	return time.Duration(c.Environment.TimeoutSeconds) * time.Second
}

// workDir is the guest directory shell/exec start in: the first mount.
func (c Config) workDir() string { return c.Mounts[0].Guest }

// loadConfig reads and validates filename, or searches upward from the
// current directory when filename is empty.
func loadConfig(filename string) (Config, error) {
	var c Config
	if filename == "" {
		var err error
		if filename, err = findConfig(); err != nil {
			return c, err
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
	if c.Root, err = filepath.EvalSymlinks(filepath.Dir(filename)); err != nil {
		return c, err
	}
	c.File = filepath.Join(c.Root, filepath.Base(filename))
	if c.Instance == "" {
		c.Instance = defaultInstance
	}
	if c.Environment.TimeoutSeconds == 0 {
		c.Environment.TimeoutSeconds = defaultTimeoutSecs
	}
	if c.ForwardAgent == nil {
		on := true
		c.ForwardAgent = &on
	}
	if c.MountInotify == nil {
		on := true
		c.MountInotify = &on
	}
	return c, c.validate()
}

func findConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, configName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found; run init or pass --config", configName)
		}
		dir = parent
	}
}

// validate normalizes paths in place and rejects unsafe or ambiguous input.
func (c *Config) validate() error {
	if c.SchemaVersion != 1 {
		return errors.New("schema_version must be 1")
	}
	if !identifier.MatchString(c.Project) || !identifier.MatchString(c.Instance) {
		return errors.New("project and instance must be simple names (letters, digits, hyphens, underscores)")
	}
	if err := c.validateMounts(); err != nil {
		return err
	}
	if err := c.validateResolver(); err != nil {
		return err
	}
	return c.validateForwards()
}

func (c *Config) validateMounts() error {
	if len(c.Mounts) == 0 {
		return errors.New("at least one project mount is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return err
	}
	for i := range c.Mounts {
		m := &c.Mounts[i]
		if !filepath.IsAbs(m.Host) {
			m.Host = filepath.Join(c.Root, m.Host)
		}
		if m.Host, err = filepath.EvalSymlinks(m.Host); err != nil {
			return fmt.Errorf("mount %d: %w", i+1, err)
		}
		if info, err := os.Stat(m.Host); err != nil || !info.IsDir() {
			return fmt.Errorf("mount %d must be an existing directory", i+1)
		}
		// Never expose the host home or an ancestor of it, even via a symlink.
		if within(m.Host, home) {
			return fmt.Errorf("mount %d exposes the host home or an ancestor", i+1)
		}
		if strings.ContainsAny(m.Host, "\x00\r\n") {
			return fmt.Errorf("mount %d has an unsupported host path", i+1)
		}
		if !strings.HasPrefix(m.Guest, guestMountRoot) || path.Clean(m.Guest) != m.Guest || strings.ContainsAny(m.Guest, "\x00\r\n") {
			return fmt.Errorf("mount %d guest path must be a clean absolute path beneath %s", i+1, guestMountRoot)
		}
		for _, other := range c.Mounts[:i] {
			if overlaps(other.Host, m.Host) || overlaps(other.Guest, m.Guest) {
				return errors.New("mounts must not overlap")
			}
		}
	}
	return nil
}

func (c *Config) validateResolver() error {
	env := &c.Environment
	if env.TimeoutSeconds < 1 || env.TimeoutSeconds > maxTimeoutSecs {
		return fmt.Errorf("resolver timeout must be between 1 and %d seconds", maxTimeoutSecs)
	}
	for _, arg := range env.Resolver {
		if strings.ContainsRune(arg, 0) {
			return errors.New("resolver arguments must not contain NUL")
		}
	}
	if len(env.Resolver) == 0 {
		return nil
	}
	exe := env.Resolver[0]
	if exe == "" {
		return errors.New("resolver executable is empty")
	}
	// "./script" is project-relative; a bare name is looked up on PATH.
	if strings.Contains(exe, string(filepath.Separator)) && !filepath.IsAbs(exe) {
		env.Resolver[0] = filepath.Join(c.Root, exe)
	}
	return nil
}

func (c *Config) validateForwards() error {
	seen := make(map[string]bool, len(c.Forwards))
	for i := range c.Forwards {
		f := &c.Forwards[i]
		if f.Direction != GuestToHost && f.Direction != HostToGuest {
			return fmt.Errorf("forward direction must be %s or %s", GuestToHost, HostToGuest)
		}
		if f.ListenPort < minUnprivilegedPort || f.ListenPort > maxPort || f.TargetPort < 1 || f.TargetPort > maxPort {
			return fmt.Errorf("forward listener must be %d..%d and target 1..%d", minUnprivilegedPort, maxPort, maxPort)
		}
		if f.TargetHost == "" {
			f.TargetHost = "127.0.0.1"
		}
		if net.ParseIP(f.TargetHost) == nil && !hostName.MatchString(f.TargetHost) {
			return errors.New("invalid forward target host")
		}
		key := fmt.Sprintf("%s:%d", f.Direction, f.ListenPort)
		if seen[key] {
			return fmt.Errorf("duplicate forward listener %s", key)
		}
		seen[key] = true
	}
	return nil
}

// within reports whether child is parent or lies beneath it.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

// jsonString marshals values that cannot fail (plain structs and strings).
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
