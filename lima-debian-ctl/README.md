# lima-debian-ctl

CLI that scopes a reusable Lima VM to one project at a time: the project's
folders are the only mounts, its credentials are the only environment, its
forwards are the only open tunnels. Tools, caches and Docker images on the
guest disk survive a switch.

Go 1.23+, macOS or Linux host. Talks to `limactl` and `ssh` as subprocesses;
no daemon. Runtime dependencies: `go-toml/v2`, `x/sys`.

```bash
go install .                 # → $(go env GOPATH)/bin; make sure that is on PATH
./lima-debian-provision/provision.sh --tags project-session   # guest, once
```

The Ansible tag installs `/usr/local/bin/lima-debian-ctl-guest`, shell loaders
and a systemd unit. `activate` refuses to run against a VM that lacks them.

## Commands

| Command | Effect |
| --- | --- |
| `init` | Write a `.devvm.toml` stub. Refuses to overwrite. |
| `plan` | Show what `activate` would do. Reads only; does not run the resolver. |
| `activate` | Make this project the VM's active project. Prompts if it has to stop the VM or replace mounts; `--yes` skips the prompt. |
| `provision` | Rerun the resolver and replace the env file. No restart. |
| `status` | Ready or not, and why. |
| `shell` | Login shell in the project directory. |
| `exec -- CMD...` | Run a command there. Exit code is passed through. |
| `deactivate`, `stop` | Delete the env file, close tunnels, stop the VM. |

Flags come after the command: `activate --yes --json`. `--json` prints the
result compactly; errors go to stderr. `shell` and `exec` have no `--json`.
`.devvm.toml` is found by walking up from the current directory, or given
with `--config`.

## .devvm.toml

```toml
schema_version = 1
project = "service-a"      # [A-Za-z0-9_-], used as the env marker
instance = "devbox"        # Lima instance name

[[mounts]]                 # first mount = working directory for shell/exec
host = "."                 # relative to this file; symlinks resolved
guest = "/work/service-a"  # must be under /work/
writable = true

[environment]
resolver = ["./scripts/dev-env"]   # argv; "./x" is relative to this file
timeout_seconds = 60               # 1..600

[[forwards]]
direction = "guest-to-host"        # or host-to-guest
listen_port = 1080                 # 1024..65535, binds 127.0.0.1
target_host = "127.0.0.1"
target_port = 1080
```

Rejected: unknown keys, mounts that overlap, mounts that would expose the host
home or one of its parents, guest paths outside `/work/`, duplicate forward
listeners. There is no global config file; everything is per project.

### Resolver

Runs on the host, in the project directory, with the host environment, no
terminal, empty stdin. Killed with its process group at the timeout. Output:

```text
ARTIFACTORY_USER=alice
ARTIFACTORY_TOKEN=abc=def
EMPTY=
```

One record per line, CR, LF or CRLF. Blank lines, `#` comments and a leading
`export ` are ignored, so a plain `.env` file works. The key is a shell
identifier; the value is the rest of the line, untouched — quotes are kept,
nothing is interpolated. Duplicates, bad names, NUL bytes, non-UTF-8 and output over
1 MiB are errors. `HOME`, `BASH_ENV`, `ENV`, `ZDOTDIR`, `IFS`, the read-only
bash/zsh variables and anything starting with `LIMA_DEBIAN_CTL_` are reserved.
On error the existing env file is left alone and no output is echoed.

The resolver is code you run. Read it before activating someone else's project.

### Forwards

`guest-to-host` is `ssh -R`: a port inside the VM reaches a service on the
Mac (SOCKS proxy, local registry). `host-to-guest` is `ssh -L`: a port on the
Mac reaches a service in the VM. Both listen on loopback only.

Lima's own automatic forwarding of guest ports is switched off at takeover, so
a server you start in the VM is reachable from the Mac only if it is listed
here. Lima's SSH port is unaffected.

## What activate does

1. Reads `limactl list --json` and the saved state; decides whether an edit
   or restart is needed.
2. Runs the resolver. Fails here, and nothing has changed yet.
3. Backs up `lima.yaml` to the state directory.
4. Closes the previous project's tunnels. If restarting: deletes the old env
   file, `limactl stop`, `limactl edit --set` mounts and forwarding policy,
   `limactl start`, waits for SSH.
5. Uploads the env file (`0600`, atomic rename) to
   `~/.config/lima-debian-ctl/environment.sh` in the guest.
6. Opens the tunnels on a private control socket. `-L` failures abort via
   `ExitOnForwardFailure`; `-R` listeners are checked with `ss` on the guest
   because sshd only warns when a bind fails.
7. Checks the marker both through the helper and through `bash -lc`, so a
   broken loader shows up now rather than in the IDE.
8. Writes `phase: ready` with the guest boot ID.

Same project, unchanged config, VM already matching: steps 4 skip the restart.
Different project or changed mounts: always a restart, so no process from the
previous project survives.

On any failure the phase is `incomplete`, tunnels are closed and a freshly
uploaded env file is removed. The previous project's credentials are not
restored. Run `plan`, fix the cause, `activate` again.

## Guest side

The env file is loaded by `/etc/profile.d/lima-debian-ctl.sh` (login shells),
`~/.bashrc` (before the interactive-only return) and `/etc/zsh/zshenv`. `exec`
and `shell` go through the guest helper, which sources the file itself, checks
the project marker and `cd`s to the first mount — no dependency on shell
startup files.

`lima-debian-ctl-session.service` deletes the env file on clean shutdown and
on boot. A hard stop can leave it on disk until the next boot; the file is on
the guest disk by design and is not shredded. Snapshots keep it.

Changing the file does not change running processes. Restart your IDE backend
or Gradle daemon after `provision`.

`shell`, and `exec` with a terminal on stdin, allocate a remote TTY so Ctrl-C
reaches the remote command.

## State

`${XDG_STATE_HOME:-~/.local/state}/lima-debian-ctl/<instance>/` holds
`state.json` (project, phase, boot ID, config hash — no values), the lock file
and `lima-before-edit.yaml`. Control sockets are in
`~/.lima-debian-ctl/sockets/`.

Readiness is tied to the config hash and the guest boot ID. Edit
`.devvm.toml` or restart the VM by hand and `exec`/`provision` ask you to
`activate` again. Mutating commands take a non-blocking lock per instance.

## Testing

```bash
go test -race -cover ./...
go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

Tests drive the controller against a fake `limactl`/`ssh`. The real-VM
scenarios (takeover, switch, refresh, deactivate, cleanup after plain
`limactl stop`, `-R` traffic, bash/zsh loading) were run by hand on Lima 2.2.1.
IntelliJ Remote Development has not been exercised yet.
