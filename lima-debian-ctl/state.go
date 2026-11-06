package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type State struct {
	Project    string `json:"project"`
	Root       string `json:"root"`
	ConfigHash string `json:"config_hash"`
	Phase      string `json:"phase"`
	BootID     string `json:"boot_id,omitempty"`
}
type Store struct {
	Dir    string
	Socket string
}

func openStore(instance string) (Store, error) {
	root := os.Getenv("XDG_STATE_HOME")
	home, err := os.UserHomeDir()
	if err != nil {
		return Store{}, err
	}
	if root == "" {
		root = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(root, "lima-debian-ctl", instance)
	sockDir := filepath.Join(home, ".lima-debian-ctl", "sockets")
	for _, p := range []string{dir, sockDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return Store{}, err
		}
		if err := os.Chmod(p, 0700); err != nil {
			return Store{}, err
		}
	}
	sum := sha256.Sum256([]byte(dir))
	socket := filepath.Join(sockDir, hex.EncodeToString(sum[:8]))
	if len(socket) > 95 {
		return Store{}, fmt.Errorf("home path too long for OpenSSH control socket")
	}
	return Store{Dir: dir, Socket: socket}, nil
}
func (s Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another command is changing this instance")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func (s Store) load() (State, error) {
	var state State
	data, err := os.ReadFile(filepath.Join(s.Dir, "state.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}
func (s Store) save(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.Dir, "state.json"), append(data, '\n'))
}
func atomicWrite(dest string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}
func configHash(c Config) string {
	sum := sha256.Sum256([]byte(jsonString(c)))
	return hex.EncodeToString(sum[:])
}
