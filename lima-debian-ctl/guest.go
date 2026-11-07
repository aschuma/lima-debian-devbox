package main

import (
	"context"
	"fmt"
	"strings"
)

const guestHelper = "/usr/local/bin/lima-debian-ctl-guest"

// guest runs commands inside one instance over SSH, using Lima's generated
// SSH config but never Lima's shared control master.
type guest struct {
	r    Runner
	inst Instance
}

func newGuest(r Runner, inst Instance) guest { return guest{r: r, inst: inst} }

// sshArgs returns connection options for a one-off, forward-free session.
func (g guest) sshArgs() []string {
	return []string{
		"-F", g.inst.SSHConfigFile,
		// Our own connections: no reuse of Lima's master (and its stale
		// group/env state), no inherited forwards.
		"-S", "none", "-o", "ControlMaster=no", "-o", "ClearAllForwardings=yes",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
	}
}

// bash runs script under a bare bash (no profile) with input on stdin.
func (g guest) bash(ctx context.Context, script, input string) (string, error) {
	args := append(g.sshArgs(), g.inst.Hostname, "/bin/bash --noprofile --norc -c "+shellQuote(script))
	out, err := run(ctx, g.r, Command{Name: "ssh", Args: args, Input: input})
	return strings.TrimSpace(out), err
}

func (g guest) helper(ctx context.Context, subcommand, input string) (string, error) {
	return g.bash(ctx, guestHelper+" "+subcommand, input)
}

// BootID verifies the helper and cleanup service are installed and returns
// the guest's boot ID, which changes on every (re)start.
func (g guest) BootID(ctx context.Context) (string, error) {
	return g.helper(ctx, "check", "")
}

// Provision installs the environment file and the banner atomically.
func (g guest) Provision(ctx context.Context, p payload) error {
	if _, err := g.helper(ctx, "provision", p.Environment); err != nil {
		return err
	}
	_, err := g.helper(ctx, "motd", p.MOTD)
	return err
}

// Clear removes the environment file.
func (g guest) Clear(ctx context.Context) error {
	_, err := g.helper(ctx, "clear", "")
	return err
}

// PruneMounts removes empty, unmounted leftover directories under /work.
func (g guest) PruneMounts(ctx context.Context) error {
	_, err := g.helper(ctx, "prune-mounts", "")
	return err
}

// Project returns the project marker from the installed environment file.
func (g guest) Project(ctx context.Context) (string, error) {
	return g.helper(ctx, "status", "")
}

// LoginShellProject reads the marker through a real login shell, i.e. the
// path plain SSH sessions and IDE backends take, not the helper's sourcing.
func (g guest) LoginShellProject(ctx context.Context) (string, error) {
	args := append(g.sshArgs(), g.inst.Hostname, `/bin/bash -lc 'printf %s "$LIMA_DEBIAN_CTL_PROJECT"'`)
	out, err := run(ctx, g.r, Command{Name: "ssh", Args: args})
	return strings.TrimSpace(out), err
}

// Listening reports whether a TCP listener exists on the guest port. sshd
// only warns when a -R listener cannot bind, so tunnels check explicitly.
func (g guest) Listening(ctx context.Context, port int) bool {
	_, err := g.bash(ctx, fmt.Sprintf("/usr/bin/ss -Hltn 'sport = :%d' | /usr/bin/grep -q .", port), "")
	return err == nil
}

// Session attaches the terminal to the project shell or to command, run
// through the helper so the project environment and workdir are verified
// guest-side. The remote exit status is returned.
func (g guest) Session(ctx context.Context, c Config, command []string, shell bool) (int, error) {
	remote := guestHelper + " exec " + shellQuote(c.Project) + " " + shellQuote(c.workDir())
	if shell {
		remote += " --shell"
	} else {
		remote += " --"
		for _, arg := range command {
			remote += " " + shellQuote(arg)
		}
	}
	args := g.sshArgs()
	// A TTY lets Ctrl-C reach the remote command instead of only killing ssh.
	if shell || stdinIsTerminal() {
		args = append(args, "-t")
	}
	args = append(args, g.inst.Hostname, remote)
	result, err := g.r.Run(ctx, Command{Name: "ssh", Args: args, Interactive: true})
	return result.Code, err
}
