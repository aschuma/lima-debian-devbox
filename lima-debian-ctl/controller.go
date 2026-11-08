package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"
)

const cleanupTimeout = 20 * time.Second

// Controller orchestrates one active project on a reusable Lima instance.
type Controller struct {
	Runner Runner
	Store  Store
}

func (a Controller) lima() limaClient          { return limaClient{r: a.Runner} }
func (a Controller) guest(inst Instance) guest { return newGuest(a.Runner, inst) }
func (a Controller) tunnels(inst Instance) tunnels {
	return tunnels{r: a.Runner, inst: inst, socket: a.Store.Socket}
}
func (a Controller) backupPath() string { return filepath.Join(a.Store.Dir, "lima-before-edit.yaml") }

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

// Reconciliation is one consistent observation of host and VM state. It is
// computed once and handed to Activate so prompt and action agree.
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
	inst, err := a.lima().Discover(ctx, c.Instance)
	if err != nil {
		return Reconciliation{}, err
	}
	if inst.Status != statusRunning && inst.Status != statusStopped {
		return Reconciliation{}, fmt.Errorf("instance is %s; resolve its state manually first", inst.Status)
	}
	edit := !inst.matchesProject(c)
	switching := !state.matches(c)
	restart := inst.Status == statusRunning && (edit || switching || state.Phase != PhaseReady)
	takeover := state.Project == ""
	return Reconciliation{
		Plan: Plan{
			Project: c.Project, Instance: c.Instance, VMStatus: inst.Status,
			PreviousProject: state.Project, Mounts: c.Mounts, PreviousMounts: inst.Config.Mounts,
			Forwards: c.Forwards, Restart: restart, Edit: edit, Takeover: takeover,
			// Replacing mounts is the consequential step even when the VM is stopped.
			ConfirmationRequired: restart || edit || takeover,
		},
		Instance: inst,
		State:    state,
	}, nil
}

// Activate applies rec. The caller must have obtained approval when
// rec.Plan.ConfirmationRequired. On failure the state is left "incomplete"
// and any partially installed environment or tunnels are removed.
func (a Controller) Activate(ctx context.Context, c Config, rec Reconciliation, approved bool) (state State, err error) {
	p, inst := rec.Plan, rec.Instance
	if p.ConfirmationRequired && !approved {
		return state, errors.New("activation requires approval (--yes): it replaces mounts/forwarding and may stop the running VM")
	}
	// Refuse an unprepared running VM before stopping or changing anything.
	if inst.Status == statusRunning {
		if _, err := a.guest(inst).BootID(ctx); err != nil {
			return state, fmt.Errorf("guest helpers unavailable; apply Ansible --tags project-session first: %w", err)
		}
	}
	// Resolve before disrupting the old session; values never enter host state.
	upload, err := resolveEnvironment(ctx, a.Runner, c)
	if err != nil {
		return state, err
	}
	if p.Takeover || p.Edit {
		if err := a.lima().BackupConfig(inst, a.backupPath()); err != nil {
			return state, err
		}
	}
	// The shared VM carries every installed tool and image; guard it against
	// an accidental `limactl delete` before touching it.
	if !inst.Protected {
		if err := a.lima().Protect(ctx, c.Instance); err != nil {
			return state, err
		}
		inst.Protected = true
	}
	state = State{Project: c.Project, Root: c.Root, ConfigHash: configHash(c), Phase: PhaseActivating}
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	defer func() {
		if err != nil {
			a.rollback(inst)
			state.Phase = PhaseIncomplete
			_ = a.Store.save(state)
		}
	}()

	if err = a.tunnels(inst).Stop(ctx); err != nil {
		return state, err
	}
	if inst, err = a.switchInstance(ctx, c, p, inst); err != nil {
		return state, err
	}
	if state.BootID, err = a.installProject(ctx, c, inst, upload); err != nil {
		return state, err
	}
	state.Phase = PhaseReady
	return state, a.Store.save(state)
}

// switchInstance brings the VM to a running state with the project's mounts,
// restarting only when the plan requires it.
func (a Controller) switchInstance(ctx context.Context, c Config, p Plan, inst Instance) (Instance, error) {
	lima := a.lima()
	if p.Restart {
		_ = a.guest(inst).Clear(ctx) // best effort; orderly shutdown cleanup is authoritative
		if err := lima.Stop(ctx, c.Instance); err != nil {
			return inst, err
		}
		inst.Status = statusStopped
	}
	if p.Edit {
		if err := lima.Edit(ctx, c); err != nil {
			return inst, err
		}
	}
	if inst.Status != statusRunning {
		if err := lima.Start(ctx, c.Instance); err != nil {
			return inst, err
		}
	}
	inst, err := lima.WaitReady(ctx, c.Instance)
	if err != nil {
		return inst, err
	}
	if !inst.matchesProject(c) {
		return inst, errors.New("lima mount/forwarding configuration did not match the project after activation")
	}
	return inst, nil
}

// installProject provisions the environment, opens tunnels and verifies
// both loading paths. It returns the guest boot ID that readiness is tied to.
func (a Controller) installProject(ctx context.Context, c Config, inst Instance, p payload) (string, error) {
	g := a.guest(inst)
	bootID, err := g.BootID(ctx)
	if err != nil {
		return "", fmt.Errorf("guest helpers unavailable; apply Ansible --tags project-session first: %w", err)
	}
	_ = g.PruneMounts(ctx) // cosmetic: stale mountpoint dirs from previous projects
	if err := g.Provision(ctx, p); err != nil {
		return "", err
	}
	if err := a.tunnels(inst).Start(ctx, c.Forwards); err != nil {
		return "", err
	}
	return bootID, a.verifyEnvironment(ctx, inst, c)
}

// rollback removes whatever a failed activation may have installed.
func (a Controller) rollback(inst Instance) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = a.tunnels(inst).Stop(ctx)
	if inst.Status == statusRunning {
		_ = a.guest(inst).Clear(ctx)
	}
}

// verifyEnvironment checks the helper path (used by exec/shell) and the real
// login-shell path (used by plain SSH and IDE backends).
func (a Controller) verifyEnvironment(ctx context.Context, inst Instance, c Config) error {
	g := a.guest(inst)
	if project, err := g.Project(ctx); err != nil || project != c.Project {
		return errors.New("guest environment readiness check failed")
	}
	if loaded, err := g.LoginShellProject(ctx); err != nil || loaded != c.Project {
		return errors.New("login shells do not load the project environment; re-apply Ansible --tags project-session")
	}
	return nil
}

// requireActive returns the running instance if this exact project
// configuration is ready and the VM has not been restarted since.
func (a Controller) requireActive(ctx context.Context, c Config) (Instance, error) {
	state, err := a.Store.load()
	if err != nil {
		return Instance{}, err
	}
	switch {
	case !state.matches(c):
		return Instance{}, fmt.Errorf("active project is %q, not %q; run activate here to switch", state.Project, c.Project)
	case state.ConfigHash != configHash(c):
		return Instance{}, errors.New("project configuration changed since activation; run activate")
	case state.Phase != PhaseReady:
		return Instance{}, fmt.Errorf("project is not ready (phase %q); run activate", state.Phase)
	}
	inst, err := a.lima().Discover(ctx, c.Instance)
	if err != nil {
		return inst, err
	}
	if inst.Status != statusRunning || !inst.matchesProject(c) {
		return inst, errors.New("VM state has drifted; run activate")
	}
	if boot, err := a.guest(inst).BootID(ctx); err != nil || boot != state.BootID {
		return inst, errors.New("VM was restarted or guest helpers are unavailable; run activate")
	}
	return inst, nil
}

// Provision refreshes the active project's environment without a restart.
func (a Controller) Provision(ctx context.Context, c Config) error {
	inst, err := a.requireActive(ctx, c)
	if err != nil {
		return err
	}
	p, err := resolveEnvironment(ctx, a.Runner, c)
	if err != nil {
		return err
	}
	if err := a.guest(inst).Provision(ctx, p); err != nil {
		return err
	}
	return a.verifyEnvironment(ctx, inst, c)
}

// Stop removes the project's credentials and tunnels and stops the VM.
func (a Controller) Stop(ctx context.Context, c Config) (State, error) {
	state, err := a.Store.load()
	if err != nil {
		return state, err
	}
	if !state.matches(c) {
		return state, errors.New("refusing to stop another project's or an unmanaged VM")
	}
	inst, err := a.lima().Discover(ctx, c.Instance)
	if err != nil {
		return state, err
	}
	if inst.Status != statusRunning && inst.Status != statusStopped {
		return state, fmt.Errorf("cannot stop instance in state %s", inst.Status)
	}
	state.Phase = PhaseStopping
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	if err := a.tunnels(inst).Stop(ctx); err != nil {
		return state, fmt.Errorf("%w; VM left running in phase %q: run activate or stop again", err, state.Phase)
	}
	var clearErr error
	if inst.Status == statusRunning {
		// Still stop the guest if explicit removal fails: orderly systemd cleanup
		// is the fallback, and leaving old processes running is worse than a stop.
		clearErr = a.guest(inst).Clear(ctx)
		if err := a.lima().Stop(ctx, c.Instance); err != nil {
			return state, err
		}
	}
	state.Phase = PhaseStopped
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	if clearErr != nil {
		return state, errors.New("VM stopped, but explicit credential removal failed; verify guest cleanup service")
	}
	return state, nil
}

// Status is the non-secret health summary for the selected project.
type Status struct {
	Instance        string          `json:"instance"`
	VMStatus        string          `json:"vm_status"`
	Protected       bool            `json:"protected"`
	SelectedProject string          `json:"selected_project"`
	State           State           `json:"state"`
	Ready           bool            `json:"ready"`
	TunnelsHealthy  bool            `json:"tunnels_healthy"`
	Forwards        []ForwardStatus `json:"forwards"`
	SSHAgent        SSHAgentStatus  `json:"ssh_agent"`
	MountInotify    SettingStatus   `json:"mount_inotify"`
	Hint            string          `json:"hint,omitempty"`
}

// SettingStatus compares a .devvm.toml setting with what the instance has.
type SettingStatus struct {
	Wanted  bool `json:"wanted"`
	Enabled bool `json:"enabled"`
}

// ForwardStatus is one configured tunnel and whether its listener is bound.
// For guest-to-host the listener is inside the VM (ssh -R), for host-to-guest
// it is on the host (ssh -L).
type ForwardStatus struct {
	Forward
	Listening bool `json:"listening"`
}

// SSHAgentStatus reports whether Lima's agent forwarding is configured on the
// instance and whether the guest actually has the socket this boot.
type SSHAgentStatus struct {
	Wanted    bool `json:"wanted"`    // forward_agent in .devvm.toml
	Enabled   bool `json:"enabled"`   // ssh.forwardAgent on the instance
	Available bool `json:"available"` // /run/host-services/ssh-auth.sock exists in the guest
}

func (a Controller) Status(ctx context.Context, c Config) (Status, error) {
	state, err := a.Store.load()
	if err != nil {
		return Status{}, err
	}
	inst, err := a.lima().Discover(ctx, c.Instance)
	if err != nil {
		return Status{}, err
	}
	s := Status{Instance: c.Instance, VMStatus: inst.Status, Protected: inst.Protected, SelectedProject: c.Project, State: state,
		Forwards:     make([]ForwardStatus, 0, len(c.Forwards)),
		SSHAgent:     SSHAgentStatus{Wanted: c.forwardAgent(), Enabled: inst.Config.SSH.ForwardAgent},
		MountInotify: SettingStatus{Wanted: c.mountInotify(), Enabled: inst.Config.MountInotify}}
	for _, f := range c.Forwards {
		s.Forwards = append(s.Forwards, ForwardStatus{Forward: f})
	}
	if inst.Status != statusRunning {
		s.Hint = "VM is not running; run activate"
		return s, nil
	}
	g := a.guest(inst)
	_, activeErr := a.requireActive(ctx, c)
	envErr := a.verifyEnvironment(ctx, inst, c)
	s.SSHAgent.Available = g.AgentSocketPresent(ctx)
	alive := len(c.Forwards) == 0 || a.tunnels(inst).Alive(ctx)
	s.TunnelsHealthy = alive
	for i := range s.Forwards {
		if !alive {
			break
		}
		f := s.Forwards[i].Forward
		if f.Direction == GuestToHost {
			s.Forwards[i].Listening = g.Listening(ctx, f.ListenPort)
		} else {
			s.Forwards[i].Listening = hostListening(f.ListenPort)
		}
		s.TunnelsHealthy = s.TunnelsHealthy && s.Forwards[i].Listening
	}
	s.Ready = activeErr == nil && envErr == nil && s.TunnelsHealthy
	switch {
	case activeErr != nil:
		s.Hint = activeErr.Error()
	case envErr != nil:
		s.Hint = envErr.Error() + "; run provision"
	case !alive:
		s.Hint = "project tunnels are down; run activate"
	case !s.TunnelsHealthy:
		s.Hint = "a project forward is not listening; run activate"
	case s.SSHAgent.Wanted && !s.SSHAgent.Enabled:
		s.Hint = "ssh.forwardAgent is not set on the instance; run activate"
	case s.SSHAgent.Wanted && !s.SSHAgent.Available:
		s.Hint = "SSH agent socket missing in the guest (degraded Lima start?); restart with activate"
	}
	return s, nil
}

// hostListening reports whether something accepts connections on the host
// loopback port, i.e. whether an ssh -L listener is bound.
func hostListening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Session opens the project shell or runs command in the guest and returns
// the remote exit status.
func (a Controller) Session(ctx context.Context, c Config, command []string, shell bool) (int, error) {
	inst, err := a.requireActive(ctx, c)
	if err != nil {
		return 1, err
	}
	g := a.guest(inst)
	if project, err := g.Project(ctx); err != nil || project != c.Project {
		return 1, errors.New("project environment is missing; run provision")
	}
	if len(c.Forwards) > 0 && !a.tunnels(inst).Alive(ctx) {
		return 1, errors.New("project tunnels are unavailable; run activate")
	}
	return g.Session(ctx, c, command, shell)
}
