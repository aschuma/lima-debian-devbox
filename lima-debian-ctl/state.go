package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Phase is the controller's view of the active project's lifecycle.
type Phase string

const (
	PhaseActivating Phase = "activating"
	PhaseReady      Phase = "ready"
	PhaseIncomplete Phase = "incomplete"
	PhaseStopping   Phase = "stopping"
	PhaseStopped    Phase = "stopped"
)

// State is the non-secret host-side record of the active project.
type State struct {
	Project         string  `json:"project"`
	Root            string  `json:"root"`
	ConfigHash      string  `json:"config_hash"`
	Phase           Phase   `json:"phase"`
	BootID          string  `json:"boot_id,omitempty"`
	Backend         Backend `json:"forwarding_backend,omitempty"`
	NativeTransport string  `json:"native_transport,omitempty"`
	HostAgentPID    int     `json:"host_agent_pid,omitempty"`
}

// matches reports whether the state belongs to this project checkout.
func (s State) matches(c Config) bool {
	return s.Project == c.Project && s.Root == c.Root
}

// Store persists State for one instance and owns its lock and tunnel socket.
type Store struct {
	Dir    string
	Socket string
}

// maxSocketPath keeps below OpenSSH's Unix socket path limit with headroom.
const maxSocketPath = 95

func openStore(instance string) (Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Store{}, err
	}
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		root = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(root, "lima-debian-ctl", instance)
	sockDir := filepath.Join(home, ".lima-debian-ctl", "sockets")
	for _, p := range []string{dir, sockDir} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return Store{}, err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return Store{}, err
		}
	}
	sum := sha256.Sum256([]byte(dir))
	socket := filepath.Join(sockDir, hex.EncodeToString(sum[:8]))
	if len(socket) > maxSocketPath {
		return Store{}, errors.New("home path too long for OpenSSH control socket")
	}
	return Store{Dir: dir, Socket: socket}, nil
}

// lock takes the per-instance mutation lock without blocking.
func (s Store) lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another command is changing this instance")
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (s Store) statePath() string { return filepath.Join(s.Dir, "state.json") }

func (s Store) load() (State, error) {
	var state State
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("corrupt state file %s: %w", s.statePath(), err)
	}
	return state, nil
}

func (s Store) save(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.statePath(), append(data, '\n'))
}

// atomicWrite replaces dest via a 0600 temp file and rename.
func atomicWrite(dest string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}

func configHash(c Config) string {
	sum := sha256.Sum256([]byte(jsonString(c)))
	return hex.EncodeToString(sum[:])
}
