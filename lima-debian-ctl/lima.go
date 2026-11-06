package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type Instance struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	Status        string `json:"status"`
	Dir           string `json:"dir"`
	SSHConfigFile string `json:"sshConfigFile"`
	Config        struct {
		Mounts       []Mount          `json:"mounts"`
		PortForwards []map[string]any `json:"portForwards"`
	} `json:"config"`
}

func discover(ctx context.Context, r Runner, name string) (Instance, error) {
	text, err := checked(ctx, r, Command{Name: "limactl", Args: []string{"list", "--json"}})
	if err != nil {
		return Instance{}, err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	for {
		var inst Instance
		if err := dec.Decode(&inst); err == io.EOF {
			break
		} else if err != nil {
			return inst, fmt.Errorf("invalid Lima metadata: %w", err)
		}
		if inst.Name == name {
			if inst.Hostname == "" || inst.Dir == "" || inst.SSHConfigFile == "" || strings.HasPrefix(inst.Hostname, "-") || strings.ContainsAny(inst.Hostname, "\x00\r\n ") {
				return inst, fmt.Errorf("incomplete Lima SSH metadata")
			}
			return inst, nil
		}
	}
	return Instance{}, fmt.Errorf("lima instance %q does not exist; create/provision it separately", name)
}
func ignoreForwards() []map[string]any {
	// guestIPMustBeZero must be explicit: Lima defaults it to true for 0.0.0.0,
	// which would leave loopback-bound guest ports auto-forwarded.
	return []map[string]any{{"guestIP": "0.0.0.0", "guestIPMustBeZero": false, "proto": "any", "ignore": true}}
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
func hasManagedForwardPolicy(i Instance) bool {
	// Lima's metadata expands defaults; compare semantics, not raw JSON.
	if len(i.Config.PortForwards) != 1 {
		return false
	}
	rule := i.Config.PortForwards[0]
	if rule["ignore"] != true || rule["guestIP"] != "0.0.0.0" || rule["proto"] != "any" || rule["guestIPMustBeZero"] == true {
		return false
	}
	if port, ok := rule["guestPort"]; ok && port != float64(0) && port != 0 {
		return false
	}
	if socket, ok := rule["guestSocket"]; ok && socket != "" {
		return false
	}
	if ports, ok := rule["guestPortRange"].([]any); ok {
		return len(ports) == 2 && ports[0] == float64(1) && ports[1] == float64(65535)
	}
	return true
}
func editInstance(ctx context.Context, r Runner, c Config) error {
	expr := ".mounts = " + jsonString(c.Mounts) + " | .portForwards = " + jsonString(ignoreForwards())
	_, err := checked(ctx, r, Command{Name: "limactl", Args: []string{"edit", "--tty=false", "--set", expr, c.Instance}})
	return err
}
func waitInstance(ctx context.Context, r Runner, name string) (Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		inst, err := discover(ctx, r, name)
		if err == nil && inst.Status == "Running" {
			if _, err := sshScript(ctx, r, inst, "true", ""); err == nil {
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
func sshArgs(i Instance) []string {
	// -S none prevents reuse of Lima's existing master (including old group/env state).
	return []string{"-F", i.SSHConfigFile, "-S", "none", "-o", "ControlMaster=no", "-o", "ClearAllForwardings=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
}
func sshScript(ctx context.Context, r Runner, i Instance, script, input string) (string, error) {
	args := append(sshArgs(i), i.Hostname, "/bin/bash --noprofile --norc -c "+shellQuote(script))
	return checked(ctx, r, Command{Name: "ssh", Args: args, Input: input})
}
func requireGuestHelpers(ctx context.Context, r Runner, i Instance) (string, error) {
	// The helper uses the account database, not a possibly overridden HOME.
	return sshScript(ctx, r, i, "/usr/local/bin/lima-debian-ctl-guest check", "")
}
func provisionGuest(ctx context.Context, r Runner, i Instance, payload string) error {
	_, err := sshScript(ctx, r, i, "/usr/local/bin/lima-debian-ctl-guest provision", payload)
	return err
}
func clearGuest(ctx context.Context, r Runner, i Instance) error {
	_, err := sshScript(ctx, r, i, "/usr/local/bin/lima-debian-ctl-guest clear", "")
	return err
}

// pruneMounts removes empty, unmounted leftover directories under /work.
func pruneMounts(ctx context.Context, r Runner, i Instance) error {
	_, err := sshScript(ctx, r, i, "/usr/local/bin/lima-debian-ctl-guest prune-mounts", "")
	return err
}
func guestProject(ctx context.Context, r Runner, i Instance) (string, error) {
	text, err := sshScript(ctx, r, i, "/usr/local/bin/lima-debian-ctl-guest status", "")
	return strings.TrimSpace(text), err
}

// loaderProbe exercises the real login-shell startup path rather than the
// helper's direct sourcing, so readiness reflects what IDE/SSH launches see.
func loaderProbe(ctx context.Context, r Runner, i Instance) (string, error) {
	args := append(sshArgs(i), i.Hostname, "/bin/bash -lc 'printf %s \"$LIMA_DEBIAN_CTL_PROJECT\"'")
	text, err := checked(ctx, r, Command{Name: "ssh", Args: args})
	return strings.TrimSpace(text), err
}

// guestListening reports whether the guest has a TCP listener on the port.
// sshd only warns when a -R listener cannot bind, so this is checked explicitly.
func guestListening(ctx context.Context, r Runner, i Instance, port int) bool {
	script := fmt.Sprintf("/usr/bin/ss -Hltn 'sport = :%d' | /usr/bin/grep -q .", port)
	_, err := sshScript(ctx, r, i, script, "")
	return err == nil
}

func backupConfig(i Instance, dest string) error {
	data, err := os.ReadFile(i.Dir + "/lima.yaml")
	if err != nil {
		return fmt.Errorf("back up Lima config: %w", err)
	}
	return atomicWrite(dest, data)
}
