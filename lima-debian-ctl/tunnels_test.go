package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestTunnelsUseDedicatedSocketAndLoopback(t *testing.T) {
	a, c, f := testController(t)
	c.Forwards = []Forward{{Direction: "guest-to-host", ListenPort: 1080, TargetHost: "127.0.0.1", TargetPort: 1080}, {Direction: "host-to-guest", ListenPort: 8080, TargetHost: "::1", TargetPort: 8080}}
	var calls []Command
	runner := runnerFunc(func(_ context.Context, call Command) (Result, error) {
		calls = append(calls, call)
		if strings.Contains(strings.Join(call.Args, " "), "-O exit") {
			_ = os.Remove(a.Store.Socket)
		}
		return Result{}, nil
	})
	if err := startTunnels(context.Background(), runner, f.inst, c, a.Store.Socket); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("expected tunnel start, master check and -R listener check, got %d calls", len(calls))
	}
	if !strings.Contains(strings.Join(calls[2].Args, " "), "sport = :1080") {
		t.Fatal("guest-to-host listener was not checked on the guest")
	}
	start := strings.Join(calls[0].Args, " ")
	for _, want := range []string{"-S " + a.Store.Socket, "ExitOnForwardFailure=yes", "-R 127.0.0.1:1080:127.0.0.1:1080", "-L 127.0.0.1:8080:[::1]:8080"} {
		if !strings.Contains(start, want) {
			t.Fatalf("missing tunnel option %s", want)
		}
	}
	if !calls[0].Quiet {
		t.Fatal("daemonized ssh inherits capture pipes")
	}
	if err := os.WriteFile(a.Store.Socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := stopTunnels(context.Background(), runner, f.inst, a.Store.Socket); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Store.Socket); !os.IsNotExist(err) {
		t.Fatal("socket not removed")
	}
}
func TestForwardPolicyRecognizesExpandedLimaDefaults(t *testing.T) {
	var inst Instance
	inst.Config.PortForwards = []map[string]any{{"guestIP": "0.0.0.0", "ignore": true, "proto": "any", "guestPortRange": []any{float64(1), float64(65535)}, "hostIP": "127.0.0.1", "hostPortRange": []any{float64(1), float64(65535)}}}
	if !hasManagedForwardPolicy(inst) {
		t.Fatal("expanded defaults caused false config drift")
	}
	inst.Config.PortForwards[0]["guestIPMustBeZero"] = true
	if hasManagedForwardPolicy(inst) {
		t.Fatal("partial ignore rule accepted")
	}
}
