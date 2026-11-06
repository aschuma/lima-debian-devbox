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

type Command struct {
	Name        string
	Args        []string
	Dir         string
	Input       string
	Interactive bool
	Quiet       bool // A daemonizing command must not inherit capture pipes.
	Limit       int
}
type Result struct {
	Stdout string
	Stderr string
	Code   int
}
type Runner interface {
	Run(context.Context, Command) (Result, error)
}
type ProcessRunner struct{}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	available := b.limit - b.buffer.Len()
	if n > available {
		b.overflow = true
		if available > 0 {
			_, _ = b.buffer.Write(p[:available])
		}
		return n, nil
	}
	return b.buffer.Write(p)
}
func stdinIsTerminal() bool {
	// ModeCharDevice is also true for /dev/null; ask the kernel for termios.
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	return err == nil
}

func (ProcessRunner) Run(ctx context.Context, c Command) (Result, error) {
	if c.Interactive {
		// Interactive sessions own the terminal: the user ends them via the
		// remote side, and a host SIGINT must not tear down ssh underneath them.
		ctx = context.WithoutCancel(ctx)
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if c.Interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd.Stdin = bytes.NewBufferString(c.Input)
		// Cancel the whole process group: resolver scripts can spawn children.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var once sync.Once
		cmd.Cancel = func() error {
			var err error
			once.Do(func() { err = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			return err
		}
	}
	cmd.WaitDelay = 2 * time.Second
	limit := c.Limit
	if limit == 0 {
		limit = 4 << 20
	}
	out, stderr := boundedBuffer{limit: limit}, boundedBuffer{limit: 64 << 10}
	if !c.Interactive && !c.Quiet {
		cmd.Stdout = &out
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	r := Result{Stdout: out.buffer.String(), Stderr: stderr.buffer.String()}
	if ctx.Err() != nil {
		r.Code = 124
		if errors.Is(ctx.Err(), context.Canceled) {
			r.Code = 130
		}
		return r, ctx.Err()
	}
	if out.overflow || stderr.overflow {
		return r, fmt.Errorf("command output exceeded limit")
	}
	if err == nil {
		return r, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.Code = exit.ExitCode()
		if r.Code < 0 {
			r.Code = 130
		}
		return r, nil
	}
	return r, err
}

func checked(ctx context.Context, r Runner, c Command) (string, error) {
	result, err := r.Run(ctx, c)
	// Never include raw command output: SSH errors may contain credential values.
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", c.Name, err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("%s exited with status %d", c.Name, result.Code)
	}
	return result.Stdout, nil
}
