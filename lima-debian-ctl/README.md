# lima-debian-ctl

A small Go controller for **one active project on a reusable Lima VM**. The VM's
disk, installed tools, caches, and Docker state survive project switches, but
only the active project's explicitly declared host folders are mounted.

Uses installed `limactl` and OpenSSH, not a daemon or a Lima SDK. The only Go
runtime dependency is `github.com/pelletier/go-toml/v2` for TOML decoding.
Supports macOS/Linux hosts with Go 1.23+ for building; the built binary requires
no Go installation. Base guest provisioning lives in `../lima-debian-provision/`.

**Validation status:** automated tests cover orchestration with a fake VM.
A real-VM acceptance run on Lima 2.2.1 verified: takeover and project switch
(stop/edit/start, ~12-15 s), exclusive mounts incl. read-only, literal
credential values via `exec`, plain `bash -l` and `zsh -c`, exit-code
propagation, guest-to-host tunnel traffic, config-change detection, same-project
refresh without restart, deactivate, and credential removal by the guest systemd
service on both controller-driven and plain `limactl stop`. The IntelliJ
Remote Development launch path has not been exercised yet.

## Install and prepare the guest

```bash
# Host, from this directory:
go test ./...
go install .
# Ensure $(go env GOPATH)/bin is on PATH, unless you configured GOBIN.

# From the repository root, APPLY guest loading/cleanup helpers once:
./lima-debian-provision/provision.sh --tags project-session
```

The Ansible command changes the guest: it installs a helper, shell loaders, and
a systemd credential-cleanup service. The VM must already exist and be running.
The controller never creates, clones, deletes, or rebuilds a VM.

The helper service clears any controller-generated environment when first
started, at orderly shutdown, and at subsequent guest startups. Applying the
role again while the service is already active does not restart the service.

## Configure a project

```bash
cd /path/to/project
lima-debian-ctl init
```

Edit `.devvm.toml`; commands discover it by walking upward from the current
directory, or accept `--config /absolute/path/.devvm.toml`.

```toml
schema_version = 1
project = "service-a"
instance = "devbox"

# The first mount is the working directory for shell/exec.
[[mounts]]
host = "."
guest = "/work/service-a"
writable = true

[[mounts]]
host = "../associated-library"
guest = "/work/associated-library"
writable = true

[environment]
resolver = ["./scripts/resolve-dev-env"]
timeout_seconds = 60

# Reach a macOS SOCKS service from inside the VM:
[[forwards]]
direction = "guest-to-host"
listen_port = 1080
target_host = "127.0.0.1"
target_port = 1080

# Reach the guest application from macOS:
[[forwards]]
direction = "host-to-guest"
listen_port = 8080
target_host = "127.0.0.1"
target_port = 8080
```

Paths resolve against the configuration directory; host mount sources are
canonicalized. Guest mount destinations must be non-overlapping paths below
`/work/`. The host home, its ancestors, and overlapping mount sets are rejected.
Only explicit paths are shared; do not declare a broad workspace directory if
you want project-only access. Unknown configuration fields are rejected.

The resolver is an argv array, not an implicitly evaluated shell string. It
runs on the host from the project directory and inherits the host environment.
Use an executable script, or `resolver = ["/bin/bash", "./scripts/dev-env"]`.

**The resolver must be non-interactive.** It receives no terminal and an empty
stdin, and is killed (with its process group) at the timeout. Tools that prompt
for biometrics, a passphrase, or `op signin` will hang or fail: establish such
sessions beforehand (for example a 1Password desktop-app integration or a
service-account token in the host environment) so the resolver only reads.
The project configuration is trusted executable input: inspect it before
activation, just as you would a project script. Do not put secret values in
configuration or resolver arguments. Omit the resolver to provision an empty
project environment (apart from the controller's project marker).

There is no global configuration layer in this initial version: the instance
and project settings are in `.devvm.toml`.

## Workflow

```bash
lima-debian-ctl plan
lima-debian-ctl activate        # prompts before mounts are replaced or the VM restarts
# Non-interactive approval: inspect plan, then activate --yes
lima-debian-ctl status
lima-debian-ctl shell
lima-debian-ctl exec -- ./gradlew test
lima-debian-ctl provision       # refresh credentials without restarting the VM
lima-debian-ctl deactivate      # remove credentials/tunnels, stop the VM
```

Flags follow the command: `plan --json`, `activate --yes --json`, etc. Successful
non-session results support JSON; errors go to stderr with nonzero exit status.
`shell`/`exec` use native SSH stdio and preserve the remote exit code.

Switch projects by running `activate` from the next project's directory. With
Lima 2.2.1, mount changes require **stop → edit → start**. Changing projects also
restarts the VM even if the mount lists happen to be identical, terminating old
processes and their inherited environment. Activating the same ready project
with unchanged mounts/forwarding policy does not restart it.

Initial takeover replaces **all configured mounts and Lima automatic port
forwards**, not CPU, memory, or container settings. A private config backup is
saved before edits. Automatic guest-port forwarding is disabled so only the
project's declared TCP tunnels (plus Lima's management SSH connection) are
exposed. This is intentionally more restrictive than Lima's defaults.

SSH tunnels use a dedicated control socket and do not rewrite Lima's generated
SSH config or the user's `~/.ssh/config`. Listeners bind to loopback and require
unprivileged ports (1024–65535). Host-to-guest (`-L`) listeners fail activation
via `ExitOnForwardFailure` when the host port is taken; guest-to-host (`-R`)
listeners are verified on the guest with `ss` after the tunnel is up, because
sshd only warns when it cannot bind. Arbitrary
Unix-socket forwarding and unauthenticated Docker TCP exposure are not added.
Java Testcontainers inside the guest continues to use its local Docker socket.

The controller **never runs project mise install/trust/tasks** or edits the
project's mise configuration. Run tool setup manually. Your own shell hooks
may, of course, activate mise when you open a shell.

## Credential format and storage

Resolver stdout must be UTF-8 literal `KEY=value` records, separated by CR, LF,
or CRLF. Split is at the first `=`. Blank records are ignored; empty values,
additional equals signs, spaces, and quotes are preserved literally.

```text
ARTIFACTORY_USER=alice
ARTIFACTORY_TOKEN=some=value
EMPTY_VALUE=
```

This is not a dotenv/shell interpreter: comments, `export` prefixes, surrounding
quote removal, interpolation, and embedded multiline values are not supported.
Malformed records, duplicate names, NUL bytes, oversized output (over 1 MiB), and
invalid shell identifiers fail before replacing the current environment.
Shell startup/control names (`HOME`, `BASH_ENV`, `ENV`, `ZDOTDIR`, `IFS`),
read-only shell names (`SHELLOPTS`, `BASHOPTS`, `UID`, `EUID`, `PPID`,
`BASH_VERSINFO`, `status`, `pipestatus`), and the `LIMA_DEBIAN_CTL_` prefix are
reserved.
Resolver timeout defaults to 60 seconds (configurable from 1 to 600).

The controller renders quoted export statements and streams them over SSH to:

```text
<guest-account-home>/.config/lima-debian-ctl/environment.sh
```

The directory is `0700`, file `0600`, and installation uses a temporary file and
atomic rename. The guest account home comes from `getent`, not an assumption
about `/home/<user>`. Credential values are not placed in Lima YAML, controller
JSON state, process arguments, or controller logs. Resolver stdout/stderr are
withheld on failure; errors identify invalid line numbers, not record contents.

**Persistence is intentional.** The environment file is on the guest disk, not
baked into the base image or written into a host-mounted project directory.
Orderly shutdown/deactivation deletes it. A forced stop can leave it on disk
until startup cleanup; deletion is not secure erasure and snapshots/backups may
retain it. Do not snapshot a credential-bearing VM if that is unacceptable.

## Shells and IntelliJ

The guest role loads the file in login shells, Bash `.bashrc` before its usual
interactive-only guard, and `/etc/zsh/zshenv` when zsh is installed. It preserves
personal files and works even when base shell customization is disabled. A
non-interactive Bash launched outside SSH need not read these startup files;
use `lima-debian-ctl exec` for an explicit loading path.

`exec` and `shell` check project identity/readiness again inside the guest before
executing anything. Readiness also runs a login-shell probe (`bash -lc`) so a
broken loader is reported at activation rather than discovered in the IDE.
Both `shell` and a TTY-attached `exec` request a remote TTY, so Ctrl-C reaches
the remote command instead of only killing ssh; the host process ignores SIGINT
while a session is open. Credentials removed from the generated file are absent in
fresh SSH sessions. **Existing processes and their descendants can retain old
values**; merely deleting/replacing a file does not alter their environment.

For IntelliJ Remote Development, first activate the project, then start a fresh
remote backend. If the IDE bypasses shell loading, launch its backend through
`lima-debian-ctl exec -- <backend-command> <arguments...>` or configure the IDE's
launch wrapper accordingly. Your actual Gateway/backend workflow must be tested;
profile hooks alone are not a blanket guarantee. Restart existing backends and
Gradle daemons when refreshed values need to be inherited. `provision` does not
kill them automatically.

## State and recovery

Non-secret state, instance locks, and the most recent pre-edit Lima config are
under `${XDG_STATE_HOME:-~/.local/state}/lima-debian-ctl/<instance>/`.
The backup copies the existing Lima YAML verbatim; if it already contains
secrets from other provisioners, the backup contains those too. Permissions are
restrictive. Tunnel sockets are under `~/.lima-debian-ctl/sockets/`; unusually
long home paths exceeding OpenSSH socket limits are rejected.

Readiness tracks the project/configuration and guest boot ID. Manual Lima
restarts, mount drift, or dead tunnels invalidate readiness. `plan` never runs
the credential resolver or changes the VM (it may create private host state
directories). Mutations take a nonblocking per-instance lock. Shell sessions do
not hold that lock; a confirmed project switch can terminate them.

On an interrupted/failed activation, `status` may show `activating` or
`incomplete`; run `plan`, resolve the reported problem, then `activate --yes`.
The controller does not silently restore old credentials. Failed cleanup is
best effort when SSH is unavailable; the guest lifecycle service remains the
shutdown/startup fallback. If stopped at the wrong point, the VM can remain
running with an incomplete project, but no development session is declared ready.

## Validation

```bash
go test -race -cover ./...
go vet ./...
go build .

# From the repository root:
shellcheck lima-debian-provision/roles/project_session/files/lima-debian-ctl-guest \
  lima-debian-provision/roles/project_session/files/lima-debian-ctl.sh
./lima-debian-provision/provision.sh --syntax-check
```

Unit/CLI tests use fake VM operations; process tests cover cancellation, output
limits, and exit codes. They do not restart a real VM. Real acceptance testing
must explicitly verify two-project mount switching, supported shell modes,
shutdown/reboot credential removal, forwarding conflicts, and the agreed
IntelliJ launch workflow. This is project scoping, not isolation between
mutually untrusted projects: caches, guest files, Docker images/volumes, and
installed tools remain on the shared disk.
