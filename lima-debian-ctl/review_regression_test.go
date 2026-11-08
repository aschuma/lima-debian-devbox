package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeProtocolUsesLimaDefault(t *testing.T) {
	p := portForward{GuestPort: 3000, HostPort: 8080}
	if err := normalizeForward(&p); err != nil {
		t.Fatal(err)
	}
	if p.Proto != "any" {
		t.Fatalf("default protocol = %q, want Lima's any", p.Proto)
	}
}

func TestNativeYAMLDefaultsAndStaticProtocolConstraint(t *testing.T) {
	c := testConfig(t, "project")
	text := strings.Replace(yamlProject, "backend: none", "backend: lima", 1) + "  portForwards:\n    - guestPort: 3000\n      hostPort: 8080\n      guestIPMustBeZero: true\n"
	writeProject(t, c, text)
	got, err := loadConfig(c.File)
	if err != nil {
		t.Fatal(err)
	}
	p := got.NativeForwards[0]
	if p.Proto != "any" || p.GuestIP != "0.0.0.0" {
		t.Fatalf("YAML defaults differ from Lima: %+v", p)
	}
	writeProject(t, c, text+"      static: true\n")
	if _, err := loadConfig(c.File); err == nil {
		t.Fatal("static forwarding accepted implicit any protocol")
	}
	writeProject(t, c, text+"      static: true\n      proto: tcp\n")
	if _, err := loadConfig(c.File); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGuestIPUsesMatchingFlagDefault(t *testing.T) {
	zero := true
	p := portForward{GuestPort: 3000, HostPort: 8080, GuestIPMustBeZero: &zero}
	if err := normalizeForward(&p); err != nil {
		t.Fatal(err)
	}
	if p.GuestIP != "0.0.0.0" {
		t.Fatalf("default guest IP = %q, want 0.0.0.0", p.GuestIP)
	}
}

func TestNativeStaticCannotSuppressDynamicRules(t *testing.T) {
	for _, proto := range []string{"tcp", "udp", "any"} {
		for _, staticFirst := range []bool{false, true} {
			name := proto + "/dynamic-first"
			if staticFirst {
				name = proto + "/static-first"
			}
			t.Run(name, func(t *testing.T) {
				c := testConfig(t, "project")
				c.Backend = Native
				static := portForward{GuestIP: "127.0.0.1", GuestPort: 5353, HostPort: 15353, Proto: "tcp", Static: true}
				dynamic := portForward{GuestIP: "127.0.0.2", GuestPortRange: []int{5352, 5354}, HostPortRange: []int{15354, 15356}, Proto: proto}
				c.NativeForwards = []portForward{dynamic, static}
				if staticFirst {
					c.NativeForwards = []portForward{static, dynamic}
				}
				if err := c.validate(); err == nil {
					t.Fatal("accepted a dynamic rule Lima suppresses by guest port regardless of IP/protocol")
				}
			})
		}
	}
}

func TestNativeIndependentMappingsRemainSupported(t *testing.T) {
	for _, rules := range [][]portForward{
		{{GuestPort: 5353, HostPort: 15353, Proto: "tcp"}, {GuestPort: 5353, HostPort: 15353, Proto: "udp"}},
		{{GuestIP: "127.0.0.1", GuestPort: 5353, HostPort: 15353, Proto: "tcp", Static: true}, {GuestIP: "127.0.0.2", GuestPort: 5353, HostPort: 15354, Proto: "tcp", Static: true}},
	} {
		c := testConfig(t, "project")
		c.Backend = Native
		c.NativeForwards = rules
		if err := c.validate(); err != nil {
			t.Fatalf("rejected independent native mappings: %v", err)
		}
	}
}

func TestEveryBackendSwitchRestarts(t *testing.T) {
	backends := []Backend{Native, SSH, Disabled}
	for _, from := range backends {
		for _, to := range backends {
			if from == to {
				continue
			}
			t.Run(string(from)+"-to-"+string(to), func(t *testing.T) {
				a, c, f := testController(t)
				// Empty rule lists isolate the backend selector: the native policy is
				// identical, but the agreed backend transition still requires restart.
				c.Backend = from
				if err := os.WriteFile(filepath.Join(f.inst.Dir, "ha.pid"), []byte("6000\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := activate(t, a, context.Background(), c, true); err != nil {
					t.Fatal(err)
				}
				stops := countCalls(f, "limactl", "stop")
				c.Backend = to
				rec, err := a.plan(context.Background(), c)
				if err != nil {
					t.Fatal(err)
				}
				if !rec.Plan.Restart || !rec.Plan.Edit || !rec.Plan.ConfirmationRequired {
					t.Fatalf("backend change did not require stop/edit/start and approval: %+v", rec.Plan)
				}
				if _, err := a.Activate(context.Background(), c, rec, true); err != nil {
					t.Fatal(err)
				}
				if countCalls(f, "limactl", "stop") != stops+1 {
					t.Fatal("backend change did not restart VM")
				}
				state, err := a.Store.load()
				if err != nil || state.Backend != to {
					t.Fatalf("backend was not recorded: %+v, %v", state, err)
				}
			})
		}
	}
}

func TestLegacyStateWithoutBackendRequiresActivation(t *testing.T) {
	a, c, _ := testController(t)
	ctx := context.Background()
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.Store.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	delete(state, "forwarding_backend")
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Store.statePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.requireActive(ctx, c); err == nil {
		t.Fatal("accepted readiness without knowing the previous backend")
	}
	rec, err := a.plan(ctx, c)
	if err != nil || !rec.Plan.Restart {
		t.Fatalf("legacy state did not plan reactivation: %+v, %v", rec.Plan, err)
	}
}

func TestRollbackUnknownStateStopsPossibleNativeExposure(t *testing.T) {
	for _, backend := range []Backend{SSH, Disabled} {
		t.Run(string(backend), func(t *testing.T) {
			a, c, f := nativeController(t)
			f.inst.Config.PortForwards = c.forwardPolicy()
			c.Backend = backend
			c.NativeForwards = nil
			a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
				if cmd.Name == "limactl" && cmd.Args[0] == "list" {
					return Result{}, errors.New("metadata unavailable")
				}
				return f.Run(ctx, cmd)
			})
			if err := a.rollback(c, Instance{}); err != nil {
				t.Fatal(err)
			}
			if countCalls(f, "limactl", "stop") != 1 || f.inst.Status != statusStopped {
				t.Fatal("uncertain cleanup did not stop possible native exposure")
			}
		})
	}
}

func TestRollbackUnknownVMStatusStopsAndClearsCredentials(t *testing.T) {
	a, c, f := testController(t)
	f.inst.Config.PortForwards = c.forwardPolicy()
	f.inst.Status = "Broken"
	f.project = c.Project
	f.payload = "dummy credentials"
	if err := a.rollback(c, f.inst); err != nil {
		t.Fatal(err)
	}
	if f.inst.Status != statusStopped || f.payload != "" {
		t.Fatal("uncertain VM status did not trigger shutdown cleanup")
	}
}

func TestWaitReadyFailurePreservesLastKnownInstance(t *testing.T) {
	a, c, f := testController(t)
	if _, err := activate(t, a, context.Background(), c, true); err != nil {
		t.Fatal(err)
	}
	a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
		if cmd.Name == "limactl" && cmd.Args[0] == "list" {
			return Result{}, errors.New("metadata unavailable")
		}
		return f.Run(ctx, cmd)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inst, err := a.switchInstance(ctx, c, Plan{}, f.inst)
	if err == nil || inst.Name != c.Instance || inst.SSHConfigFile != f.inst.SSHConfigFile {
		t.Fatalf("failed readiness lost last known instance: %+v, %v", inst, err)
	}
}

func TestActivationReadinessFailureStopsWhenDiscoveryIsUnavailable(t *testing.T) {
	a, c, f := nativeController(t)
	if _, err := activate(t, a, context.Background(), c, true); err != nil {
		t.Fatal(err)
	}
	nativePolicy := c.forwardPolicy()
	stops := countCalls(f, "limactl", "stop")
	c.Backend = SSH
	c.NativeForwards = nil
	unavailable := false
	a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
		if cmd.Name == "limactl" && cmd.Args[0] == "list" && unavailable {
			return Result{}, errors.New("metadata unavailable")
		}
		r, err := f.Run(ctx, cmd)
		if cmd.Name == "limactl" && cmd.Args[0] == "start" {
			// Model a global override restoring native allow rules at startup,
			// followed by both readiness and rollback discovery becoming unavailable.
			f.inst.Config.PortForwards = nativePolicy
			unavailable = true
		}
		return r, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := activate(t, a, ctx, c, true); err == nil {
		t.Fatal("ignored failed readiness")
	}
	if f.inst.Status != statusStopped || countCalls(f, "limactl", "stop") != stops+2 {
		t.Fatal("failed transition left possible native exposure running")
	}
	state, err := a.Store.load()
	if err != nil || state.Phase != PhaseIncomplete {
		t.Fatalf("failed transition lost incomplete state: %+v, %v", state, err)
	}
}

func TestNativeForwardReadinessPredicate(t *testing.T) {
	for _, state := range []forwardState{waitingForGuest, listenerEstablished} {
		if !(NativeForwardStatus{Configured: true, State: state}).ready() {
			t.Fatalf("expected ready state: %s", state)
		}
		if (NativeForwardStatus{State: state}).ready() {
			t.Fatal("unconfigured rule was ready")
		}
	}
	for _, state := range []forwardState{missingListener, forwardingUnavailable, "unknown"} {
		if (NativeForwardStatus{Configured: true, State: state}).ready() {
			t.Fatalf("unavailable state was ready: %s", state)
		}
	}
}

func TestRollbackUnknownStateReportsFailedStop(t *testing.T) {
	a, c, f := testController(t)
	a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
		if cmd.Name == "limactl" && cmd.Args[0] == "list" {
			return Result{}, errors.New("metadata unavailable")
		}
		if cmd.Name == "limactl" && cmd.Args[0] == "stop" {
			return Result{Code: 1, Stderr: "stop blocked"}, nil
		}
		return f.Run(ctx, cmd)
	})
	err := a.rollback(c, Instance{})
	if err == nil || !strings.Contains(err.Error(), "stop blocked") {
		t.Fatalf("uncertain cleanup hid stop failure: %v", err)
	}
}
