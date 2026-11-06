package main

import (
	"context"
	"encoding/json"
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
	tn := tunnels{r: runner, inst: f.inst, socket: a.Store.Socket}
	if err := tn.Start(context.Background(), c.Forwards); err != nil {
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
	if err := tn.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Store.Socket); !os.IsNotExist(err) {
		t.Fatal("socket not removed")
	}
}
func TestForwardPolicyRecognizesExpandedLimaDefaults(t *testing.T) {
	// Exactly what Lima 2.2.1 reports after filling defaults for the managed rule.
	expanded := `{"guestIPMustBeZero":false,"guestIP":"0.0.0.0","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true}`
	var rule portForward
	if err := json.Unmarshal([]byte(expanded), &rule); err != nil {
		t.Fatal(err)
	}
	if !rule.isIgnoreAll() {
		t.Fatal("expanded defaults caused false config drift")
	}
	yes := true
	rule.GuestIPMustBeZero = &yes
	if rule.isIgnoreAll() {
		t.Fatal("rule that only ignores 0.0.0.0-bound ports was accepted")
	}
	var unset portForward
	if err := json.Unmarshal([]byte(`{"guestIP":"0.0.0.0","proto":"any","ignore":true}`), &unset); err != nil {
		t.Fatal(err)
	}
	if !unset.isIgnoreAll() {
		t.Fatal("rule as written in lima.yaml not recognised")
	}
}
