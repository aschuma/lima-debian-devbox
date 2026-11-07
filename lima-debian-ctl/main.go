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

const (
	exitUsage      = 2
	commandTimeout = 15 * time.Minute
)

const usage = `Usage: lima-debian-ctl COMMAND [--config FILE] [--json] [--yes] [-- COMMAND...]

Commands:
  init        write a .devvm.toml stub in the current directory
  plan        show what activate would change (no VM changes, no resolver run)
  activate    make this project the VM's active project (may stop/restart it)
  provision   refresh this project's environment without restarting
  status      report readiness; never prints environment values
  shell       open a login shell in the project directory
  exec        run a command in the project directory: exec -- CMD [ARG...]
  deactivate  remove credentials and tunnels, then stop the VM
  stop        alias of deactivate

Configuration is .devvm.toml, searched upward from the current directory.
Project mise setup is never run.
`

// cliOptions are the flags shared by every command.
type cliOptions struct {
	config string
	json   bool
	yes    bool
}

// cli bundles what command handlers need.
type cli struct {
	ctx        context.Context
	opts       cliOptions
	args       []string // positional arguments (exec only)
	controller Controller
	config     Config
	out        io.Writer
}

// handler runs one command and returns the process exit code.
type handler func(*cli) (int, error)

// commands maps names to handlers and declares which take the mutation lock.
var commands = map[string]struct {
	run   handler
	mutex bool
}{
	"plan":       {run: (*cli).plan},
	"status":     {run: (*cli).status},
	"shell":      {run: (*cli).shell},
	"exec":       {run: (*cli).exec},
	"activate":   {run: (*cli).activate, mutex: true},
	"provision":  {run: (*cli).provision, mutex: true},
	"deactivate": {run: (*cli).stop, mutex: true},
	"stop":       {run: (*cli).stop, mutex: true},
}

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
		fmt.Fprint(out, usage)
		return 0, nil
	}
	name := args[0]
	spec, known := commands[name]
	if !known && name != "init" {
		return exitUsage, fmt.Errorf("unknown command %q", name)
	}

	var opts cliOptions
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.config, "config", "", "project configuration file")
	flags.BoolVar(&opts.json, "json", false, "non-secret JSON output (not for shell/exec)")
	flags.BoolVar(&opts.yes, "yes", false, "approve mount replacement/restart without prompting")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return exitUsage, err
	}
	isSession := name == "shell" || name == "exec"
	switch {
	case name != "exec" && flags.NArg() != 0:
		return exitUsage, errors.New("unexpected positional arguments")
	case isSession && opts.json:
		return exitUsage, errors.New("shell/exec use native terminal output, not --json")
	case name == "exec" && flags.NArg() == 0:
		return exitUsage, errors.New("exec requires -- COMMAND [ARG...]")
	}
	if name == "init" {
		return 0, initConfig(opts.config, out)
	}

	c, err := loadConfig(opts.config)
	if err != nil {
		return 1, err
	}
	store, err := openStore(c.Instance)
	if err != nil {
		return 1, err
	}
	if !isSession {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	if spec.mutex {
		unlock, err := store.lock()
		if err != nil {
			return 1, err
		}
		defer unlock()
	}
	return spec.run(&cli{
		ctx:        ctx,
		opts:       opts,
		args:       flags.Args(),
		controller: Controller{Runner: runner, Store: store},
		config:     c,
		out:        out,
	})
}

// emit prints a non-secret result as JSON (compact with --json, indented otherwise).
func (k *cli) emit(v any) (int, error) {
	enc := json.NewEncoder(k.out)
	if !k.opts.json {
		enc.SetIndent("", "  ")
	}
	return 0, enc.Encode(v)
}

func (k *cli) plan() (int, error) {
	rec, err := k.controller.plan(k.ctx, k.config)
	if err != nil {
		return 1, err
	}
	return k.emit(rec.Plan)
}

func (k *cli) status() (int, error) {
	s, err := k.controller.Status(k.ctx, k.config)
	if err != nil {
		return 1, err
	}
	return k.emit(s)
}

func (k *cli) shell() (int, error) { return k.session(nil, true) }
func (k *cli) exec() (int, error)  { return k.session(k.args, false) }

func (k *cli) session(command []string, shell bool) (int, error) {
	// ssh -t forwards Ctrl-C to the remote; the host process must not act on it.
	signal.Ignore(os.Interrupt)
	return k.controller.Session(k.ctx, k.config, command, shell)
}

func (k *cli) activate() (int, error) {
	rec, err := k.controller.plan(k.ctx, k.config)
	if err != nil {
		return 1, err
	}
	approved := k.opts.yes
	if rec.Plan.ConfirmationRequired && !approved {
		if approved, err = k.confirm(rec.Plan); err != nil {
			return 1, err
		}
	}
	state, err := k.controller.Activate(k.ctx, k.config, rec, approved)
	if err != nil {
		return 1, err
	}
	return k.emit(state)
}

// confirm shows the plan and asks on a terminal; non-interactive use must pass --yes.
func (k *cli) confirm(p Plan) (bool, error) {
	if k.opts.json {
		return false, errors.New("activation requires --yes; inspect plan first")
	}
	if !stdinIsTerminal() {
		return false, errors.New("activation requires --yes in non-interactive use; inspect plan first")
	}
	_, _ = k.emit(p)
	fmt.Fprint(k.out, "This replaces the VM's mounts/automatic forwarding and may stop the VM and its sessions. Continue? [y/N] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return false, errors.New("activation cancelled")
	}
	return true, nil
}

func (k *cli) provision() (int, error) {
	if err := k.controller.Provision(k.ctx, k.config); err != nil {
		return 1, err
	}
	return k.emit(map[string]string{
		"project":     k.config.Project,
		"environment": "refreshed; existing processes must restart to inherit changes",
	})
}

func (k *cli) stop() (int, error) {
	state, err := k.controller.Stop(k.ctx, k.config)
	if err != nil {
		return 1, err
	}
	return k.emit(state)
}

const configTemplate = `schema_version = 1
project = %s
instance = %q

[[mounts]]
host = "."
guest = %s
writable = true

# Credentials: a host program that prints KEY=value lines (.env style).
# It runs in this directory without a terminal.
#[environment]
#resolver = ["cat", "./env.1p"]
#timeout_seconds = %d

# Private keys copied to ~/.ssh in the guest (the .pub next to it too).
#[[ssh_keys]]
#source = "~/.ssh/id_ed25519"

# Tunnels, loopback only. guest-to-host = ssh -R, host-to-guest = ssh -L.
#[[forwards]]
#direction = "guest-to-host"
#listen_port = 1080
#target_host = "127.0.0.1"
#target_port = 1080
`

// initConfig writes a fresh .devvm.toml and refuses to overwrite one.
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
	text := fmt.Sprintf(configTemplate, jsonString(name), defaultInstance, jsonString(guestMountRoot+name), 180)
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintln(out, "Created", abs)
	return nil
}
