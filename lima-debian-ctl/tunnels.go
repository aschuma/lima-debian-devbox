package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// tunnels manages the project's SSH forwards as one dedicated OpenSSH master
// connection identified by a private control socket.
type tunnels struct {
	r      Runner
	inst   Instance
	socket string
}

func (t tunnels) args(extra ...string) []string {
	base := []string{"-F", t.inst.SSHConfigFile, "-S", t.socket, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	return append(base, extra...)
}

// Alive reports whether the master connection answers on the control socket.
func (t tunnels) Alive(ctx context.Context) bool {
	result, err := t.r.Run(ctx, Command{Name: "ssh", Args: t.args("-O", "check", t.inst.Hostname)})
	return err == nil && result.Code == 0
}

// Stop closes the master connection if alive and removes the socket.
func (t tunnels) Stop(ctx context.Context) error {
	if _, err := os.Stat(t.socket); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if t.Alive(ctx) {
		if _, err := run(ctx, t.r, Command{Name: "ssh", Args: t.args("-O", "exit", t.inst.Hostname)}); err != nil {
			return fmt.Errorf("stop managed tunnels: %w", err)
		}
	}
	// An unreachable stale socket is safe to unlink in our private directory.
	if err := os.Remove(t.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Start opens the forwards and verifies them. Host listeners (-L) fail the
// connection via ExitOnForwardFailure; guest listeners (-R) are checked on
// the guest because sshd only warns when it cannot bind.
func (t tunnels) Start(ctx context.Context, forwards []Forward) error {
	if len(forwards) == 0 {
		return nil
	}
	args := t.args("-M", "-f", "-N",
		"-o", "ControlPersist=no", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3")
	for _, f := range forwards {
		args = append(args, f.sshFlag(), f.sshSpec())
	}
	args = append(args, t.inst.Hostname)
	if _, err := run(ctx, t.r, Command{Name: "ssh", Args: args, Quiet: true}); err != nil {
		return fmt.Errorf("establish project tunnels (check for occupied ports): %w", err)
	}
	if !t.Alive(ctx) {
		return errors.New("managed tunnel connection did not become ready")
	}
	g := newGuest(t.r, t.inst)
	for _, f := range forwards {
		if f.Direction == GuestToHost && !g.Listening(ctx, f.ListenPort) {
			_ = t.Stop(ctx)
			return fmt.Errorf("guest-to-host forward on guest port %d is not listening (port in use on the guest?)", f.ListenPort)
		}
	}
	return nil
}

func (f Forward) sshFlag() string {
	if f.Direction == GuestToHost {
		return "-R"
	}
	return "-L"
}

func (f Forward) sshSpec() string {
	listen := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.ListenPort))
	return listen + ":" + net.JoinHostPort(f.TargetHost, strconv.Itoa(f.TargetPort))
}
