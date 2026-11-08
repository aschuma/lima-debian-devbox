package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultOutputLimit = 4 << 20
	stderrLimit        = 64 << 10
	exitTimeout        = 124 // like coreutils timeout(1)
	exitInterrupted    = 130
)

// Command describes one external process. All controller side effects on the
// host and guest go through Runner so tests can substitute a fake.
type Command struct {
	Name  string
	Args  []string
	Dir   string
	Input string   // stdin for non-interactive commands
	Env   []string // explicit overrides of the inherited host environment
	// Interactive hands the terminal to the child (shell/exec sessions).
	Interactive bool
	// Quiet drops stdout/stderr; required for commands that daemonize
	// (ssh -f) and would otherwise keep the capture pipes open.
	Quiet bool
	Limit int // stdout cap in bytes; 0 means defaultOutputLimit
}

// Result is the captured outcome of a finished command.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Runner executes commands. A non-nil error means the command could not be
// run to completion (spawn failure, timeout, output overflow); a non-zero
// Code with nil error is an ordinary failing exit.
type Runner interface {
	Run(context.Context, Command) (Result, error)
}

// ProcessRunner is the real Runner backed by os/exec.
type ProcessRunner struct{}

func (ProcessRunner) Run(ctx context.Context, c Command) (Result, error) {
	if c.Interactive {
		// The user ends an interactive session from the remote side; a host
		// SIGINT must not tear ssh down underneath them.
		ctx = context.WithoutCancel(ctx)
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(cmd.Environ(), c.Env...)
	}
	cmd.WaitDelay = 2 * time.Second

	limit := c.Limit
	if limit == 0 {
		limit = defaultOutputLimit
	}
	stdout, stderr := &boundedBuffer{limit: limit}, &boundedBuffer{limit: stderrLimit}

	switch {
	case c.Interactive:
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	default:
		cmd.Stdin = bytes.NewBufferString(c.Input)
		if !c.Quiet {
			cmd.Stdout, cmd.Stderr = stdout, stderr
		}
		killProcessGroupOnCancel(cmd)
	}

	err := cmd.Run()
	r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	switch {
	case ctx.Err() != nil:
		r.Code = exitTimeout
		if errors.Is(ctx.Err(), context.Canceled) {
			r.Code = exitInterrupted
		}
		return r, ctx.Err()
	case stdout.overflow || stderr.overflow:
		return r, errors.New("command output exceeded limit")
	case err == nil:
		return r, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.Code = exit.ExitCode()
		if r.Code < 0 { // killed by signal
			r.Code = exitInterrupted
		}
		return r, nil
	}
	return r, err
}

// killProcessGroupOnCancel makes cancellation reach the children a resolver
// script may have spawned, not just the script itself.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var once sync.Once
	cmd.Cancel = func() error {
		var err error
		once.Do(func() { err = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		return err
	}
}

// run executes c and returns stdout, treating a non-zero exit as an error.
// Output is never included in the error: it may carry credential values.
func run(ctx context.Context, r Runner, c Command) (string, error) {
	result, err := r.Run(ctx, c)
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", c.Name, err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("%s exited with status %d", c.Name, result.Code)
	}
	return result.Stdout, nil
}

// boundedBuffer keeps the first limit bytes and remembers that it overflowed.
type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p) // report full consumption so the copier keeps draining
	if available := b.limit - b.buf.Len(); n > available {
		b.overflow = true
		p = p[:max(available, 0)]
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// stdinIsTerminal asks the kernel for termios: os.ModeCharDevice is also
// set for /dev/null, which must not count as a terminal.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	return err == nil
}
