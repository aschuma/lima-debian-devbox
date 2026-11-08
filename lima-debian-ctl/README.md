# lima-debian-ctl

The daily-use CLI for the shared Lima dev VM. It points the VM at one project
at a time. That project's folders become the only mounts, its credentials the
only environment, and its forwards the only open tunnels. Tools, caches and
Docker images on the VM disk survive a switch.

It works together with [`lima-debian-provision`](../lima-debian-provision/README.md),
which prepares the VM once. The controller assumes the VM exists and has been
provisioned. See the [top-level README](../README.md) for the overall picture.

## Install

Go 1.23+ on a macOS or Linux host. The CLI talks to `limactl` and `ssh` as
subprocesses. There is no daemon. Runtime dependencies: `go-toml/v2`, `x/sys`.

```bash
go install .                 # → $(go env GOPATH)/bin; make sure that is on PATH
./lima-debian-provision/provision.sh --tags project-session   # guest side, once
```

The Ansible tag installs `/usr/local/bin/lima-debian-ctl-guest`, shell loaders
and a systemd unit in the guest. `activate` refuses to run against a VM that
lacks them.

## Commands

| Command | Effect |
| --- | --- |
| `init` | Write a `.devvm.toml` stub. Refuses to overwrite. |
| `plan` | Show what `activate` would do. Reads only. Does not run the resolver. |
| `activate` | Make this project the VM's active project. Asks before stopping the VM or replacing mounts. `--yes` skips the question. |
| `provision` | Rerun the resolver and replace the env file. No restart. |
| `status` | Ready or not, and why. Lists each forward with whether its listener is bound, the SSH agent and inotify state, and whether the instance is protected. |
| `shell` | Login shell in the project directory. Shows a banner first. |
| `exec -- CMD...` | Run a command in the project directory. Exit code is passed through. |
| `deactivate`, `stop` | Delete the env file, close tunnels, stop the VM. |

Flags come after the command: `activate --yes --json`. Results are printed as
JSON. `--json` puts them on one line. Errors go to stderr. `shell` and `exec`
have no `--json`.

`.devvm.toml` is found by walking up from the current directory. Or pass it
with `--config`.

## .devvm.toml

One file per project. Commit it; it contains no secrets.

```toml
schema_version = 1
project = "service-a"      # [A-Za-z0-9_-], used as the env marker
instance = "devbox"        # Lima instance name
forward_agent = true       # forward the host SSH agent; default true
mount_inotify = true       # host edits raise inotify in the guest; default true

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

Top-level keys such as `forward_agent` must come before the first `[[...]]`
table.

Rejected: unknown keys, mounts that overlap, mounts that would expose the host
home or one of its parents, guest paths outside `/work/`, duplicate forward
listeners. There is no global config file. Everything is per project.

### Resolver

The resolver provides the project's credentials. It runs on the host, in the
project directory, with the host environment, no terminal and empty stdin. It
is killed with its process group at the timeout. Example output:

```text
ARTIFACTORY_USER=alice
ARTIFACTORY_TOKEN=abc=def
EMPTY=
```

Rules:

- One record per line. CR, LF or CRLF.
- Blank lines, `#` comments and a leading `export ` are ignored. A plain
  `.env` file works.
- The key is a shell identifier. The value is the rest of the line, as is.
  Quotes are kept. Nothing is interpolated.
- A name that appears twice takes its last value.
- Bad names, NUL bytes, non-UTF-8 and output over 1 MiB are errors.
- Reserved: `HOME`, `BASH_ENV`, `ENV`, `ZDOTDIR`, `IFS`, the read-only
  bash/zsh variables, and anything starting with `LIMA_DEBIAN_CTL_`.

On error the existing env file is left alone and no output is echoed.

The resolver is code you run. Read it before activating someone else's project.

### SSH agent

No private key is ever copied into the guest. Instead, Lima forwards your host
SSH agent into the VM.

With `forward_agent = true` (the default), `activate` sets Lima's
`ssh.forwardAgent` on the instance. The agent then appears in the guest at
`/run/host-services/ssh-auth.sock`. The guest shell loader and the
`exec`/`shell` helper export `SSH_AUTH_SOCK` to that socket when it exists. So
`git` and `ssh` sign with your host keys. The key material stays on the host.
A compromised guest could use the agent while connected, but never read the
keys. Keep the agent unlocked on the host.

`forward_agent` is part of the VM configuration. Changing it is drift, like a
mount change: `plan` reports an edit, and `activate` stops, edits and starts
the VM to apply it. With `forward_agent = false` the guest sees no socket and
leaves `SSH_AUTH_SOCK` unset.

Older versions had a `[[ssh_keys]]` table that copied private keys into the
guest. It is gone and is rejected as an unknown key. Keys installed by an older
version are not removed automatically. Delete them from the guest's `~/.ssh`
by hand; the old manifest is `~/.config/lima-debian-ctl/ssh-keys`.

### File watching

With `mount_inotify = true` (the default), `activate` sets Lima's
`mountInotify` on the instance. Lima then forwards inotify events from the
host to the guest for writable mounts. An IDE backend or build watcher in the
VM sees a file you saved on the Mac without polling.

The setting is part of the VM configuration. Changing it is drift, like a
mount change, and `activate` restarts the VM to apply it. Lima marks the
feature experimental. If file watching misbehaves, set `mount_inotify = false`
and run `activate`.

### Protection

The shared VM carries every installed tool and Docker image. On the first
`activate` the controller runs `limactl protect` on the instance, so
`limactl delete` refuses it until you run `limactl unprotect` yourself. Lima
has no snapshots for VZ instances; the controller's `lima.yaml` backup (see
[State](#state)) is the rollback point for configuration.

### Forwards

`guest-to-host` is `ssh -R`: a port inside the VM reaches a service on the
Mac, for example a SOCKS proxy or a local registry. `host-to-guest` is
`ssh -L`: a port on the Mac reaches a service in the VM. Both listen on
loopback only.

Lima's own automatic forwarding of guest ports is switched off at takeover. A
server you start in the VM is reachable from the Mac only if it is listed
here. Lima's SSH port is not affected.

## What activate does

1. Reads `limactl list --json` and the saved state. Decides whether an edit or
   restart is needed. If the VM is running, checks that the guest helpers are
   installed.
2. Runs the resolver on the host. A failure here changes nothing.
3. Backs up `lima.yaml` to the state directory. Protects the instance if it
   is not protected yet.
4. Closes the previous project's tunnels. If a restart is needed: deletes the
   old env file, runs `limactl stop`, then `limactl edit --set` with the
   mounts, the forwarding policy, `ssh.forwardAgent` and `mountInotify`, then
   `limactl start`, and waits for SSH.
5. Uploads the env file (`0600`, atomic rename) to
   `~/.config/lima-debian-ctl/environment.sh` in the guest. Uploads a banner
   with the project, mounts, forwards, variable *names* and the agent state.
6. Opens the tunnels on a private control socket. `-L` failures abort via
   `ExitOnForwardFailure`. `-R` listeners are checked with `ss` on the guest,
   because sshd only warns when a bind fails.
7. Checks the project marker through the helper and through `bash -lc`. A
   broken loader shows up now, not later in the IDE.
8. Writes `phase: ready` with the guest boot ID.

Same project and the VM already matches: step 4 only closes tunnels. No
restart. Different project, changed mounts, changed `forward_agent` or changed
`mount_inotify`: always a restart, so no process from the previous project
survives.

On any failure the phase is `incomplete`. Tunnels are closed and a freshly
uploaded env file is removed. The previous project's credentials are not
restored. Run `plan`, fix the cause, run `activate` again.

## Guest side

The env file is loaded by:

- `/etc/profile.d/lima-debian-ctl.sh` for login shells,
- `~/.bashrc`, before the interactive-only return,
- `/etc/zsh/zshenv` for zsh.

`exec` and `shell` go through the guest helper. It sources the file itself,
checks the project marker and changes to the first mount. It does not depend
on shell startup files.

`lima-debian-ctl-session.service` deletes the env file and the banner on clean
shutdown and on boot. A hard stop can leave them on disk until the next boot.
They are on the guest disk by design. They are not shredded; snapshots keep
them.

Changing the env file does not change running processes. Restart your IDE
backend or Gradle daemon after `provision`.

`shell`, and `exec` with a terminal on stdin, allocate a remote TTY. Ctrl-C
then reaches the remote command.

## State

`${XDG_STATE_HOME:-~/.local/state}/lima-debian-ctl/<instance>/` holds:

- `state.json` — project, phase, boot ID, config hash. No values.
- the lock file,
- `lima-before-edit.yaml` — the backup from step 3.

Control sockets are in `~/.lima-debian-ctl/sockets/`.

Readiness is tied to the config hash and the guest boot ID. If you edit
`.devvm.toml` or restart the VM by hand, `exec` and `provision` ask you to
`activate` again. Mutating commands take a non-blocking lock per instance.

## Testing

```bash
go test -race -cover ./...
go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

Tests drive the controller against a fake `limactl` and `ssh`. The real-VM
scenarios were run by hand on Lima 2.2.1: takeover, switch, refresh,
deactivate, cleanup after a plain `limactl stop`, `-R` traffic, bash and zsh
loading. IntelliJ Remote Development has not been exercised yet.
