package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func hasNativeForwards(rules []portForward) bool {
	for _, p := range rules {
		if !p.isIgnoreAll() {
			return true
		}
	}
	return false
}

// Lima's own SSH listener is not a project forward, even when the host-agent
// process owns it. Reject collisions before resolver execution or mutation.
func validateInstanceForwards(c Config, inst Instance) error {
	if inst.SSHLocalPort == 0 {
		return nil
	}
	for _, p := range c.NativeForwards {
		if p.GuestPort == inst.SSHLocalPort {
			return errors.New("native target is reserved by Lima's SSH forwarding policy")
		}
		if protocolsOverlap(p.Proto, "tcp") && p.HostPortRange[0] <= inst.SSHLocalPort && p.HostPortRange[1] >= inst.SSHLocalPort {
			return errors.New("native host listener overlaps Lima's SSH port")
		}
	}
	for _, f := range c.Forwards {
		if f.Direction == HostToGuest && f.ListenPort == inst.SSHLocalPort {
			return errors.New("custom host listener overlaps Lima's SSH port")
		}
	}
	return nil
}

func (c Config) forwardPolicy() []portForward {
	rules := append([]portForward{}, c.NativeForwards...)
	return append(rules, ignoreAllForwards()...)
}

func (c *Config) validateNativeForwards() error {
	for i := range c.NativeForwards {
		p := &c.NativeForwards[i]
		if err := normalizeForward(p); err != nil {
			return fmt.Errorf("lima.portForwards[%d]: %w", i, err)
		}
		for _, r := range c.NativeForwards[:i] {
			// Lima 2.3 suppresses dynamic forwarding by guest port alone when
			// a static rule covers it, regardless of protocol or guest IP.
			if p.Static != r.Static && rangesOverlap(p.GuestPortRange, r.GuestPortRange) {
				return errors.New("static and dynamic native rules must not overlap guest ports; Lima suppresses dynamic forwarding by port")
			}
			if !protocolsOverlap(p.Proto, r.Proto) {
				continue
			}
			hostOverlap := p.HostIP == r.HostIP && rangesOverlap(p.HostPortRange, r.HostPortRange)
			guestOverlap := guestIPsOverlap(*p, r) && rangesOverlap(p.GuestPortRange, r.GuestPortRange)
			if hostOverlap || guestOverlap {
				return errors.New("native forwarding rules must not have overlapping mappings")
			}
		}
	}
	return nil
}

func normalizeForward(p *portForward) error {
	if p.Ignore || p.Reverse || p.GuestSocket != "" || p.HostSocket != "" {
		return errors.New("socket, reverse and user-defined ignore rules are not supported")
	}
	if p.GuestIP == "" {
		p.GuestIP = "127.0.0.1"
		if p.GuestIPMustBeZero != nil && *p.GuestIPMustBeZero {
			p.GuestIP = "0.0.0.0"
		}
	}
	if p.HostIP == "" {
		p.HostIP = "127.0.0.1"
	}
	guestIP, hostIP := net.ParseIP(p.GuestIP), net.ParseIP(p.HostIP)
	if guestIP == nil {
		return errors.New("guestIP must be an IP address")
	}
	if hostIP == nil || !hostIP.IsLoopback() {
		return errors.New("hostIP must be a loopback IP address")
	}
	p.GuestIP, p.HostIP = guestIP.String(), hostIP.String()
	if p.GuestIPMustBeZero == nil {
		zero := p.GuestIP == "0.0.0.0"
		p.GuestIPMustBeZero = &zero
	}
	if *p.GuestIPMustBeZero && p.GuestIP != "0.0.0.0" {
		return errors.New("guestIPMustBeZero requires guestIP: 0.0.0.0")
	}
	if p.Proto == "" {
		p.Proto = "any"
	}
	if p.Proto != "tcp" && p.Proto != "udp" && p.Proto != "any" {
		return errors.New("proto must be tcp, udp or any")
	}
	if p.GuestPort != 0 {
		if len(p.GuestPortRange) > 0 && p.GuestPortRange[0] != p.GuestPort {
			return errors.New("guestPort must match the start of guestPortRange")
		}
		if len(p.GuestPortRange) == 0 {
			p.GuestPortRange = []int{p.GuestPort, p.GuestPort}
		}
	}
	if len(p.GuestPortRange) == 0 {
		p.GuestPortRange = []int{1, maxPort}
	}
	if p.HostPort != 0 {
		if len(p.HostPortRange) > 0 && p.HostPortRange[0] != p.HostPort {
			return errors.New("hostPort must match the start of hostPortRange")
		}
		if len(p.HostPortRange) == 0 {
			p.HostPortRange = []int{p.HostPort, p.HostPort}
		}
	}
	if len(p.HostPortRange) == 0 {
		p.HostPortRange = append([]int{}, p.GuestPortRange...)
	}
	if err := validatePortRange(p.GuestPortRange, 1); err != nil {
		return fmt.Errorf("guestPortRange: %w", err)
	}
	if err := validatePortRange(p.HostPortRange, minUnprivilegedPort); err != nil {
		return fmt.Errorf("hostPortRange: %w", err)
	}
	if p.GuestPortRange[1]-p.GuestPortRange[0] != p.HostPortRange[1]-p.HostPortRange[0] {
		return errors.New("guest and host ranges must contain the same number of ports")
	}
	if p.Static && (p.Proto != "tcp" || p.GuestPortRange[0] != p.GuestPortRange[1]) {
		return errors.New("static forwarding requires explicit proto: tcp and a single port")
	}
	// Canonicalize equivalent single-port and one-element-range declarations.
	p.GuestPort, p.HostPort = 0, 0
	if p.GuestPortRange[0] == p.GuestPortRange[1] {
		p.GuestPort, p.HostPort = p.GuestPortRange[0], p.HostPortRange[0]
	}
	return nil
}

func validatePortRange(r []int, min int) error {
	if len(r) != 2 {
		return errors.New("must contain exactly two ports")
	}
	if r[0] < min || r[1] > maxPort || r[0] > r[1] || r[0] == 22 || r[1] == 22 {
		return fmt.Errorf("must be ordered within %d..65535 and must not use port 22 as an endpoint", min)
	}
	return nil
}
func protocolsOverlap(a, b string) bool { return a == b || a == "any" || b == "any" }
func rangesOverlap(a, b []int) bool     { return a[0] <= b[1] && b[0] <= a[1] }
func guestIPsOverlap(a, b portForward) bool {
	return a.GuestIP == b.GuestIP || a.GuestIP == "0.0.0.0" || b.GuestIP == "0.0.0.0" || a.GuestIP == "127.0.0.1" && b.GuestIP == "::1" || b.GuestIP == "127.0.0.1" && a.GuestIP == "::1"
}

func forwardPoliciesEqual(actual, desired []portForward) bool {
	if len(actual) != len(desired) {
		return false
	}
	for i, p := range actual {
		q := desired[i]
		if q.isIgnoreAll() {
			if !p.isIgnoreAll() || p.Static || p.Reverse || p.HostSocket != "" {
				return false
			}
			continue
		}
		if err := normalizeForward(&p); err != nil {
			return false
		}
		if err := normalizeForward(&q); err != nil {
			return false
		}
		if jsonString(p) != jsonString(q) {
			return false
		}
	}
	return true
}

func (l limaClient) checkVersion(ctx context.Context) error {
	text, err := run(ctx, l.r, Command{Name: "limactl", Args: []string{"--version"}})
	if err != nil {
		return err
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return errors.New("cannot determine Lima version; require 2.3.0+")
	}
	version := strings.TrimPrefix(fields[len(fields)-1], "v")
	version = strings.Split(version, "+")[0]
	if strings.Contains(version, "-") {
		return errors.New("require a stable Lima version 2.3.0 or newer")
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return errors.New("invalid Lima version; require 2.3.0+")
	}
	nums := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return errors.New("invalid Lima version; require 2.3.0+")
		}
		nums[i] = n
	}
	if nums[0] < 2 || nums[0] == 2 && nums[1] < 3 {
		return errors.New("lima 2.3.0 or newer is required")
	}
	return nil
}

// forwarding owns the two concrete forwarding mechanisms through a small seam.
// Native lifecycle changes remain in the controller's stopped-instance edit.
type forwarding struct {
	runner Runner
	store  Store
	inst   Instance
}

func (f forwarding) Apply(ctx context.Context, c Config) error {
	if err := (tunnels{r: f.runner, inst: f.inst, socket: f.store.Socket}).Start(ctx, c.Forwards); err != nil {
		return err
	}
	if len(c.NativeForwards) == 0 {
		return nil
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		statuses, err := f.Check(ctx, c)
		if err != nil {
			return err
		}
		ready := true
		for _, s := range statuses {
			if !s.ready() {
				ready = false
				break
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("native forwarding listeners are not established (check for occupied ports)")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type forwardState string

const (
	waitingForGuest       forwardState = "waiting for guest listener"
	missingListener       forwardState = "listener missing"
	forwardingUnavailable forwardState = "forwarding not established"
	listenerEstablished   forwardState = "listening"
)

// NativeForwardStatus distinguishes configuration, listener discovery and health.
// Rules with no guest service are waiting, not failures. No application is probed.
type NativeForwardStatus struct {
	portForward
	Configured     bool         `json:"configured"`
	Listening      bool         `json:"listening"`
	State          forwardState `json:"state"`
	GuestListeners []string     `json:"guest_listeners"`
}

// ready describes forwarding setup, not target application health. Unknown
// states and unconfigured rules are deliberately not considered ready.
func (s NativeForwardStatus) ready() bool {
	if !s.Configured {
		return false
	}
	return s.State == waitingForGuest || s.State == listenerEstablished
}

func (f forwarding) Check(ctx context.Context, c Config) ([]NativeForwardStatus, error) {
	statuses := make([]NativeForwardStatus, 0, len(c.NativeForwards))
	if len(c.NativeForwards) == 0 {
		return statuses, nil
	}
	host, err := f.listeners(ctx, c)
	if err != nil {
		return nil, err
	}
	// ss enumerates actual guest listeners, not every possible port in a range.
	out, err := newGuest(f.runner, f.inst).bash(ctx, "/usr/bin/ss -Hlnut", "")
	if err != nil {
		return nil, fmt.Errorf("inspect native guest listeners: %w", err)
	}
	for _, p := range c.NativeForwards {
		s := NativeForwardStatus{portForward: p, Configured: forwardPoliciesEqual(f.inst.Config.PortForwards, c.forwardPolicy()), State: waitingForGuest, GuestListeners: []string{}}
		if p.Static {
			s.Listening = host["static:tcp:"+net.JoinHostPort(p.HostIP, strconv.Itoa(p.HostPort))]
			s.State = missingListener
			if s.Listening {
				s.State = listenerEstablished
			}
			statuses = append(statuses, s)
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			cols := strings.Fields(line)
			if len(cols) < 5 || !protocolsOverlap(cols[0], p.Proto) {
				continue
			}
			ip, portText, err := net.SplitHostPort(cols[4])
			if err != nil {
				continue
			}
			port, err := strconv.Atoi(portText)
			if err != nil || port == 22 || port == f.inst.SSHLocalPort || port < p.GuestPortRange[0] || port > p.GuestPortRange[1] || !matchesGuestIP(p, ip) {
				continue
			}
			s.GuestListeners = append(s.GuestListeners, cols[0]+":"+cols[4])
		}
		if len(s.GuestListeners) > 0 {
			s.Listening = true
			for _, listener := range s.GuestListeners {
				proto, addr, _ := strings.Cut(listener, ":")
				_, portText, _ := net.SplitHostPort(addr)
				port, _ := strconv.Atoi(portText)
				hostPort := port + p.HostPortRange[0] - p.GuestPortRange[0]
				s.Listening = s.Listening && host["dynamic:"+proto+":"+net.JoinHostPort(p.HostIP, strconv.Itoa(hostPort))]
			}
			s.State = forwardingUnavailable
			if s.Listening {
				s.State = listenerEstablished
			}
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

func matchesGuestIP(p portForward, ip string) bool {
	addr := net.ParseIP(ip)
	if ip == "*" {
		return true
	}
	if addr == nil {
		return false
	}
	if addr.IsUnspecified() {
		return true
	}
	return ip == p.GuestIP || ip == "::1" && p.GuestIP == "127.0.0.1" || p.GuestIP == "0.0.0.0" && !*p.GuestIPMustBeZero
}

func tcpListening(ip string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// hostAgentPID identifies the controlled Lima host-agent generation.
func hostAgentPID(dir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "ha.pid"))
	if err != nil {
		return 0, fmt.Errorf("read Lima host-agent PID: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, errors.New("invalid Lima host-agent PID")
	}
	return pid, nil
}

// listeners checks OS socket ownership, not just whether an arbitrary process
// answers on a port. lsof supports both TCP and UDP on macOS and Linux.
func (f forwarding) listeners(ctx context.Context, c Config) (map[string]bool, error) {
	pid, err := hostAgentPID(f.inst.Dir)
	if err != nil {
		return nil, err
	}
	text, err := run(ctx, f.runner, Command{Name: "ps", Args: []string{"-p", strconv.Itoa(pid), "-o", "comm="}})
	if err != nil || !strings.Contains(text, "limactl") {
		return nil, errors.New("lima host agent is unavailable")
	}
	pids := []int{pid}
	for _, p := range c.NativeForwards {
		if !p.Static {
			continue
		}
		r, err := f.runner.Run(ctx, Command{Name: "ssh", Args: []string{"-F", f.inst.SSHConfigFile, "-o", "BatchMode=yes", "-O", "check", f.inst.Hostname}})
		if err != nil || r.Code != 0 {
			return nil, errors.New("lima's static SSH forwarding master is unavailable")
		}
		_, value, ok := strings.Cut(r.Stderr, "pid=")
		if !ok {
			return nil, errors.New("cannot determine Lima's static SSH master PID")
		}
		value, _, _ = strings.Cut(value, ")")
		pid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || pid <= 0 {
			return nil, errors.New("invalid Lima SSH master PID")
		}
		pids = append(pids, pid)
		break
	}
	host := make(map[string]bool)
	for i, pid := range pids {
		mode := "dynamic"
		if i > 0 {
			mode = "static"
		}
		r, err := f.runner.Run(ctx, Command{Name: "lsof", Args: []string{"-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-iUDP", "-FPTn"}})
		if err != nil {
			return nil, fmt.Errorf("inspect native listeners (lsof is required): %w", err)
		}
		if r.Code > 1 {
			return nil, errors.New("lsof could not inspect native listeners")
		}
		proto, addr, state := "", "", ""
		commit := func() {
			if addr == "" || strings.Contains(addr, "->") {
				return
			}
			if proto == "tcp" && state != "LISTEN" {
				return
			}
			if proto == "tcp" || proto == "udp" {
				host[mode+":"+proto+":"+addr] = true
			}
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			if strings.HasPrefix(line, "f") {
				commit()
				proto, addr, state = "", "", ""
			}
			if strings.HasPrefix(line, "P") {
				proto = strings.ToLower(line[1:])
			}
			if strings.HasPrefix(line, "n") {
				addr = line[1:]
			}
			if strings.HasPrefix(line, "TST=") {
				state = line[4:]
			}
		}
		commit()
	}
	return host, nil
}
