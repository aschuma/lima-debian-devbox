package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	configName          = ".devvm.yaml"
	defaultInstance     = "devbox"
	defaultTimeoutSecs  = 60
	maxTimeoutSecs      = 600
	guestMountRoot      = "/work/"
	minUnprivilegedPort = 1024
	maxPort             = 65535
	configLimit         = 1 << 20
)

// Direction identifies the listener side of a custom SSH forward.
type Direction string

const (
	GuestToHost Direction = "guest-to-host"
	HostToGuest Direction = "host-to-guest"
)

// Backend selects native Lima, custom SSH or disabled local forwarding.
type Backend string

const (
	Native   Backend = "lima"
	SSH      Backend = "ssh"
	Disabled Backend = "none"
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	hostName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

// Mount uses Lima's original field names in YAML and JSON.
type Mount struct {
	Host     string `yaml:"location" json:"location"`
	Guest    string `yaml:"mountPoint" json:"mountPoint"`
	Writable bool   `yaml:"writable" json:"writable"`
}

// Forward is a normalized, controller-managed TCP tunnel.
type Forward struct {
	Direction  Direction `yaml:"-" json:"direction"`
	ListenPort int       `yaml:"listenPort" json:"listen_port"`
	TargetHost string    `yaml:"targetHost" json:"target_host"`
	TargetPort int       `yaml:"targetPort" json:"target_port"`
}

// Environment configures the host-side credential resolver.
type Environment struct {
	Resolver       []string `yaml:"resolver" json:"resolver"`
	TimeoutSeconds int      `yaml:"timeoutSeconds" json:"timeout_seconds"`
}

type environmentConfig struct {
	Resolver       []string `yaml:"resolver"`
	TimeoutSeconds *int     `yaml:"timeoutSeconds"`
}

type tunnelConfig struct {
	Backend  Backend   `yaml:"backend"`
	Forwards []Forward `yaml:"forwards"`
}

type projectFile struct {
	SchemaVersion  int               `yaml:"schemaVersion"`
	Project        string            `yaml:"project"`
	Instance       string            `yaml:"instance"`
	Environment    environmentConfig `yaml:"environment"`
	PortForwarding struct {
		Backend Backend `yaml:"backend"`
	} `yaml:"portForwarding"`
	LocalTunnels   tunnelConfig `yaml:"localTunnels"`
	ReverseTunnels tunnelConfig `yaml:"reverseTunnels"`
	Lima           struct {
		Mounts []Mount `yaml:"mounts"`
		SSH    struct {
			ForwardAgent bool `yaml:"forwardAgent"`
		} `yaml:"ssh"`
		MountInotify bool `yaml:"mountInotify"`
		Containerd   struct {
			System bool `yaml:"system"`
			User   bool `yaml:"user"`
		} `yaml:"containerd"`
		PortForwards []portForward `yaml:"portForwards"`
	} `yaml:"lima"`
}

// Config is the effective desired configuration, independent of YAML formatting.
// Root and File are derived; credentials never enter this value.
type Config struct {
	SchemaVersion  int           `json:"schema_version"`
	Project        string        `json:"project"`
	Instance       string        `json:"instance"`
	Mounts         []Mount       `json:"mounts"`
	Environment    Environment   `json:"environment"`
	Forwards       []Forward     `json:"forwards"`
	Backend        Backend       `json:"forwarding_backend"`
	NativeForwards []portForward `json:"native_forwards"`
	ForwardAgent   bool          `json:"forward_agent"`
	MountInotify   bool          `json:"mount_inotify"`
	Root           string        `json:"root"`
	File           string        `json:"file"`
}

func (c Config) forwardAgent() bool { return c.ForwardAgent }
func (c Config) mountInotify() bool { return c.MountInotify }
func (c Config) resolverTimeout() time.Duration {
	return time.Duration(c.Environment.TimeoutSeconds) * time.Second
}
func (c Config) workDir() string { return c.Mounts[0].Guest }

func loadConfig(filename string) (Config, error) {
	var c Config
	if filename == "" {
		var err error
		filename, err = findConfig()
		if err != nil {
			return c, err
		}
	}
	filename, err := filepath.Abs(filename)
	if err != nil {
		return c, err
	}
	f, err := os.Open(filename)
	if err != nil {
		return c, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, configLimit+1))
	if err != nil {
		return c, err
	}
	if len(data) > configLimit {
		return c, errors.New("project config exceeds 1 MiB")
	}
	var p projectFile
	if err := decodeConfig(data, &p); err != nil {
		return c, fmt.Errorf("invalid YAML project config: %w", err)
	}
	if p.Lima.Containerd.System || p.Lima.Containerd.User {
		return c, errors.New("lima.containerd must be disabled; the guest uses Docker")
	}
	c = Config{
		SchemaVersion: p.SchemaVersion, Project: p.Project, Instance: p.Instance,
		Environment: Environment{Resolver: p.Environment.Resolver, TimeoutSeconds: defaultTimeoutSecs}, Mounts: p.Lima.Mounts,
		Backend: p.PortForwarding.Backend, NativeForwards: p.Lima.PortForwards,
		ForwardAgent: p.Lima.SSH.ForwardAgent, MountInotify: p.Lima.MountInotify,
	}
	c.Root, err = filepath.EvalSymlinks(filepath.Dir(filename))
	if err != nil {
		return c, err
	}
	c.File = filepath.Join(c.Root, filepath.Base(filename))
	if c.Instance == "" {
		c.Instance = defaultInstance
	}
	if p.Environment.TimeoutSeconds != nil {
		c.Environment.TimeoutSeconds = *p.Environment.TimeoutSeconds
	}
	if err := c.addTunnels(p.LocalTunnels, HostToGuest); err != nil {
		return c, err
	}
	if err := c.addTunnels(p.ReverseTunnels, GuestToHost); err != nil {
		return c, err
	}
	return c, c.validate()
}

// Reject aliases and custom tags before decoding to prevent expansion and
// ambiguous schemas. KnownFields and the decoder reject unknown/duplicate keys.
func decodeConfig(data []byte, p *projectFile) error {
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&node); err != nil {
		return err
	}
	if err := validateYAML(&node); err != nil {
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("exactly one YAML document is required")
	}
	dec = yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(p)
}

func validateYAML(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("YAML anchors and aliases are not supported")
	}
	switch n.Tag {
	case "", "!!map", "!!seq", "!!str", "!!int", "!!bool", "!!null":
	default:
		return fmt.Errorf("unsupported YAML tag on line %d", n.Line)
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if key.Tag != "!!str" {
				return fmt.Errorf("mapping key on line %d must be a string", key.Line)
			}
			if err := validateFieldType(key.Value, value); err != nil {
				return err
			}
		}
	}
	for _, child := range n.Content {
		if err := validateYAML(child); err != nil {
			return err
		}
	}
	return nil
}

// yaml/v3 otherwise permits scalar coercion (for example integer project names
// and YAML 1.1 yes/no booleans). Keep the documented YAML types authoritative.
func validateFieldType(key string, value *yaml.Node) error {
	if value.Tag == "!!null" {
		return nil
	}
	tag := ""
	switch key {
	case "project", "instance", "backend", "location", "mountPoint", "targetHost", "guestIP", "hostIP", "proto":
		tag = "!!str"
	case "writable", "forwardAgent", "mountInotify", "system", "user", "guestIPMustBeZero", "static":
		tag = "!!bool"
	case "schemaVersion", "listenPort", "targetPort", "guestPort", "hostPort", "timeoutSeconds":
		tag = "!!int"
	}
	if tag != "" && value.Tag != tag {
		return fmt.Errorf("field %s on line %d must be %s", key, value.Line, strings.TrimPrefix(tag, "!!"))
	}
	if value.Kind == yaml.SequenceNode && (key == "resolver" || key == "guestPortRange" || key == "hostPortRange") {
		tag := "!!int"
		if key == "resolver" {
			tag = "!!str"
		}
		for _, item := range value.Content {
			if item.Tag != tag {
				return fmt.Errorf("field %s on line %d requires %s items", key, item.Line, strings.TrimPrefix(tag, "!!"))
			}
		}
	}
	return nil
}

func (c *Config) addTunnels(t tunnelConfig, direction Direction) error {
	if t.Backend != "" && t.Backend != SSH && t.Backend != Disabled {
		return errors.New("custom tunnel backend must be ssh or none")
	}
	if direction == HostToGuest && c.Backend == SSH && t.Backend == Disabled {
		return errors.New("localTunnels.backend must match the ssh local selector")
	}
	if len(t.Forwards) == 0 {
		return nil
	}
	if t.Backend != SSH {
		return errors.New("populated custom tunnels require backend: ssh")
	}
	if direction == HostToGuest && c.Backend != SSH {
		return errors.New("localTunnels require portForwarding.backend: ssh")
	}
	for _, f := range t.Forwards {
		f.Direction = direction
		c.Forwards = append(c.Forwards, f)
	}
	return nil
}

func findConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, configName)
		_, err := os.Stat(candidate)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found; run init or pass --config", configName)
		}
		dir = parent
	}
}

func (c *Config) validate() error {
	if c.SchemaVersion != 2 {
		return errors.New("schemaVersion must be 2; only the new YAML format is supported")
	}
	if !identifier.MatchString(c.Project) || !identifier.MatchString(c.Instance) {
		return errors.New("project and instance must be simple names (letters, digits, hyphens, underscores)")
	}
	if c.Backend != Native && c.Backend != SSH && c.Backend != Disabled {
		return errors.New("portForwarding.backend must explicitly be lima, ssh or none")
	}
	if c.Backend != Native && len(c.NativeForwards) > 0 {
		return errors.New("lima.portForwards requires portForwarding.backend: lima")
	}
	if err := c.validateMounts(); err != nil {
		return err
	}
	if err := c.validateResolver(); err != nil {
		return err
	}
	if err := c.validateForwards(); err != nil {
		return err
	}
	return c.validateNativeForwards()
}

func (c *Config) validateMounts() error {
	if len(c.Mounts) == 0 {
		return errors.New("at least one project mount is required")
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
		if m.Host == "" {
			return fmt.Errorf("mount %d requires location", i+1)
		}
		if strings.Contains(m.Host, "{{") || strings.Contains(m.Guest, "{{") {
			return errors.New("lima Go-template expressions are not supported in mount paths")
		}
		if m.Host == "~" {
			m.Host = home
		}
		if strings.HasPrefix(m.Host, "~/") {
			m.Host = filepath.Join(home, m.Host[2:])
		}
		if strings.HasPrefix(m.Host, "~") {
			return errors.New("only ~/ home expansion is supported")
		}
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
	if strings.Contains(exe, string(filepath.Separator)) && !filepath.IsAbs(exe) {
		env.Resolver[0] = filepath.Join(c.Root, exe)
	}
	return nil
}

func (c *Config) validateForwards() error {
	seen := make(map[string]bool)
	for i := range c.Forwards {
		f := &c.Forwards[i]
		if f.Direction != GuestToHost && f.Direction != HostToGuest {
			return errors.New("invalid custom forward direction")
		}
		if f.Direction == HostToGuest && c.Backend != SSH {
			return errors.New("local SSH forwards require backend: ssh")
		}
		if f.ListenPort < minUnprivilegedPort || f.ListenPort > maxPort || f.TargetPort < 1 || f.TargetPort > maxPort {
			return errors.New("custom listener must be 1024..65535 and target 1..65535")
		}
		if f.TargetHost == "" {
			f.TargetHost = "127.0.0.1"
		}
		if net.ParseIP(f.TargetHost) == nil && !hostName.MatchString(f.TargetHost) {
			return errors.New("invalid custom target host")
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
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
