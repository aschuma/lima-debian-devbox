package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Controller orchestrates one active project on a reusable Lima instance.
type Controller struct {
	Runner Runner
	Store  Store
}

// Plan is the non-secret, user-facing summary of what activation would do.
type Plan struct {
	Project              string    `json:"project"`
	Instance             string    `json:"instance"`
	VMStatus             string    `json:"vm_status"`
	PreviousProject      string    `json:"previous_project,omitempty"`
	Mounts               []Mount   `json:"mounts"`
	PreviousMounts       []Mount   `json:"previous_mounts"`
	Forwards             []Forward `json:"forwards"`
	Restart              bool      `json:"restart"`
	Edit                 bool      `json:"edit"`
	Takeover             bool      `json:"takeover"`
	ConfirmationRequired bool      `json:"confirmation_required"`
}

// Reconciliation is one consistent observation of host state and VM state,
// computed once and then handed to Activate so both agree on what was seen.
type Reconciliation struct {
	Plan     Plan
	Instance Instance
	State    State
}

func (a Controller) plan(ctx context.Context, c Config) (Reconciliation, error) {
	state, err := a.Store.load()
	if err != nil {
		return Reconciliation{}, err
	}
	inst, err := discover(ctx, a.Runner, c.Instance)
	if err != nil {
		return Reconciliation{}, err
	}
	if inst.Status != "Running" && inst.Status != "Stopped" {
		return Reconciliation{}, fmt.Errorf("instance is %s; resolve its state manually first", inst.Status)
	}
	edit := !mountsEqual(inst.Config.Mounts, c.Mounts) || !hasManagedForwardPolicy(inst)
	switching := state.Project != c.Project || state.Root != c.Root
	restart := inst.Status == "Running" && (edit || switching || state.Phase != "ready")
	takeover := state.Project == ""
	p := Plan{
		Project: c.Project, Instance: c.Instance, VMStatus: inst.Status,
		PreviousProject: state.Project, Mounts: c.Mounts, PreviousMounts: inst.Config.Mounts,
		Forwards: c.Forwards, Restart: restart, Edit: edit, Takeover: takeover,
		// Replacing mounts is the consequential step even when the VM is stopped.
		ConfirmationRequired: restart || edit || takeover,
	}
	return Reconciliation{Plan: p, Instance: inst, State: state}, nil
}

// Activate applies a previously computed reconciliation. The caller is
// responsible for having obtained approval when rec.Plan.ConfirmationRequired.
func (a Controller) Activate(ctx context.Context, c Config, rec Reconciliation, approved bool) (state State, err error) {
	p, inst := rec.Plan, rec.Instance
	if p.ConfirmationRequired && !approved {
		return state, fmt.Errorf("activation requires approval (--yes): it replaces mounts/forwarding and may stop the running VM")
	}
	// Reject an unprepared running VM before stopping or changing anything.
	if inst.Status == "Running" {
		if _, err := requireGuestHelpers(ctx, a.Runner, inst); err != nil {
			return state, fmt.Errorf("guest helpers unavailable; apply Ansible --tags project-session first: %w", err)
		}
	}
	// Resolve before disrupting the old session; values never enter host state.
	payload, err := resolveEnvironment(ctx, a.Runner, c)
	if err != nil {
		return state, err
	}
	if p.Takeover || p.Edit {
		if err := backupConfig(inst, filepath.Join(a.Store.Dir, "lima-before-edit.yaml")); err != nil {
			return state, err
		}
	}
	state = State{Project: c.Project, Root: c.Root, ConfigHash: configHash(c), Phase: "activating"}
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = stopTunnels(cleanup, a.Runner, inst, a.Store.Socket)
		if inst.Status == "Running" {
			_ = clearGuest(cleanup, a.Runner, inst)
		}
		state.Phase = "incomplete"
		_ = a.Store.save(state)
	}()
	if err = stopTunnels(ctx, a.Runner, inst, a.Store.Socket); err != nil {
		return state, err
	}
	if p.Restart {
		// Best effort: orderly shutdown cleanup remains authoritative.
		_ = clearGuest(ctx, a.Runner, inst)
		if _, err = checked(ctx, a.Runner, Command{Name: "limactl", Args: []string{"stop", "--tty=false", c.Instance}}); err != nil {
			return state, err
		}
		inst.Status = "Stopped"
	}
	if p.Edit {
		if err = editInstance(ctx, a.Runner, c); err != nil {
			return state, err
		}
	}
	if inst.Status != "Running" {
		if _, err = checked(ctx, a.Runner, Command{Name: "limactl", Args: []string{"start", "--tty=false", c.Instance}}); err != nil {
			return state, err
		}
	}
	inst, err = waitInstance(ctx, a.Runner, c.Instance)
	if err != nil {
		return state, err
	}
	if !mountsEqual(inst.Config.Mounts, c.Mounts) || !hasManagedForwardPolicy(inst) {
		return state, fmt.Errorf("lima mount/forwarding configuration did not match the project after activation")
	}
	boot, helperErr := requireGuestHelpers(ctx, a.Runner, inst)
	if helperErr != nil {
		return state, fmt.Errorf("guest helpers unavailable; apply Ansible --tags project-session first: %w", helperErr)
	}
	state.BootID = strings.TrimSpace(boot)
	// Cosmetic, best effort: stale mountpoint directories from previous projects.
	_ = pruneMounts(ctx, a.Runner, inst)
	if err = provisionGuest(ctx, a.Runner, inst, payload); err != nil {
		return state, err
	}
	if err = startTunnels(ctx, a.Runner, inst, c, a.Store.Socket); err != nil {
		return state, err
	}
	if err = a.verifyEnvironment(ctx, inst, c); err != nil {
		return state, err
	}
	state.Phase = "ready"
	err = a.Store.save(state)
	return state, err
}

// verifyEnvironment checks both the helper path (used by exec/shell) and the
// real login-shell loading path (used by plain SSH and IDE backends).
func (a Controller) verifyEnvironment(ctx context.Context, inst Instance, c Config) error {
	project, err := guestProject(ctx, a.Runner, inst)
	if err != nil || project != c.Project {
		return fmt.Errorf("guest environment readiness check failed")
	}
	loaded, err := loaderProbe(ctx, a.Runner, inst)
	if err != nil || loaded != c.Project {
		return fmt.Errorf("login shells do not load the project environment; re-apply Ansible --tags project-session")
	}
	return nil
}

func (a Controller) requireActive(ctx context.Context, c Config) (Instance, State, error) {
	state, err := a.Store.load()
	if err != nil {
		return Instance{}, state, err
	}
	switch {
	case state.Project != c.Project || state.Root != c.Root:
		return Instance{}, state, fmt.Errorf("active project is %q, not %q; run activate here to switch", state.Project, c.Project)
	case state.ConfigHash != configHash(c):
		return Instance{}, state, fmt.Errorf("project configuration changed since activation; run activate")
	case state.Phase != "ready":
		return Instance{}, state, fmt.Errorf("project is not ready (phase %q); run activate", state.Phase)
	}
	inst, err := discover(ctx, a.Runner, c.Instance)
	if err != nil {
		return inst, state, err
	}
	if inst.Status != "Running" || !mountsEqual(inst.Config.Mounts, c.Mounts) || !hasManagedForwardPolicy(inst) {
		return inst, state, fmt.Errorf("VM state has drifted; run activate")
	}
	boot, err := requireGuestHelpers(ctx, a.Runner, inst)
	if err != nil || strings.TrimSpace(boot) != state.BootID {
		return inst, state, fmt.Errorf("VM was restarted or guest helpers are unavailable; run activate")
	}
	return inst, state, nil
}

func (a Controller) Provision(ctx context.Context, c Config) error {
	inst, _, err := a.requireActive(ctx, c)
	if err != nil {
		return err
	}
	payload, err := resolveEnvironment(ctx, a.Runner, c)
	if err != nil {
		return err
	}
	if err := provisionGuest(ctx, a.Runner, inst, payload); err != nil {
		return err
	}
	return a.verifyEnvironment(ctx, inst, c)
}

func (a Controller) Stop(ctx context.Context, c Config) (State, error) {
	state, err := a.Store.load()
	if err != nil {
		return state, err
	}
	if state.Project != c.Project || state.Root != c.Root {
		return state, fmt.Errorf("refusing to stop another project's or an unmanaged VM")
	}
	inst, err := discover(ctx, a.Runner, c.Instance)
	if err != nil {
		return state, err
	}
	if inst.Status != "Running" && inst.Status != "Stopped" {
		return state, fmt.Errorf("cannot stop instance in state %s", inst.Status)
	}
	state.Phase = "stopping"
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	if err := stopTunnels(ctx, a.Runner, inst, a.Store.Socket); err != nil {
		return state, fmt.Errorf("%w; VM left running, phase is %q: run activate or stop again", err, state.Phase)
	}
	var clearErr error
	if inst.Status == "Running" {
		// Still stop the guest if explicit removal fails: orderly systemd cleanup is
		// the fallback, and leaving old processes running is worse than a stop.
		clearErr = clearGuest(ctx, a.Runner, inst)
		if _, err := checked(ctx, a.Runner, Command{Name: "limactl", Args: []string{"stop", "--tty=false", c.Instance}}); err != nil {
			return state, err
		}
	}
	state.Phase = "stopped"
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	if clearErr != nil {
		return state, fmt.Errorf("VM stopped, but explicit credential removal failed; verify guest cleanup service")
	}
	return state, nil
}

type Status struct {
	Instance        string `json:"instance"`
	VMStatus        string `json:"vm_status"`
	SelectedProject string `json:"selected_project"`
	State           State  `json:"state"`
	Ready           bool   `json:"ready"`
	TunnelsHealthy  bool   `json:"tunnels_healthy"`
	Hint            string `json:"hint,omitempty"`
}

func (a Controller) Status(ctx context.Context, c Config) (Status, error) {
	state, err := a.Store.load()
	if err != nil {
		return Status{}, err
	}
	inst, err := discover(ctx, a.Runner, c.Instance)
	if err != nil {
		return Status{}, err
	}
	status := Status{Instance: c.Instance, VMStatus: inst.Status, SelectedProject: c.Project, State: state}
	if inst.Status != "Running" {
		status.Hint = "VM is not running; run activate"
		return status, nil
	}
	_, _, activeErr := a.requireActive(ctx, c)
	envErr := a.verifyEnvironment(ctx, inst, c)
	status.TunnelsHealthy = len(c.Forwards) == 0 || tunnelCheck(ctx, a.Runner, inst, a.Store.Socket)
	status.Ready = activeErr == nil && envErr == nil && status.TunnelsHealthy
	switch {
	case activeErr != nil:
		status.Hint = activeErr.Error()
	case envErr != nil:
		status.Hint = envErr.Error() + "; run provision"
	case !status.TunnelsHealthy:
		status.Hint = "project tunnels are down; run activate"
	}
	return status, nil
}

// Session runs the project shell or a command inside the guest with the
// project environment, preserving the remote exit status.
func (a Controller) Session(ctx context.Context, c Config, command []string, shell bool) (int, error) {
	inst, _, err := a.requireActive(ctx, c)
	if err != nil {
		return 1, err
	}
	project, err := guestProject(ctx, a.Runner, inst)
	if err != nil || project != c.Project {
		return 1, fmt.Errorf("project environment is missing; run provision")
	}
	if len(c.Forwards) > 0 && !tunnelCheck(ctx, a.Runner, inst, a.Store.Socket) {
		return 1, fmt.Errorf("project tunnels are unavailable; run activate")
	}
	// Helper accepts a quoted argv vector; no command string interpolation.
	remote := "/usr/local/bin/lima-debian-ctl-guest exec " + shellQuote(c.Project) + " " + shellQuote(c.Mounts[0].Guest)
	if shell {
		remote += " --shell"
	} else {
		remote += " --"
		for _, arg := range command {
			remote += " " + shellQuote(arg)
		}
	}
	args := sshArgs(inst)
	// A TTY lets Ctrl-C reach the remote command instead of only killing ssh.
	if shell || stdinIsTerminal() {
		args = append(args, "-t")
	}
	args = append(args, inst.Hostname, remote)
	result, err := a.Runner.Run(ctx, Command{Name: "ssh", Args: args, Interactive: true})
	return result.Code, err
}
