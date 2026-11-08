package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	Project              string        `json:"project"`
	Instance             string        `json:"instance"`
	VMStatus             string        `json:"vm_status"`
	PreviousProject      string        `json:"previous_project,omitempty"`
	Mounts               []Mount       `json:"mounts"`
	PreviousMounts       []Mount       `json:"previous_mounts"`
	Forwards             []Forward     `json:"forwards"`
	Restart              bool          `json:"restart"`
	Edit                 bool          `json:"edit"`
	Takeover             bool          `json:"takeover"`
	ConfirmationRequired bool          `json:"confirmation_required"`
	ForwardingBackend    Backend       `json:"forwarding_backend"`
	NativeForwards       []portForward `json:"native_forwards"`
	NativePolicy         []portForward `json:"native_policy"`
	Reasons              []string      `json:"reasons"`
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
	if err := validateInstanceForwards(c, inst); err != nil {
		return Reconciliation{}, err
	}
	backendChanged := state.Backend != c.Backend
	nativeChanged := !inst.matchesProject(c)
	edit := nativeChanged || backendChanged
	switching := !state.matches(c)
	restart := inst.Status == statusRunning && (edit || switching || state.Phase != PhaseReady)
	reasons := make([]string, 0)
	if nativeChanged {
		reasons = append(reasons, "native configuration or forwarding policy changed")
	}
	if backendChanged {
		reasons = append(reasons, "local forwarding backend changed or was not recorded")
	}
	if switching {
		reasons = append(reasons, "project switch or initial takeover")
	}
	if state.Phase != PhaseReady {
		reasons = append(reasons, "previous activation is not ready")
	}
	if c.Backend == Native && inst.Status == statusRunning {
		pid, pidErr := hostAgentPID(inst.Dir)
		transportMatches := state.NativeTransport == "grpc" && state.Phase == PhaseReady && pidErr == nil && pid == state.HostAgentPID
		if transportMatches {
			boot, err := a.guest(inst).BootID(ctx)
			transportMatches = err == nil && boot == state.BootID
		}
		if !transportMatches {
			restart = true
			reasons = append(reasons, "restart required to establish native gRPC transport for this boot")
		}
	}
	takeover := state.Project == ""
	return Reconciliation{
		Plan: Plan{
			Project: c.Project, Instance: c.Instance, VMStatus: inst.Status,
			PreviousProject: state.Project, Mounts: c.Mounts, PreviousMounts: inst.Config.Mounts,
			Forwards: c.Forwards, Restart: restart, Edit: edit, Takeover: takeover,
			ForwardingBackend: c.Backend, NativeForwards: c.NativeForwards, NativePolicy: c.forwardPolicy(), Reasons: reasons,
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
	if c.Backend == Native && len(c.NativeForwards) > 0 {
		if _, err := run(ctx, a.Runner, Command{Name: "lsof", Args: []string{"-v"}}); err != nil {
			return state, fmt.Errorf("native forwarding requires host lsof: %w", err)
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
	state = State{Project: c.Project, Root: c.Root, ConfigHash: configHash(c), Backend: c.Backend, Phase: PhaseActivating}
	if c.Backend == Native {
		state.NativeTransport = "grpc"
	}
	if err := a.Store.save(state); err != nil {
		return state, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, a.rollback(c, inst))
			state.Phase = PhaseIncomplete
			err = errors.Join(err, a.Store.save(state))
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
	if c.Backend == Native {
		state.HostAgentPID, err = hostAgentPID(inst.Dir)
		if err != nil {
			return state, err
		}
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
		if err := lima.StartProject(ctx, c); err != nil {
			return inst, err
		}
		inst.Status = statusRunning
	}
	current, err := lima.WaitReady(ctx, c.Instance)
	if err != nil {
		return inst, err
	}
	if !current.matchesProject(c) {
		return current, errors.New("lima mount/forwarding configuration did not match the project after activation")
	}
	return current, nil
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
	if err := (forwarding{runner: a.Runner, store: a.Store, inst: inst}).Apply(ctx, c); err != nil {
		return "", err
	}
	return bootID, a.verifyEnvironment(ctx, inst, c)
}

// rollback removes whatever a failed activation may have installed.
func (a Controller) rollback(c Config, inst Instance) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	current, discoverErr := a.lima().Discover(ctx, c.Instance)
	if discoverErr == nil {
		inst = current
	}
	tunnelErr := a.tunnels(inst).Stop(ctx)
	uncertain := discoverErr != nil || inst.Status != statusRunning && inst.Status != statusStopped
	var clearErr error
	// If discovery failed or the VM status is unknown, prioritize stopping over
	// another slow SSH attempt. Shutdown provides authoritative credential cleanup.
	if !uncertain && inst.Status == statusRunning {
		clearErr = a.guest(inst).Clear(ctx)
	}
	mustStop := uncertain || c.Backend == Native || hasNativeForwards(inst.Config.PortForwards)
	if !mustStop {
		return errors.Join(tunnelErr, clearErr)
	}
	if inst.Status != statusStopped || uncertain {
		if err := a.lima().Stop(ctx, c.Instance); err != nil {
			return errors.Join(tunnelErr, clearErr, discoverErr, fmt.Errorf("cleanup could not stop VM forwarding: %w", err))
		}
	}
	// A successful stop resolves discovery uncertainty and provides the
	// shutdown fallback for any failed explicit credential removal.
	return tunnelErr
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
	case state.Backend != c.Backend:
		return Instance{}, errors.New("forwarding backend changed or was not recorded; run activate")
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
	if c.Backend == Native {
		pid, err := hostAgentPID(inst.Dir)
		if state.NativeTransport != "grpc" || err != nil || pid != state.HostAgentPID {
			return inst, errors.New("native transport is not ready or host agent restarted; run activate")
		}
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
	// LimaContainerdDisabled is true when Lima's own containerd (system and
	// rootless) is off, as the controller enforces; the guest uses Docker CE.
	LimaContainerdDisabled bool                  `json:"lima_containerd_disabled"`
	Hint                   string                `json:"hint,omitempty"`
	ForwardingBackend      Backend               `json:"forwarding_backend"`
	NativeForwards         []NativeForwardStatus `json:"native_forwards"`
}

// SettingStatus compares a .devvm.yaml setting with what the instance has.
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
	Wanted    bool `json:"wanted"`    // lima.ssh.forwardAgent in .devvm.yaml
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
	s := Status{Instance: c.Instance, VMStatus: inst.Status, Protected: inst.Protected, SelectedProject: c.Project, State: state, ForwardingBackend: c.Backend,
		NativeForwards:         make([]NativeForwardStatus, 0),
		Forwards:               make([]ForwardStatus, 0, len(c.Forwards)),
		SSHAgent:               SSHAgentStatus{Wanted: c.forwardAgent(), Enabled: inst.Config.SSH.ForwardAgent},
		MountInotify:           SettingStatus{Wanted: c.mountInotify(), Enabled: inst.Config.MountInotify},
		LimaContainerdDisabled: inst.Config.Containerd.disabled()}
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
	native, nativeErr := (forwarding{runner: a.Runner, store: a.Store, inst: inst}).Check(ctx, c)
	s.NativeForwards = native
	for _, f := range native {
		if !f.ready() {
			nativeErr = errors.New("native forwarding listener is not established; run activate")
			break
		}
	}
	s.Ready = activeErr == nil && envErr == nil && s.TunnelsHealthy && nativeErr == nil
	switch {
	case activeErr != nil:
		s.Hint = activeErr.Error()
	case envErr != nil:
		s.Hint = envErr.Error() + "; run provision"
	case nativeErr != nil:
		s.Hint = nativeErr.Error()
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
	return tcpListening("127.0.0.1", port)
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
