package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeMachine struct {
	inst          Instance
	calls         []Command
	project       string
	payload       string
	failProvision bool
	failResolver  bool
	failHelper    bool
	brokenLoader  bool
	guestPortBusy bool
	boot          string
	keys          map[string]string // guest ~/.ssh contents
}

// activate computes one reconciliation and applies it, like the CLI does.
func activate(t *testing.T, a Controller, ctx context.Context, c Config, approved bool) (State, error) {
	t.Helper()
	rec, err := a.plan(ctx, c)
	if err != nil {
		return State{}, err
	}
	return a.Activate(ctx, c, rec, approved)
}

func (f *fakeMachine) Run(_ context.Context, c Command) (Result, error) {
	f.calls = append(f.calls, c)
	if c.Name == "resolver" {
		if f.failResolver {
			return Result{Code: 1, Stderr: "SECRET=never-log"}, nil
		}
		return Result{Stdout: "TOKEN=secret\rONLY_NEW=new\r"}, nil
	}
	if c.Name == "limactl" {
		switch c.Args[0] {
		case "list":
			return Result{Stdout: jsonString(f.inst) + "\n"}, nil
		case "stop":
			f.inst.Status = "Stopped"
			f.project = ""
			return Result{}, nil
		case "start":
			f.inst.Status = "Running"
			f.project = ""
			f.boot = "new-boot"
			return Result{}, nil
		case "edit":
			expression := c.Args[3]
			mounts, forwards, ok := strings.Cut(strings.TrimPrefix(expression, ".mounts = "), " | .portForwards = ")
			if !ok {
				return Result{}, fmt.Errorf("invalid edit expression")
			}
			if err := json.Unmarshal([]byte(mounts), &f.inst.Config.Mounts); err != nil {
				return Result{}, err
			}
			if err := json.Unmarshal([]byte(forwards), &f.inst.Config.PortForwards); err != nil {
				return Result{}, err
			}
			return Result{}, nil
		}
	}
	if c.Name == "ssh" {
		remote := c.Args[len(c.Args)-1]
		switch {
		case strings.Contains(remote, "guest check"):
			if f.failHelper {
				return Result{Code: 127}, nil
			}
			return Result{Stdout: f.boot + "\n"}, nil
		case strings.Contains(remote, "guest clear"):
			f.project = ""
			f.payload = ""
			f.keys = nil
			return Result{}, nil
		case strings.Contains(remote, "guest ssh-clear"):
			f.keys = map[string]string{}
			return Result{}, nil
		case strings.Contains(remote, "guest ssh-key "):
			// remote is: /bin/bash ... -c '<helper> ssh-key '"'"'NAME'"'"''  — recover NAME.
			rest := remote[strings.Index(remote, "ssh-key ")+len("ssh-key "):]
			name := strings.Trim(strings.ReplaceAll(rest, `'"'"'`, ""), "'")
			if f.keys == nil {
				f.keys = map[string]string{}
			}
			f.keys[name] = c.Input
			return Result{}, nil
		case strings.Contains(remote, "guest motd"):
			if strings.Contains(c.Input, "secret") {
				return Result{}, fmt.Errorf("motd leaked a value")
			}
			return Result{}, nil
		case strings.Contains(remote, "guest provision"):
			if f.failProvision {
				return Result{Code: 1}, nil
			}
			f.payload = c.Input
			for _, line := range strings.Split(c.Input, "\n") {
				if strings.HasPrefix(line, "export LIMA_DEBIAN_CTL_PROJECT='") {
					f.project = strings.TrimSuffix(strings.TrimPrefix(line, "export LIMA_DEBIAN_CTL_PROJECT='"), "'")
				}
			}
			return Result{}, nil
		case strings.Contains(remote, "guest status"):
			return Result{Stdout: f.project + "\n"}, nil
		case strings.Contains(remote, "LIMA_DEBIAN_CTL_PROJECT"):
			// Login-shell loader probe: mirrors the helper unless the loader is broken.
			if f.brokenLoader {
				return Result{Stdout: "\n"}, nil
			}
			return Result{Stdout: f.project + "\n"}, nil
		case strings.Contains(remote, "/usr/bin/ss "):
			if f.guestPortBusy {
				return Result{Code: 1}, nil
			}
			return Result{}, nil
		case strings.Contains(remote, "guest exec"):
			return Result{Code: 17}, nil
		case remote == "/bin/bash --noprofile --norc -c 'true'":
			return Result{}, nil
		}
	}
	return Result{}, fmt.Errorf("unexpected fake command %s %v", c.Name, c.Args)
}
func testController(t *testing.T) (Controller, Config, *fakeMachine) {
	t.Helper()
	c := testConfig(t, "project-a")
	c.Environment.Resolver = []string{"resolver"}
	dir := filepath.Join(t.TempDir(), "instance")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte("mounts: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inst := Instance{Name: "devbox", Hostname: "lima-devbox", Status: "Running", Dir: dir, SSHConfigFile: filepath.Join(dir, "ssh.config")}
	fake := &fakeMachine{inst: inst, boot: "old-boot"}
	store := Store{Dir: t.TempDir(), Socket: filepath.Join(t.TempDir(), "socket")}
	return Controller{Runner: fake, Store: store}, c, fake
}
func countCalls(f *fakeMachine, name, first string) int {
	n := 0
	for _, c := range f.calls {
		if c.Name == name && len(c.Args) > 0 && c.Args[0] == first {
			n++
		}
	}
	return n
}
func TestActivationRequiresApprovalBeforeResolverOrMutation(t *testing.T) {
	a, c, f := testController(t)
	if _, err := activate(t, a, context.Background(), c, false); err == nil {
		t.Fatal("activation without approval succeeded")
	}
	for _, call := range f.calls {
		if call.Name != "limactl" || call.Args[0] != "list" {
			t.Fatal("performed work before approval")
		}
	}
}
func TestActivateSwitchAndRefresh(t *testing.T) {
	a, c, f := testController(t)
	ctx := context.Background()
	state, err := activate(t, a, ctx, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "ready" || state.BootID != "new-boot" {
		t.Fatal("not ready")
	}
	if countCalls(f, "limactl", "stop") != 1 || countCalls(f, "limactl", "edit") != 1 || countCalls(f, "limactl", "start") != 1 {
		t.Fatal("incorrect lifecycle")
	}
	if !strings.Contains(f.payload, "export TOKEN='secret'") {
		t.Fatal("environment not provisioned")
	}
	b, err := os.ReadFile(filepath.Join(a.Store.Dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatal("secret persisted in host state")
	}
	if _, err := activate(t, a, ctx, c, false); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "limactl", "stop") != 1 {
		t.Fatal("same-project refresh restarted VM")
	}
	c.Project = "project-b"
	c.Mounts[0].Guest = "/work/project-b"
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if f.project != "project-b" || countCalls(f, "limactl", "stop") != 2 || f.inst.Config.Mounts[0].Guest != "/work/project-b" {
		t.Fatal("project switch failed")
	}
	if err := a.Provision(ctx, c); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "limactl", "stop") != 2 {
		t.Fatal("provision restarted VM")
	}
}
func TestResolverFailureDoesNotDisruptActiveProject(t *testing.T) {
	a, c, f := testController(t)
	ctx := context.Background()
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	f.failResolver = true
	if err := a.Provision(ctx, c); err == nil {
		t.Fatal("resolver failure ignored")
	}
	if f.project != c.Project || countCalls(f, "limactl", "stop") != 1 {
		t.Fatal("resolver failure disrupted session")
	}
}
func TestProvisionFailureLeavesIncompleteState(t *testing.T) {
	a, c, f := testController(t)
	f.failProvision = true
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("failure ignored")
	}
	state, err := a.Store.load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "incomplete" || f.project != "" {
		t.Fatal("failure left ready state")
	}
}
func TestManualRestartInvalidatesReadinessAndSessionExitCode(t *testing.T) {
	a, c, f := testController(t)
	ctx := context.Background()
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	code, err := a.Session(ctx, c, []string{"some-command", "a b"}, false)
	if err != nil || code != 17 {
		t.Fatal("session exit status not preserved")
	}
	f.boot = "another-boot"
	f.project = ""
	if err := a.Provision(ctx, c); err == nil {
		t.Fatal("accepted stale boot state")
	}
}
func TestStopAndLock(t *testing.T) {
	a, c, f := testController(t)
	ctx := context.Background()
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	state, err := a.Stop(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "stopped" || f.inst.Status != "Stopped" || f.project != "" {
		t.Fatal("stop did not clear session")
	}
	unlock, err := a.Store.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := a.Store.lock(); err == nil {
		t.Fatal("concurrent mutation allowed")
	}
}
func TestPlanDoesNotExecuteResolver(t *testing.T) {
	a, c, f := testController(t)
	if _, err := a.plan(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if call.Name != "limactl" || call.Args[0] != "list" {
			t.Fatal("plan executed a mutation or resolver")
		}
	}
}

func TestLoginShellLoaderIsPartOfReadiness(t *testing.T) {
	a, c, f := testController(t)
	f.brokenLoader = true
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("activation reported ready although login shells do not load the environment")
	}
	state, err := a.Store.load()
	if err != nil || state.Phase != "incomplete" {
		t.Fatal("broken loader did not leave an incomplete phase")
	}
}

func TestGuestToHostForwardMustListen(t *testing.T) {
	a, c, f := testController(t)
	c.Forwards = []Forward{{Direction: "guest-to-host", ListenPort: 1080, TargetHost: "127.0.0.1", TargetPort: 1080}}
	f.guestPortBusy = true
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("silent -R bind failure was accepted")
	}
}

func TestStoppedVMMountReplacementRequiresApproval(t *testing.T) {
	a, c, f := testController(t)
	f.inst.Status = "Stopped"
	rec, err := a.plan(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Plan.Restart || !rec.Plan.Edit || !rec.Plan.ConfirmationRequired {
		t.Fatalf("stopped VM mount replacement must still require confirmation: %+v", rec.Plan)
	}
}

func TestSSHKeysProvisionedAndReplaced(t *testing.T) {
	a, c, f := testController(t)
	ctx := context.Background()
	keyDir := t.TempDir()
	for name, body := range map[string]string{"id_a": "PRIVATE-A", "id_a.pub": "PUBLIC-A", "id_b": "PRIVATE-B"} {
		if err := os.WriteFile(filepath.Join(keyDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.SSHKeys = []SSHKey{{Source: filepath.Join(keyDir, "id_a")}, {Source: filepath.Join(keyDir, "id_b"), Name: "deploy"}}
	if err := c.validateSSHKeys(); err != nil {
		t.Fatal(err)
	}
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if f.keys["id_a"] != "PRIVATE-A" || f.keys["id_a.pub"] != "PUBLIC-A" || f.keys["deploy"] != "PRIVATE-B" || len(f.keys) != 3 {
		t.Fatalf("keys not installed as configured: %v", f.keys)
	}
	// Dropping a key from the config removes it on the next provision.
	c.SSHKeys = c.SSHKeys[:1]
	if _, err := activate(t, a, ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if _, still := f.keys["deploy"]; still || len(f.keys) != 2 {
		t.Fatalf("removed key still present: %v", f.keys)
	}
	if _, err := a.Stop(ctx, c); err != nil {
		t.Fatal(err)
	}
	if f.keys != nil {
		t.Fatal("keys survived deactivate")
	}
}

func TestSSHKeyValidation(t *testing.T) {
	c := testConfig(t, "p")
	for _, bad := range []SSHKey{{Source: ""}, {Source: "/x", Name: "../etc"}, {Source: "/x", Name: "k.pub"}} {
		c.SSHKeys = []SSHKey{bad}
		if err := c.validateSSHKeys(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	c.SSHKeys = []SSHKey{{Source: "~/.ssh/id_x"}, {Source: "./k/id_x"}}
	if err := c.validateSSHKeys(); err == nil {
		t.Fatal("accepted duplicate target name")
	}
	c.SSHKeys = []SSHKey{{Source: "./k/id_x"}}
	if err := c.validateSSHKeys(); err != nil || c.SSHKeys[0].Source != filepath.Join(c.Root, "k/id_x") || c.SSHKeys[0].Name != "id_x" {
		t.Fatalf("relative source not resolved: %+v %v", c.SSHKeys[0], err)
	}
}
