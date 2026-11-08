package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Lima instance states as reported by `limactl list --json`.
const (
	statusRunning = "Running"
	statusStopped = "Stopped"
)

const sshReadyTimeout = 3 * time.Minute

// Instance is the subset of Lima's instance metadata the controller uses.
type Instance struct {
	Name          string         `json:"name"`
	Hostname      string         `json:"hostname"`
	Status        string         `json:"status"`
	Dir           string         `json:"dir"`
	SSHConfigFile string         `json:"sshConfigFile"`
	Config        instanceConfig `json:"config"`
}

type instanceConfig struct {
	Mounts       []Mount       `json:"mounts"`
	PortForwards []portForward `json:"portForwards"`
	SSH          instanceSSH   `json:"ssh"`
}

type instanceSSH struct {
	ForwardAgent bool `json:"forwardAgent"`
}

// portForward mirrors the Lima rule fields needed to recognise the managed
// "ignore everything" policy. Pointers distinguish "unset" from "false".
type portForward struct {
	GuestIP           string `json:"guestIP,omitempty"`
	GuestIPMustBeZero *bool  `json:"guestIPMustBeZero,omitempty"`
	GuestPort         int    `json:"guestPort,omitempty"`
	GuestPortRange    []int  `json:"guestPortRange,omitempty"`
	GuestSocket       string `json:"guestSocket,omitempty"`
	Proto             string `json:"proto,omitempty"`
	Ignore            bool   `json:"ignore,omitempty"`
}

// ignoreAllForwards disables Lima's automatic guest-port forwarding so only
// the project's explicit tunnels (and Lima's own SSH port) are exposed.
// guestIPMustBeZero must be explicit: Lima defaults it to true for 0.0.0.0,
// which would leave loopback-bound guest ports auto-forwarded.
func ignoreAllForwards() []portForward {
	no := false
	return []portForward{{GuestIP: "0.0.0.0", GuestIPMustBeZero: &no, Proto: "any", Ignore: true}}
}

// isIgnoreAll recognises ignoreAllForwards after Lima has filled in defaults.
func (p portForward) isIgnoreAll() bool {
	if !p.Ignore || p.GuestIP != "0.0.0.0" || p.Proto != "any" || p.GuestSocket != "" || p.GuestPort != 0 {
		return false
	}
	if p.GuestIPMustBeZero != nil && *p.GuestIPMustBeZero {
		return false
	}
	return len(p.GuestPortRange) == 0 || (len(p.GuestPortRange) == 2 && p.GuestPortRange[0] == 1 && p.GuestPortRange[1] == maxPort)
}

// matchesProject reports whether the instance already carries exactly the
// project's mounts, the managed forwarding policy and its agent-forwarding
// setting.
func (i Instance) matchesProject(c Config) bool {
	return mountsEqual(i.Config.Mounts, c.Mounts) &&
		len(i.Config.PortForwards) == 1 && i.Config.PortForwards[0].isIgnoreAll() &&
		i.Config.SSH.ForwardAgent == c.forwardAgent()
}

func mountsEqual(a, b []Mount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// limaClient wraps the limactl subcommands the controller needs.
type limaClient struct{ r Runner }

// run executes a limactl subcommand. Unlike the generic run helper it reports
// limactl's stderr: limactl never sees credential values, and its messages
// ("cannot edit a running instance", a degraded start, ...) are what the user
// needs to act on.
func (l limaClient) run(ctx context.Context, args ...string) error {
	result, err := l.r.Run(ctx, Command{Name: "limactl", Args: args})
	if err != nil {
		return fmt.Errorf("limactl %s failed: %w", args[0], err)
	}
	if result.Code != 0 {
		if msg := lastLimaMessage(result.Stderr); msg != "" {
			return fmt.Errorf("limactl %s exited with status %d: %s", args[0], result.Code, msg)
		}
		return fmt.Errorf("limactl %s exited with status %d", args[0], result.Code)
	}
	return nil
}

// lastLimaMessage returns the final non-empty stderr line, which is where
// limactl puts its fatal/error summary, stripped of its log prefix.
func lastLimaMessage(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if _, msg, ok := strings.Cut(line, "msg="); ok {
			return strings.Trim(msg, `"`)
		}
		return line
	}
	return ""
}

func (l limaClient) Stop(ctx context.Context, name string) error {
	return l.run(ctx, "stop", "--tty=false", name)
}

func (l limaClient) Start(ctx context.Context, name string) error {
	return l.run(ctx, "start", "--tty=false", name)
}

// Edit replaces the instance's mounts and forwarding policy and sets SSH
// agent forwarding from the project config. Lima refuses to edit a running
// instance, so callers stop it first.
func (l limaClient) Edit(ctx context.Context, c Config) error {
	expr := ".mounts = " + jsonString(c.Mounts) +
		" | .portForwards = " + jsonString(ignoreAllForwards()) +
		" | .ssh.forwardAgent = " + strconv.FormatBool(c.forwardAgent())
	return l.run(ctx, "edit", "--tty=false", "--set", expr, c.Instance)
}

// Discover returns the named instance from `limactl list --json`.
func (l limaClient) Discover(ctx context.Context, name string) (Instance, error) {
	text, err := run(ctx, l.r, Command{Name: "limactl", Args: []string{"list", "--json"}})
	if err != nil {
		return Instance{}, err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	for {
		var inst Instance
		if err := dec.Decode(&inst); errors.Is(err, io.EOF) {
			return Instance{}, fmt.Errorf("lima instance %q does not exist; create/provision it separately", name)
		} else if err != nil {
			return Instance{}, fmt.Errorf("invalid Lima metadata: %w", err)
		}
		if inst.Name != name {
			continue
		}
		if inst.Hostname == "" || inst.Dir == "" || inst.SSHConfigFile == "" ||
			strings.HasPrefix(inst.Hostname, "-") || strings.ContainsAny(inst.Hostname, "\x00\r\n ") {
			return inst, errors.New("incomplete Lima SSH metadata")
		}
		return inst, nil
	}
}

// WaitReady polls until the instance is running and accepts SSH.
func (l limaClient) WaitReady(ctx context.Context, name string) (Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, sshReadyTimeout)
	defer cancel()
	for {
		inst, err := l.Discover(ctx, name)
		if err == nil && inst.Status == statusRunning {
			if _, err := newGuest(l.r, inst).bash(ctx, "true", ""); err == nil {
				return inst, nil
			}
		}
		select {
		case <-ctx.Done():
			return Instance{}, fmt.Errorf("waiting for guest SSH: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// BackupConfig copies the instance's lima.yaml verbatim before an edit.
func (l limaClient) BackupConfig(inst Instance, dest string) error {
	data, err := os.ReadFile(filepath.Join(inst.Dir, "lima.yaml"))
	if err != nil {
		return fmt.Errorf("back up Lima config: %w", err)
	}
	return atomicWrite(dest, data)
}
