# Plan: project-scoped development on one reusable Lima VM

Status: Go implementation added in `lima-debian-ctl/`; real-VM acceptance checks
and IntelliJ workflow verification remain pending. See that directory's README
for the implemented command names and initial-version limitations.

## Requirements

- Reuse an existing provisioned Lima VM and its guest disk across projects.
- Expose only the active project's explicitly declared host directories (normally one or two), not workspace roots or the host home.
- Stop/edit/start the VM when switching projects or changing its mount set; Lima 2.2.1 rejects editing a running instance.
- Resolve the active project's environment on the host using a configurable executable. Accept CR-, LF-, and CRLF-delimited `KEY=value` output.
- Render safely quoted environment exports on the host and stream a persistent guest environment script over SSH. On-disk persistence is accepted.
- Make credentials available to new supported shells and explicitly launched development processes. Do not promise that arbitrary IDE launch paths inherit them without testing.
- Remove credentials during orderly guest shutdown and explicit deactivation; clear stale files at startup after an unclean shutdown.
- Manage only the active project's declared port forwards.
- Never run project mise installation, trust, tasks, or edit project mise configuration.

## Proposed stack

A Go module and binary, `lima-debian-ctl/` / `lima-debian-ctl`. Use synchronous
command orchestration and the Go standard library
for CLI parsing, subprocesses, JSON, locking, and tests, with go-toml/v2 as the
only runtime library dependency. Use ordinary OpenSSH and installed `limactl`
subprocesses, not a Lima SDK or embedded SSH library.

Use a small JSON state file per instance under the host's application state/data
directory. One active project does not justify SQLite, a web server, or a
resident controller daemon. OpenSSH owns persistent tunnel connections.

Use guest Bash loading helpers and a systemd lifecycle service installed through
the Ansible setup in `lima-debian-provision/`. Keep credential parsing, quoting,
and forwarding behavior covered by Go tests; no cross-language shared code is
needed.

## Configuration

A committed `.devvm.toml` identifies the project and declares mounts, forwards,
and a resolver argv array. The initial implementation keeps settings in that
file; a host user-configuration override layer is deferred. Relative host paths
and resolver working directory are
resolved against the directory containing `.devvm.toml`, never the caller's cwd.
No secret values belong in either configuration file.

```toml
schema_version = 1
project = "service-a"
instance = "devbox"

[[mounts]]
host = "."
guest = "/work/service-a"
writable = true

[[mounts]]
host = "../shared-library"
guest = "/work/shared-library"
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

Forward listeners default to loopback. `host-to-guest` is also supported.
Initially support TCP SSH forwarding; do not silently expose a Docker daemon
on unauthenticated TCP. Docker Unix-socket/SSH transport can be a later feature.

Treat project config/resolvers as executable trust: activation runs host code.
Canonicalize mount sources and validate destination overlap, reserved guest
paths, duplicate forwards, and undeclared existing Lima mounts. Never silently
retain a broad home/workspace mount. Initial adoption of the VM's mount and
forward configuration requires explicit confirmation and a non-secret backup.

## CLI interface

- `lima-debian-ctl init`: create a non-secret project configuration stub.
- `lima-debian-ctl plan`: show the intended mounts, forwards, and lifecycle changes without running the resolver or modifying the VM.
- `lima-debian-ctl activate [--yes]`: reconcile mounts and project environment/tunnels; restart only where required.
- `lima-debian-ctl provision`: refresh the active project's environment, without mount changes or a VM restart.
- `lima-debian-ctl shell`: enter the active project's guest directory with its environment.
- `lima-debian-ctl exec -- COMMAND...`: execute a command with the active project's environment and preserve argument/exit-code semantics.
- `lima-debian-ctl deactivate`: remove credentials, stop managed tunnels, and stop the VM so old project mounts/processes are no longer active.
- `lima-debian-ctl stop`: the same safe shutdown behavior for the managed instance.
- `lima-debian-ctl status`: show actual VM state, selected/ready project, drift, and tunnel state; never show environment values.

Non-interactive mutations require explicit restart/takeover approval through
flags where necessary. JSON output contains only non-secret structured results.

## Activation implementation

1. Acquire the per-instance mutation lock. Discover actual Lima state via `limactl list --json`; do not trust host state alone.
2. Validate config, paths, mount scope, port syntax, and guest identity. Explain required interruption. Obtain confirmation before resolver execution or mutation when switching projects.
3. Run the resolver once from the project directory, with timeout/output limits. Parse/validate the complete output before replacing any guest file. Hold values in memory only on the controller; do not log stdout or raw stderr.
4. If switching project or mount set: invalidate old readiness, remove the old environment if reachable, terminate managed tunnels, stop the VM, update mounts using `limactl edit` argument arrays, then start and await SSH. Do not edit the live YAML behind Lima's back.
5. Reconcile only the explicitly authorized mounts and forwarding policy. Preserve unrelated CPU/memory/container settings. Account for Lima's automatic port forwarding: managed operation must not unintentionally expose arbitrary guest ports or conflict with explicit SSH forwards.
6. Discover guest home/UID/group from the guest. Stream generated shell-quoted exports to a temporary file beside the persistent guest destination, set restrictive ownership/permissions, and rename atomically. Store a non-secret project marker alongside it.
7. Establish dedicated project tunnel connections using Lima's generated SSH config, `ExitOnForwardFailure`, keepalives, and private per-instance control sockets. Do not mutate Lima's generated SSH config or master connection. Forward setup must be checked, not assumed successful because ssh was spawned.
8. Check the environment-loading path with non-secret sentinel variables. Record project readiness only after mounts, environment, and tunnel setup succeed. Values are not written into the host state file.
9. On failure, mark activation incomplete, stop new managed tunnels, and remove newly provisioned credentials where reachable. Do not automatically restore the previous project's secret file. Keep enough non-secret operation state to diagnose/reconcile on the next command.

Same-project refresh does not restart the VM when mount configuration is
unchanged. Removing keys replaces the whole environment file; existing processes
retain inherited values until restarted.

## Environment implementation

Use a guest path outside all mounts, such as
`<discovered-home>/.config/lima-debian-ctl/environment.sh`, inside a restrictive directory.
Developer ownership, directory 0700, file 0600; atomic replacement. No secrets in
Lima YAML/cidata, command arguments, configuration backups, or controller logs.

Resolver contract: UTF-8, split CR/LF/CRLF records, skip empty records, first `=`
separates name/value, validate shell identifiers, preserve literal value text
including additional equals signs and surrounding spaces, reject duplicates,
malformed records, NUL bytes, and multiline values. No shell expansion or raw
`source` of resolver output. Quotes in input values are literal characters, not
dotenv syntax. Set explicit size/time limits. Report invalid line numbers, not
secret-bearing lines. Resolver failure leaves the existing active environment
unchanged for an ordinary refresh.

Install idempotent Bash/zsh hooks that load the generated script if present,
without replacing existing personal configuration. Test login, interactive, and
relevant SSH non-interactive launch paths separately. Provide an explicit guest
execution wrapper for commands that bypass shell initialization.

A running IntelliJ backend or Gradle daemon does not adopt refreshed variables.
Document/test the actual JetBrains Remote Development launch workflow before
claiming seamless integration; provide an explicit environment-aware backend
launch command through `lima-debian-ctl exec` where needed. Generic profile.d sourcing
alone is not the acceptance criterion.

A guest systemd oneshot service remains active until shutdown and removes the
generated environment at shutdown. Its startup path clears stale state after
an unclean stop. Lifecycle cleanup is installed by Ansible before project use,
not installed afresh by each project. No secure-erasure or forced-poweroff
immediate-deletion guarantee. Startup readiness must wait for stale cleanup.

## Internal module seams

- Config: validated desired project specification.
- Environment: resolver execution, strict parsing, safe rendering.
- Lima: discover, stop, edit, start, readiness through subprocess calls.
- Guest session: SSH execution, atomic environment installation/removal, project execution.
- Tunnels: connect/check/close dedicated OpenSSH connections.
- Activation: compute changes and perform recoverable state transitions.

Keep these internal; the caller-facing interface remains the CLI above. Inject
subprocess execution into orchestration for fake-driven tests rather than
introducing a generalized multi-hypervisor framework.

## Delivery sequence

1. Go CLI/config skeleton, config validation, plan/status, instance locking and state model. Fake limactl/ssh test harness.
2. Project-scoped mount reconciliation, stop/edit/start, startup readiness, takeover confirmation and failure recovery. Real two-project switching check.
3. Resolver parsing/rendering, SSH provisioning, guest loaders and systemd cleanup via Ansible. Environment refresh and removal tests.
4. Project SSH tunnels, conflict detection, stale socket cleanup, shell/exec commands.
5. End-to-end tests, IntelliJ workflow verification, documentation, installation recipe. No web interface, VM cloning, automatic project tool setup, or custom hot-mount mechanism.

## Acceptance checks

- Switch A to B: A's folders are absent, B's one/two folders are mounted, old guest processes have terminated, B's environment contains no removed A-only keys.
- Same-project refresh: no VM restart, removed keys disappear from the file and fresh processes, existing processes are documented as unchanged.
- CR/LF/CRLF, empty values, spaces, equals signs, shell metacharacters, duplicate/invalid names, resolver failure/timeout/oversized output are covered.
- New supported shells, explicit exec, and the agreed IntelliJ launch path see the expected environment without exposing values in test logs.
- Orderly stop deletes the environment; simulated unclean stop is followed by startup cleanup before readiness.
- Port conflicts fail visibly, project switch removes old managed forwards, and no broad unintended forwarding remains.
- Interrupted activation, stale host state, manual Lima stop/start, and concurrent commands are reconciled safely.
- Go controller and Python inventory tests pass without a real VM. No automatic project mise commands run.

## References

- `lima-debian-ctl/README.md` for the controller interface and operational constraints.
- `lima-debian-provision/README.md` for base guest provisioning and helper installation.
- `docs/research/lima-project-environment.md` for Lima version-specific findings and primary sources.
