# Lima Debian development VM

Use one reusable **Debian 12 ARM64 Lima VM** for development, exposing only the
active project's declared host folders and provisioning its associated
environment. No host-home mount is required.

## Two separate responsibilities

| Directory | Purpose |
| --- | --- |
| [`lima-debian-provision/`](lima-debian-provision/README.md) | Ansible base provisioning: Docker, mise-managed tools, shell configuration, and project-session helpers |
| [`lima-debian-ctl/`](lima-debian-ctl/README.md) | Go controller: project mounts, credentials, SSH tunnels, activation, and sessions |

Base provisioning targets an **existing running VM and existing user**. It does
not create accounts, change SSH authentication, or manage VM lifecycle.

The controller reuses the guest disk, installed tools, caches, and Docker state.
It exposes only the active project's explicitly declared folders. Switching
projects uses **stop → replace mounts → start**; refreshing credentials for the
same active project does not require restarting the VM. The controller does
**not** run project mise installation, trust commands, or tasks.

## Getting started

See **[QUICKSTART.md](QUICKSTART.md)** for prerequisites and first-time setup.

```bash
# Prepare an existing running Debian 12 ARM64 Lima instance:
./lima-debian-provision/provision.sh --install-deps
./lima-debian-provision/provision.sh --preflight
./lima-debian-provision/provision.sh

# Build/install the controller on the host:
just install

# In a project directory:
lima-debian-ctl init
# Edit .devvm.toml: mounts, credential resolver, and optional forwards.
lima-debian-ctl plan
lima-debian-ctl activate
lima-debian-ctl shell
```

Defaults for base provisioning are instance `devbox` and user `aschuma`.
Override `LIMA_INSTANCE` and `LIMA_USER` for another existing instance/account.
The project configuration selects the controller's instance independently.

## Project configuration

Commit a `.devvm.toml` containing non-secret configuration:

```toml
schema_version = 1
project = "service-a"
instance = "devbox"

[[mounts]]
host = "."
guest = "/work/service-a"
writable = true

[environment]
resolver = ["./scripts/resolve-dev-env"]
timeout_seconds = 60

[[forwards]]
direction = "guest-to-host"
listen_port = 1080
target_host = "127.0.0.1"
target_port = 1080
```

Additional mounts and host-to-guest forwards are supported. Paths resolve
relative to the project configuration. The first mount is the working directory
for `shell` and `exec`.

The resolver runs **on the host** and emits literal `KEY=value` records, accepting
CR, LF, and CRLF line endings. Values are safely quoted and transferred over SSH
to a developer-owned guest file with mode `0600`, outside the mounted folders.
Credential values stay out of controller logs, JSON state, and Lima configuration.

New supported shells load the file; `exec` provides an explicit loading path.
Existing IDE backends/build daemons need restarting to inherit refreshed values.
The guest cleanup service deletes the file during orderly shutdown and clears
stale files at startup after an unclean stop. On-disk persistence is intentional;
this is not secure erasure or isolation between mutually untrusted projects.

Read the [controller reference](lima-debian-ctl/README.md) for parsing rules,
permissions, forwarding policy, recovery, and IntelliJ launch integration.

## Host development commands

Requires Go 1.23+, Python 3, and the tools appropriate to each recipe. Run
`just --list` to see which commands modify the guest.

```bash
just test       # Go race tests/vet/format check and Python inventory tests; no VM
just build      # Build lima-debian-ctl/bin/lima-debian-ctl
just install    # Install the Go controller into GOBIN or GOPATH/bin
just deps       # Install Ansible collections locally
just validate   # Ansible syntax check using localhost inventory; no VM
just lint       # ShellCheck, YAML lint, and Ansible lint
```

Go tests run on macOS and Linux in CI. Provisioning validation runs independently
without a VM. Starting containers with the Java Testcontainers smoke test is an
explicit operation, never part of ordinary tests:

```bash
./lima-debian-provision/provision.sh --tags testcontainers-smoke
```

## Validation status

The controller has fake-VM lifecycle/CLI tests, environment parsing and quoting
tests, process cancellation/output-limit tests, locking, and tunnel checks.
Its mount-edit expression has been validated against a temporary Lima 2.2.1
configuration. Actual project switching, guest shutdown cleanup, and the exact
IntelliJ remote-development workflow still require an explicit real-VM
acceptance run.

Design details: [implementation plan](docs/devvm-plan.md) and
[Lima lifecycle research](docs/research/lima-project-environment.md).

## License

[MIT](LICENSE).
