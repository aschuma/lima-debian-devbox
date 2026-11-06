package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunnerTimeoutKillsResolverChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (ProcessRunner{}).Run(ctx, Command{Name: "/bin/sh", Args: []string{"-c", "sleep 30 & wait"}})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatal("resolver descendants were not cancelled promptly")
	}
}
func TestRunnerBoundsOutput(t *testing.T) {
	_, err := (ProcessRunner{}).Run(context.Background(), Command{Name: "/bin/sh", Args: []string{"-c", "printf 'secret-too-long'"}, Limit: 4})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("output limit failed or leaked contents")
	}
}
func TestRunnerPreservesExitStatus(t *testing.T) {
	r, err := (ProcessRunner{}).Run(context.Background(), Command{Name: "/bin/sh", Args: []string{"-c", "exit 23"}})
	if err != nil || r.Code != 23 {
		t.Fatal("exit code lost")
	}
}
func TestDaemonCommandDoesNotWaitForInheritedCapturePipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := (ProcessRunner{}).Run(ctx, Command{Name: "/bin/sh", Args: []string{"-c", "sleep 1 &"}, Quiet: true})
	if err != nil || time.Since(start) > 750*time.Millisecond {
		t.Fatal("daemon command retained capture pipes")
	}
}
