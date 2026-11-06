package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code, err := runCLI(ctx, os.Args[1:], ProcessRunner{}, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
func runCLI(ctx context.Context, args []string, runner Runner, out, stderr io.Writer) (int, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(out, "Usage: lima-debian-ctl COMMAND [--config FILE] [--json] [--yes] [-- COMMAND...]\n\nCommands: init, plan, activate, provision, status, shell, exec, deactivate, stop\nConfiguration: .devvm.toml (searched upward from the current directory).\nProject mise setup is never run. activate may stop/restart the VM.")
		return 0, nil
	}
	command := args[0]
	switch command {
	case "init", "plan", "activate", "provision", "status", "shell", "exec", "deactivate", "stop":
	default:
		return 2, fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	filename := flags.String("config", "", "project configuration file")
	asJSON := flags.Bool("json", false, "non-secret JSON output (not for shell/exec)")
	yes := flags.Bool("yes", false, "approve project takeover/restart without prompting")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if command != "exec" && flags.NArg() != 0 {
		return 2, fmt.Errorf("unexpected positional arguments")
	}
	if (command == "exec" || command == "shell") && *asJSON {
		return 2, fmt.Errorf("shell/exec use native terminal output, not --json")
	}
	if command == "exec" && flags.NArg() == 0 {
		return 2, fmt.Errorf("exec requires -- COMMAND [ARG...]")
	}
	if command == "init" {
		return 0, initConfig(*filename, out)
	}
	c, err := loadConfig(*filename)
	if err != nil {
		return 1, err
	}
	store, err := openStore(c.Instance)
	if err != nil {
		return 1, err
	}
	controller := Controller{Runner: runner, Store: store}
	if command == "shell" || command == "exec" {
		// ssh -t forwards Ctrl-C to the remote; the host process must not act on it.
		signal.Ignore(os.Interrupt)
		return controller.Session(ctx, c, flags.Args(), command == "shell")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	emit := func(v any) (int, error) {
		if *asJSON {
			return 0, json.NewEncoder(out).Encode(v)
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(out, string(b))
		return 0, nil
	}
	if command == "plan" {
		rec, err := controller.plan(ctx, c)
		if err != nil {
			return 1, err
		}
		return emit(rec.Plan)
	}
	if command == "status" {
		s, err := controller.Status(ctx, c)
		if err != nil {
			return 1, err
		}
		return emit(s)
	}
	unlock, err := store.lock()
	if err != nil {
		return 1, err
	}
	defer unlock()
	switch command {
	case "activate":
		rec, err := controller.plan(ctx, c)
		if err != nil {
			return 1, err
		}
		if rec.Plan.ConfirmationRequired && !*yes {
			if *asJSON {
				return 1, fmt.Errorf("activation requires --yes; inspect plan first")
			}
			if !stdinIsTerminal() {
				return 1, fmt.Errorf("activation requires --yes in non-interactive use; inspect plan first")
			}
			_, _ = emit(rec.Plan)
			fmt.Fprint(out, "This replaces the VM's mounts/automatic forwarding and may stop the VM and its sessions. Continue? [y/N] ")
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				return 1, fmt.Errorf("activation cancelled")
			}
			*yes = true
		}
		state, err := controller.Activate(ctx, c, rec, *yes)
		if err != nil {
			return 1, err
		}
		return emit(state)
	case "provision":
		if err := controller.Provision(ctx, c); err != nil {
			return 1, err
		}
		return emit(map[string]string{"project": c.Project, "environment": "refreshed; existing processes must restart to inherit changes"})
	case "stop", "deactivate":
		state, err := controller.Stop(ctx, c)
		if err != nil {
			return 1, err
		}
		return emit(state)
	}
	return 2, fmt.Errorf("unsupported command")
}
func initConfig(filename string, out io.Writer) error {
	if filename == "" {
		filename = configName
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return err
	}
	name := filepath.Base(filepath.Dir(abs))
	if !identifier.MatchString(name) {
		name = "my-project"
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("schema_version = 1\nproject = %s\ninstance = \"devbox\"\n\n[[mounts]]\nhost = \".\"\nguest = %s\nwritable = true\n\n[environment]\n# Runs on the host from this directory. Output: literal KEY=value records.\n# resolver = [\"./scripts/resolve-dev-env\"]\ntimeout_seconds = 60\n\n# [[forwards]]\n# direction = \"guest-to-host\"\n# listen_port = 1080\n# target_host = \"127.0.0.1\"\n# target_port = 1080\n", jsonString(name), jsonString("/work/"+name))
	_, writeErr := f.WriteString(text)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Fprintln(out, "Created", abs)
	return nil
}
