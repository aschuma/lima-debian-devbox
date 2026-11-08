package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeController(t *testing.T) (Controller, Config, *fakeMachine) {
	t.Helper()
	a, c, f := testController(t)
	c.Backend = Native
	c.NativeForwards = []portForward{{GuestPort: 3000, HostPort: 8080}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.inst.Dir, "ha.pid"), []byte("6000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return a, c, f
}

func TestNativeDefaultsRangesAndPolicy(t *testing.T) {
	c := testConfig(t, "project")
	c.Backend = Native
	c.NativeForwards = []portForward{
		{GuestPort: 3000, HostPort: 8080},
		{GuestPortRange: []int{4000, 4999}, HostPortRange: []int{14000, 14999}, Proto: "any"},
		{GuestPort: 5353, HostPort: 15353, Proto: "udp"},
		{GuestPort: 443, HostPort: 8443, Proto: "tcp", Static: true},
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	p := c.NativeForwards[0]
	if p.HostIP != "127.0.0.1" || p.GuestIP != "127.0.0.1" || p.Proto != "any" || p.Static || *p.GuestIPMustBeZero {
		t.Fatalf("incorrect native defaults: %+v", p)
	}
	rules := c.forwardPolicy()
	if len(rules) != 5 || !rules[4].isIgnoreAll() {
		t.Fatal("missing trailing deny-all policy")
	}
	if !forwardPoliciesEqual(rules, c.forwardPolicy()) {
		t.Fatal("native policy falsely drifted")
	}
	rules[0].HostPort = 8081
	if forwardPoliciesEqual(rules, c.forwardPolicy()) {
		t.Fatal("changed listener was not detected")
	}
}

func TestNativeRejectsUnsupportedRules(t *testing.T) {
	tests := map[string]portForward{
		"udp-static":      {GuestPort: 3000, HostPort: 8080, Proto: "udp", Static: true},
		"any-static":      {GuestPort: 3000, HostPort: 8080, Proto: "any", Static: true},
		"range-static":    {GuestPortRange: []int{3000, 3001}, HostPortRange: []int{8080, 8081}, Proto: "tcp", Static: true},
		"unequal-range":   {GuestPortRange: []int{3000, 3002}, HostPortRange: []int{8080, 8081}},
		"range-length":    {GuestPortRange: []int{3000}, HostPortRange: []int{8080}},
		"privileged-host": {GuestPort: 80, HostPort: 80},
		"lan":             {GuestPort: 3000, HostPort: 8080, HostIP: "0.0.0.0"},
		"dns":             {GuestPort: 3000, HostPort: 8080, GuestIP: "localhost"},
		"socket":          {GuestSocket: "/run/docker.sock", HostPort: 8080},
		"ignore":          {GuestPort: 3000, HostPort: 8080, Ignore: true},
		"ssh":             {GuestPort: 22, HostPort: 8080},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			c := testConfig(t, "project")
			c.Backend = Native
			c.NativeForwards = []portForward{p}
			if err := c.validate(); err == nil {
				t.Fatal("accepted invalid native rule")
			}
		})
	}
}

func TestNativeOverlappingMappings(t *testing.T) {
	c := testConfig(t, "project")
	c.Backend = Native
	c.NativeForwards = []portForward{{GuestPort: 3000, HostPort: 8080}, {GuestPort: 3001, HostPort: 8080, Proto: "any"}}
	if err := c.validate(); err == nil {
		t.Fatal("accepted overlapping native host mappings")
	}
}

func TestNativePlanDoesNotRequireGuestProvisioning(t *testing.T) {
	a, c, f := nativeController(t)
	f.failHelper = true
	rec, err := a.plan(context.Background(), c)
	if err != nil || !rec.Plan.Restart {
		t.Fatalf("could not inspect unprepared native takeover: %+v, %v", rec.Plan, err)
	}
	for _, call := range f.calls {
		if call.Name != "limactl" {
			t.Fatal("native takeover plan executed guest commands or resolver")
		}
	}
}

func TestNativeWaitingIsReadyAndTransportIsExplicit(t *testing.T) {
	a, c, f := nativeController(t)
	ctx := context.Background()
	state, err := activate(t, a, ctx, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if state.NativeTransport != "grpc" || state.HostAgentPID != 6000 {
		t.Fatalf("transport not recorded: %+v", state)
	}
	for _, call := range f.calls {
		if call.Name == "limactl" && call.Args[0] == "start" && strings.Join(call.Env, " ") != "LIMA_SSH_PORT_FORWARDER=false" {
			t.Fatal("native start inherited host transport")
		}
	}
	s, err := a.Status(ctx, c)
	if err != nil || !s.Ready || len(s.NativeForwards) != 1 || s.NativeForwards[0].State != "waiting for guest listener" {
		t.Fatalf("dynamic waiting status: %+v, %v", s, err)
	}
	rec, err := a.plan(ctx, c)
	if err != nil || rec.Plan.Restart {
		t.Fatalf("matching native project restarts: %+v, %v", rec.Plan, err)
	}
	if err := os.WriteFile(filepath.Join(f.inst.Dir, "ha.pid"), []byte("6002\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rec, err = a.plan(ctx, c)
	if err != nil || !rec.Plan.Restart {
		t.Fatal("host-agent restart did not invalidate native transport")
	}
}

func TestNativeUDPAndRangeStatusUsesOwnedListeners(t *testing.T) {
	a, c, f := nativeController(t)
	c.NativeForwards = []portForward{{GuestPort: 5353, HostPort: 15353, Proto: "udp"}, {GuestPortRange: []int{3000, 3001}, HostPortRange: []int{8080, 8081}}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	f.nativeGuestListeners = "udp UNCONN 0 0 127.0.0.1:5353 0.0.0.0:*\ntcp LISTEN 0 128 127.0.0.1:3001 0.0.0.0:*\n"
	f.nativeHostListeners = "p6000\nf10\nPUDP\nn127.0.0.1:15353\nf11\nPTCP\nn127.0.0.1:8081\nTST=LISTEN\n"
	if _, err := activate(t, a, context.Background(), c, true); err != nil {
		t.Fatal(err)
	}
	s, err := a.Status(context.Background(), c)
	if err != nil || !s.Ready || !s.NativeForwards[0].Listening || !s.NativeForwards[1].Listening {
		t.Fatalf("native owned listeners: %+v, %v", s, err)
	}
	f.nativeHostListeners = ""
	s, err = a.Status(context.Background(), c)
	if err != nil || s.Ready || !strings.Contains(s.Hint, "not established") {
		t.Fatalf("native missing listener was accepted: %+v, %v", s, err)
	}
}

func TestNativeActivationFailureStopsExposure(t *testing.T) {
	a, c, f := nativeController(t)
	f.failProvision = true
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("ignored native failure")
	}
	state, err := a.Store.load()
	if err != nil || state.Phase != PhaseIncomplete || f.inst.Status != statusStopped {
		t.Fatalf("native cleanup: %+v, %s, %v", state, f.inst.Status, err)
	}
}

func TestNativeBindFailureIsNotSuccessfulStart(t *testing.T) {
	a, c, f := nativeController(t)
	c.NativeForwards[0].Static = true
	c.NativeForwards[0].Proto = "tcp"
	f.nativeHostListeners = "" // Neither host agent nor Lima SSH owns the listener.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := activate(t, a, ctx, c, true); err == nil {
		t.Fatal("accepted static bind failure")
	}
	if f.inst.Status != statusStopped {
		t.Fatal("failed native activation left exposure running")
	}
}

func TestBackendSwitchesAndCustomOnlyRefresh(t *testing.T) {
	a, c, f := nativeController(t)
	ctx := context.Background()
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	c.Backend = SSH
	c.NativeForwards = nil
	c.Forwards = []Forward{{Direction: HostToGuest, ListenPort: 8080, TargetHost: "127.0.0.1", TargetPort: 3000}}
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	stops := countCalls(f, "limactl", "stop")
	c.Forwards[0].ListenPort = 8081
	if _, err := activate(t, a, ctx, c, false); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "limactl", "stop") != stops {
		t.Fatal("custom-only forwarding refresh restarted VM")
	}
	c.Backend = Native
	c.Forwards = nil
	c.NativeForwards = []portForward{{GuestPort: 3000, HostPort: 8080}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "limactl", "stop") != stops+1 {
		t.Fatal("switch to native did not restart")
	}
}

func TestLimaSSHListenerCannotCountAsNativeForward(t *testing.T) {
	a, c, f := nativeController(t)
	f.inst.SSHLocalPort = 8080
	c.NativeForwards[0].Static = true
	c.NativeForwards[0].Proto = "tcp"
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("accepted collision with Lima's own SSH listener")
	}
	for _, call := range f.calls {
		if call.Name != "limactl" || (call.Args[0] != "list" && call.Args[0] != "--version") {
			t.Fatal("reserved-port rejection performed work")
		}
	}
}

func TestMissingLsofDoesNotDisruptProject(t *testing.T) {
	a, c, f := nativeController(t)
	a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
		if cmd.Name == "lsof" {
			return Result{Code: 127}, nil
		}
		return f.Run(ctx, cmd)
	})
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("ignored missing lsof")
	}
	if countCalls(f, "limactl", "stop") != 0 || countCalls(f, "limactl", "protect") != 0 {
		t.Fatal("missing lsof disrupted project")
	}
	for _, call := range f.calls {
		if call.Name == "resolver" {
			t.Fatal("ran resolver without native prerequisites")
		}
	}
}

func TestNativeCleanupFailureIsReported(t *testing.T) {
	a, c, f := nativeController(t)
	f.failProvision = true
	stops := 0
	a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
		if cmd.Name == "limactl" && cmd.Args[0] == "stop" {
			stops++
			if stops == 2 {
				return Result{Code: 1, Stderr: "cleanup stop blocked"}, nil
			}
		}
		return f.Run(ctx, cmd)
	})
	_, err := activate(t, a, context.Background(), c, true)
	if err == nil || !strings.Contains(err.Error(), "cleanup could not stop VM forwarding") {
		t.Fatalf("cleanup failure not reported: %v", err)
	}
	state, err := a.Store.load()
	if err != nil || state.Phase != PhaseIncomplete {
		t.Fatalf("cleanup failure lost incomplete state: %+v, %v", state, err)
	}
}

func TestLimaVersionGateBeforeMutation(t *testing.T) {
	a, c, f := testController(t)
	a.Runner = runnerFunc(func(ctx context.Context, cmd Command) (Result, error) {
		if cmd.Name == "limactl" && cmd.Args[0] == "--version" {
			return Result{Stdout: "limactl version 2.2.1"}, nil
		}
		return f.Run(ctx, cmd)
	})
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("accepted unsupported Lima")
	}
	if len(f.calls) != 0 {
		t.Fatal("performed work after version rejection")
	}
}
