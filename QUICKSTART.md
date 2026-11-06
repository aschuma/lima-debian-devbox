# Quick start: project-scoped development with Lima

This setup uses an **existing Debian 12 ARM64 Lima VM** with an existing user,
passwordless sudo, and `/usr/bin/python3`. It does not create or rebuild the VM.
Only one project is active at a time; the guest disk and tools are reused.

## 1. Prepare host tools

On an Apple Silicon Mac, one option is:

```bash
brew install lima go ansible just
```

Go 1.23+ is required for building the controller, not for running a built binary.
Python 3 and OpenSSH must also be available on the host. For optional local
linting, install `shellcheck` and `uv`.

Check the existing VM:

```bash
limactl list
```

If you do not already have a compatible VM, create one separately using Lima's
Debian 12 configuration before continuing. Provisioning does not assume or
require a shared host home. The default instance/user are `devbox`/`aschuma`;
substitute your existing account as needed.

## 2. Provision the base guest

From the repository root:

```bash
just deps

# Optional overrides for an existing instance/account:
export LIMA_INSTANCE=devbox
export LIMA_USER=aschuma

./lima-debian-provision/provision.sh --preflight
./lima-debian-provision/provision.sh
```

**The second command modifies the guest:** it installs base tools, Docker,
mise runtimes/CLI tools (including Pi), shell configuration, and project-session
helpers. Back up existing guest shell and mise customizations first.

If base provisioning was already applied before the controller helpers existed,
install just those helpers:

```bash
./lima-debian-provision/provision.sh --tags project-session
```

This installs environment loaders, an SSH execution helper, and a systemd
shutdown/startup cleanup service. Its first start clears an existing
controller-generated environment file, if present.

Docker-group membership requires a fresh login/group context for old sessions.
See the [base provisioning reference](lima-debian-provision/README.md).

## 3. Install the controller

```bash
just test
just install
lima-debian-ctl --help
```

Ensure `$(go env GOPATH)/bin` is on PATH unless you configured `GOBIN`.
Alternatively, `just build` writes `lima-debian-ctl/bin/lima-debian-ctl`.

## 4. Configure a project

In the project directory, not this repository unless it is the project you want
to develop:

```bash
cd /path/to/project
lima-debian-ctl init
```

Edit `.devvm.toml`:

```toml
schema_version = 1
project = "service-a"
instance = "devbox"

[[mounts]]
host = "."
guest = "/work/service-a"
writable = true

# Optional second folder, explicitly scoped to this project:
[[mounts]]
host = "../associated-library"
guest = "/work/associated-library"
writable = true

[environment]
resolver = ["./scripts/resolve-dev-env"]
timeout_seconds = 60
```

The resolver runs on the host with the project directory as its working
directory. It must emit UTF-8 literal `KEY=value` records on stdout (CR, LF, and
CRLF accepted). Values may contain `=` and spaces; do not emit shell `export`
prefixes, comments, or multiline values. The controller does not evaluate the
raw output. Use an executable script or specify its interpreter explicitly.
The resolver runs without a terminal and is killed at the timeout, so it must
not prompt (unlock 1Password or sign in beforehand; the script only reads).

Omit `resolver` if the project needs no environment variables. Keep secret values
out of `.devvm.toml`, script arguments, and the project checkout. Review a
project's resolver before running it: it executes host code.

For port forwards, including guest access to a host SOCKS service, see the
[controller configuration reference](lima-debian-ctl/README.md#configure-a-project).

## 5. Activate and develop

```bash
lima-debian-ctl plan
lima-debian-ctl activate
lima-debian-ctl status
lima-debian-ctl shell
# Or run a command directly:
lima-debian-ctl exec -- ./gradlew test
```

Inspect the plan before approving. Activation asks for confirmation whenever it
replaces the VM's mounts (even if the VM is stopped) or restarts it. It replaces the VM's
configured mounts and automatic port forwards with the active project's scope.
It can stop the VM and terminate open shells, IDE sessions, and containers.
Only configured TCP tunnels are exposed, plus Lima's management SSH connection.

Run any project mise setup manually inside the VM. The controller does not run
mise install/trust/tasks or modify the project's mise configuration.

For IntelliJ Remote Development, activate first and start a fresh backend.
Shell hooks cover common launch paths; if the IDE bypasses them, use an explicit
environment-aware backend launch through `lima-debian-ctl exec`. The exact IDE
workflow still needs a real integration test; do not assume an already-running
backend adopts refreshed credentials.

## 6. Refresh, switch, and stop

```bash
# Same project: replace the environment file without restarting the VM.
lima-debian-ctl provision

# Another project: configure it and activate from its directory.
cd /path/to/another-project
lima-debian-ctl init
# Edit .devvm.toml, then:
lima-debian-ctl plan
lima-debian-ctl activate

# Remove this project's environment/tunnels and stop the shared VM:
lima-debian-ctl deactivate
```

A project switch restarts the VM so old processes and mounts do not remain
active. The disk, tool installations, caches, and Docker images/volumes survive.

Credential files persist across SSH disconnects, but orderly guest shutdown
removes them. An unclean stop can leave them on disk until startup cleanup.
Existing processes retain inherited variables until restarted. Deleting a file
is not secure erasure; snapshots/backups can preserve it.

If activation fails, inspect `status`, resolve the reported problem, and retry
`activate --yes` after reviewing `plan`. A project is not reported ready until
mounts, credentials, and tunnels have passed their checks.
