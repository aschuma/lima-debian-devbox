package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
)

func tunnelArgs(i Instance, socket string) []string {
	return []string{"-F", i.SSHConfigFile, "-S", socket, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
}
func tunnelCheck(ctx context.Context, r Runner, i Instance, socket string) bool {
	args := append(tunnelArgs(i, socket), "-O", "check", i.Hostname)
	result, err := r.Run(ctx, Command{Name: "ssh", Args: args})
	return err == nil && result.Code == 0
}
func stopTunnels(ctx context.Context, r Runner, i Instance, socket string) error {
	if _, err := os.Stat(socket); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if tunnelCheck(ctx, r, i, socket) {
		_, err := checked(ctx, r, Command{Name: "ssh", Args: append(tunnelArgs(i, socket), "-O", "exit", i.Hostname)})
		if err != nil {
			return fmt.Errorf("stop managed tunnels: %w", err)
		}
	}
	// An unreachable stale socket is safe to unlink in our private directory.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func startTunnels(ctx context.Context, r Runner, i Instance, c Config, socket string) error {
	if len(c.Forwards) == 0 {
		return nil
	}
	args := append(tunnelArgs(i, socket), "-M", "-f", "-N", "-o", "ControlPersist=no", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3")
	for _, f := range c.Forwards {
		kind := "-L"
		if f.Direction == "guest-to-host" {
			kind = "-R"
		}
		spec := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.ListenPort)) + ":" + net.JoinHostPort(f.TargetHost, strconv.Itoa(f.TargetPort))
		args = append(args, kind, spec)
	}
	args = append(args, i.Hostname)
	if _, err := checked(ctx, r, Command{Name: "ssh", Args: args, Quiet: true}); err != nil {
		return fmt.Errorf("establish project tunnels (check for occupied ports): %w", err)
	}
	if !tunnelCheck(ctx, r, i, socket) {
		return fmt.Errorf("managed tunnel connection did not become ready")
	}
	// ExitOnForwardFailure covers -L; a failed -R bind is only a server warning.
	for _, f := range c.Forwards {
		if f.Direction == "guest-to-host" && !guestListening(ctx, r, i, f.ListenPort) {
			_ = stopTunnels(ctx, r, i, socket)
			return fmt.Errorf("guest-to-host forward on guest port %d is not listening (port in use on the guest?)", f.ListenPort)
		}
	}
	return nil
}
