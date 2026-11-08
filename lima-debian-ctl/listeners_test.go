package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestNativeListenerOwnershipWithRealHostSockets(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ha.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	// Supply Lima's process identity while testing real lsof output for this
	// test process. No Lima instance is observed, started, stopped or edited.
	r := runnerFunc(func(ctx context.Context, c Command) (Result, error) {
		if c.Name == "ps" {
			return Result{Stdout: "limactl\n"}, nil
		}
		return (ProcessRunner{}).Run(ctx, c)
	})
	f := forwarding{runner: r, inst: Instance{Dir: dir}}
	ports, err := f.listeners(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !ports["dynamic:tcp:"+tcp.Addr().String()] || !ports["dynamic:udp:"+udp.LocalAddr().String()] {
		t.Fatalf("could not identify owned TCP/UDP listeners: %v", ports)
	}
}
