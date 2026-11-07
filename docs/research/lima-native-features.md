# Native Lima features for provisioning and controlling the dev VM

## Scope

This note surveys Lima configuration and CLI features that overlap with work
`lima-debian-ctl/` and `lima-debian-provision/` currently do by hand, and
records which of them are worth adopting. It is a design/decision record: it
documents and recommends, and (except where a change is marked *implemented*
below) does not itself change the controller, the playbook, or any VM.

Installed host version at the time of writing is Lima 2.2.1 (`limactl
--version`). Behavioural claims that matter for this installation should be
checked against 2.2.1; the citations below point to the live documentation
(`master`), which may describe later behaviour. Where a feature has a version
requirement, it is called out in the finding.

## Project context

The repository runs **one long-lived Debian 12 ARM64 Lima VM (`devbox`)** that
is reused across projects, rather than a VM per project. Two components share
it:

- `lima-debian-provision/` — Ansible run against an *already existing* VM. It
  installs Docker, a mise toolchain, zsh/Starship and the guest helpers. By
  design it never creates, starts, stops, or changes SSH access on the VM, and
  never puts secrets into Lima config (see
  `lima-project-environment.md`).
- `lima-debian-ctl/` — a Go CLI that scopes the shared VM to one project at a
  time. On `activate` it makes the project's folders the only mounts, its
  resolved credentials the only environment, and its declared forwards the only
  open tunnels. Switching projects stops the VM, rewrites mounts with `limactl
  edit`, and starts it again, because Lima 2.2.1 rejects editing a running
  instance.

Because the VM is shared and long-lived, two project constraints shape which
native features fit: (1) secrets must stay off the Lima template/cidata and off
the guest disk where possible, and (2) same-project credential refresh must not
require a reboot. Features are judged against those constraints below, not just
on capability.

## Current instance baseline (observed)

From `limactl list --json` for `devbox` on Lima 2.2.1. This is what the native
features would be turned on *against*:

| Setting | Current value | Relevance |
| --- | --- | --- |
| `vmType` | `vz` | Immutable after creation; virtiofs mounts, vzNAT available [7][8] |
| `arch` | `aarch64` | Matches the Debian 12 ARM64 target |
| `mountTypesUnsupported` | `["9p"]` | 9p excluded; virtiofs is the effective mount type [8] |
| `mountInotify` | `false` | IDE/build file-watching on host edits is **off** [8] |
| `ssh.forwardAgent` | `false` → **set by `forward_agent`** | Keys were copied to guest before; agent forwarding replaces that [4] |
| `ssh.loadDotSSHPubKeys` | `false` | Host `~/.ssh/*.pub` not auto-trusted |
| `plain` | `false` | Convenience features (mounts, forwarding, agent) are available [5] |
| `containerd.user` | `true` | Present but unused; the project uses Docker |
| `user.passwordlessSudo` | `true` | Preflight requirement already satisfied natively [6] |
| `user.home` / `shell` | `/home/aschuma.guest` / `/bin/bash` | Guest helpers assume a POSIX login shell |

## Findings

- Lima has **two independent native forwarders**, not one. `portForwards` in the
  instance config does typed guest→host forwarding (a guest loopback port
  exposed on a host loopback port); this matches the controller's
  `host-to-guest` direction. `limactl tunnel` is a separate SOCKS tunnel. The
  controller's reverse `guest-to-host` direction (`ssh -R`) has no native config
  equivalent. [1][2]
- `portForwards` rules carry a `static: true` flag. Static rules are the
  declarative way to pin a fixed set of forwards into the instance, instead of
  the controller's current "ignore everything, then run a side-band `ssh`
  process" approach. [1]
- Native `portForwards` are applied when the instance starts. Changing them
  requires a stop/edit/start cycle, because `limactl edit` rejects a running
  instance. The controller's custom `ssh -L/-R` tunnels can be changed on a
  running VM without a restart. This is a real tradeoff: declarative and robust
  versus hot-swappable. [1][3]
- `ssh.forwardAgent: true` forwards the host SSH agent into the guest; the
  socket is available to the guest user at `/run/host-services/ssh-auth.sock`.
  This lets private keys stay on the host instead of being copied onto the guest
  disk, which is what the removed `[[ssh_keys]]` feature used to do. [4]
- `ssh.forwardAgent` is disabled in `plain` mode, along with `mounts` and
  dynamic port forwarding. `devbox` is not plain, so agent forwarding is
  available; but a template must not set `plain: true` if these features are
  wanted. [5]
- Passwordless sudo (`user.passwordlessSudo: true`) is the built-in default for
  cloud-init guests (QEMU/VZ) since Lima 2.2, and `devbox` already has it. The
  provisioning preflight requires passwordless sudo; a template-created VM
  satisfies it without manual setup. Note that `param.env` is read via `sudo`
  inside the guest, so it is affected if passwordless sudo is turned off. [6]
- `vmType` can only be set when the instance is created and cannot be changed
  afterwards. `devbox` is VZ; any template used to recreate it must fix
  `vmType: vz` up front. [7]
- The default mount type for VZ is `virtiofs`, and `devbox` already excludes 9p.
  `mountType` can be set explicitly in config to avoid relying on the resolved
  default. [8]
- `mountInotify: true` forwards host→guest inotify events, and applies only to
  writable mounts. `devbox` has it `false`. This is the native mechanism for
  making IDE/build file watching react to host edits on the writable source
  mount. It is marked experimental and has caveats (e.g. 9p nested files; file
  removals do not trigger events). [8]
- `portForwards` supports `guestSocket`/`hostSocket` with `reverse: true` for
  Unix-socket forwarding. This is the native path for the Docker-socket / SSH
  transport feature deferred in `docs/devvm-plan.md`. [1]
- `limactl shell --preserve-env`, together with `LIMA_SHELLENV_ALLOW` /
  `LIMA_SHELLENV_BLOCK` (Lima 2.0+), propagates selected host environment
  variables into the guest per invocation, with nothing written to the guest
  disk. It only affects that `shell`/`exec` invocation, so it does not reach an
  IDE backend or build daemon launched outside the controller — the same
  limitation the controller already documents for its env file. [9]
- `limactl snapshot` and `limactl protect`/`unprotect` exist as subcommands,
  giving a real rollback point and accidental-deletion guard around edits, where
  the controller currently only copies `lima.yaml` (`BackupConfig`). [10]
- Named managed networks (`limactl network create`, `networks: [{lima: NAME}]`)
  and `vzNAT` give the guest a routable IP. These are out of scope for a
  loopback-only dev VM but are the escape hatch if IP-level guest access is ever
  required. [2]

## Proposal

### Tier A — adopt (clear wins, low risk)

1. **[implemented]** `ssh.forwardAgent`: the controller's `limactl edit`
   expression now sets it from a new top-level `forward_agent` flag in
   `.devvm.toml` (default `true`), and the setting is part of the
   instance-matches-config check so flipping it plans an edit+restart like a
   mount change. The guest login loader and the `exec`/`shell` helper export
   `SSH_AUTH_SOCK=/run/host-services/ssh-auth.sock` only when that socket
   exists. The former `[[ssh_keys]]` feature, which copied private keys onto
   the guest disk, was removed entirely (config, `sshkeys.go`, guest
   `ssh-key`/`ssh-clear` subcommands); previously copied keys are to be
   removed from the guest by hand. [4]
2. Pin durable, non-secret defaults (`mountType: virtiofs`, `ssh.forwardAgent`)
   so they survive recreation — in the instance config and/or
   `$LIMA_HOME/_config/default.yaml`. [8]
3. Use `limactl snapshot` / `limactl protect` around the stop/edit/start cycle,
   augmenting or replacing `BackupConfig`. [10]

### Tier B — evaluate (real tradeoffs)

4. `mountInotify: true` on the writable source mount for IDE/build file
   watching, gated on testing because it is experimental. [8]
5. Native `portForwards` with `static: true` for the `host-to-guest` subset only
   if restart-to-change-forwards is acceptable; otherwise keep the custom
   tunnels, which are justified by no-restart refresh. Splitting forwarding
   between native (forward) and custom (reverse) is the cost. [1][3]
6. A `--preserve-env` env mode as an opt-in `.devvm.toml` switch: secrets never
   touch the guest disk for `shell`/`exec`, at the cost of not reaching
   externally launched IDE backends. [9]

### Tier C — note for later

7. `guestSocket`/`hostSocket` reverse forwarding as the native implementation of
   the deferred Docker-socket feature. [1]
8. `vzNAT` / managed networks only if IP-level guest access is ever needed. [2]

### Stays custom (correctly)

- Per-project mount swapping via stop/edit/start: Lima has no hot mount-swap and
  `limactl edit` still rejects a running instance. [3]
- Reverse `guest-to-host` (`ssh -R`) forwards: no native config equivalent. [1]
- Keeping resolved secrets out of the Lima template/cidata, per
  `docs/research/lima-project-environment.md`.

## Primary sources

1. Lima docs, Port Forwarding: https://lima-vm.io/docs/config/port/
2. Lima docs, Network: https://lima-vm.io/docs/config/network/
3. Lima docs, Configuration guide (and `limactl edit` behaviour): https://lima-vm.io/docs/config/
4. Lima default template, `ssh.forwardAgent`: https://github.com/lima-vm/lima/blob/master/templates/default.yaml
5. Lima docs, Plain mode: https://lima-vm.io/docs/config/plain/
6. Lima docs, Sudo: https://lima-vm.io/docs/config/sudo/
7. Lima docs, VM types: https://lima-vm.io/docs/config/vmtype/
8. Lima docs, Filesystem mounts: https://lima-vm.io/docs/config/mount/
9. Lima docs, Environment Variables: https://lima-vm.io/docs/config/environment-variables/
10. Lima command reference (`snapshot`, `protect`, `tunnel`): https://lima-vm.io/docs/ (Command Reference)
