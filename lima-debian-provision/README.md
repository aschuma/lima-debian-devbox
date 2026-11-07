# lima-debian-provision

The one-time setup for the shared Lima dev VM. An Ansible playbook turns an
existing Debian 12 ARM64 Lima VM into a dev box with Docker, a mise toolchain,
zsh with Starship, and the guest helpers that
[`lima-debian-ctl`](../lima-debian-ctl/README.md) needs.

Run it once after creating the VM. Run it again when tool versions change. The
day-to-day project switching is done by the controller, not by this playbook.
See the [top-level README](../README.md) for the overall picture.

## What it expects

- A running Lima instance. Default name: `devbox`.
- A user in the guest with passwordless sudo and `/usr/bin/python3`. Default
  name: `aschuma`. Lima creates such a user by default.

The playbook does not create users, change SSH, start or stop the VM, or
mount anything. It reads the guest home and group from `getent`, so a Lima
home like `/home/aschuma.guest` is fine.

## Run

```bash
./lima-debian-provision/provision.sh --install-deps   # Ansible collections
./lima-debian-provision/provision.sh --preflight      # facts and checks only
./lima-debian-provision/provision.sh                  # apply
```

The wrapper takes the SSH config from `limactl list --json` and builds a
temporary inventory under `.runtime/`. Extra arguments go to
`ansible-playbook`, for example `--tags`, `--check --diff` or `-e`.

Applying changes the guest. Besides packages it writes `~/.zshenv`,
`~/.zshrc` and `~/.config/mise/config.toml`, and adds lines to `~/.bashrc`.
Put personal zsh settings in `~/.zshrc.local`. Starship runs with its default
prompt. A sample config is installed as `~/.config/starship.toml.example`;
copy it to `~/.config/starship.toml` to use it.

## Settings

Set these as environment variables, or in a gitignored `provision.env`
(see `provision.env.example`).

| Variable | Default | Meaning |
| --- | --- | --- |
| `LIMA_INSTANCE` | `devbox` | instance to provision |
| `LIMA_USER` | `aschuma` | existing guest user |
| `INSTALL_DOCKER` | `true` | Docker CE + Compose |
| `CONFIGURE_SHELL` | `true` | zsh, Starship, completions, login shell |
| `UPGRADE_PACKAGES` | `false` | `apt dist-upgrade` first |

## Roles and tags

The roles run in this order:

`common-tools` → `docker` → `mise` → `runtimes` → `cli-tools` → `jdks` →
`shell` → `project-session` → `verify`

Preflight always runs. Tags can be rerun on their own, but later roles assume
earlier ones have been applied.

```bash
./lima-debian-provision/provision.sh --tags mise,runtimes,cli-tools,verify
```

### Tool versions

Tool versions live in `group_vars/all/tool_versions.yml`: Java, Node, Python,
GraalPy, Gradle, Maven, Kotlin, gh, aws, terraform, uv, ruff, just, Pi and
more. mise skips versions that are already installed. It does not remove tools
you drop from the list. `latest` moves; pin versions if you need
reproducibility. `MISE_GITHUB_TOKEN` from the host is passed to `mise install`
to avoid rate limits.

mise trusts only the guest home by default. For project files elsewhere:

```bash
./lima-debian-provision/provision.sh -e '{"mise_trusted_config_paths":["/home/aschuma.guest","/work"]}'
```

### JDKs for IntelliJ

`jdks` creates `~/.jdks` with symlinks `mise-<version>` to every mise-installed
JDK. IntelliJ looks there for JDKs. A justfile in that directory makes the
links. After installing another JDK, run `just -f ~/.jdks/justfile` in the
guest to refresh them.

### Guest helpers for the controller

`project-session` installs what `lima-debian-ctl` needs in the guest:

- `/usr/local/bin/lima-debian-ctl-guest`, the helper the controller calls over
  SSH,
- shell loaders that source the controller's env file,
- a systemd unit that deletes the env file at shutdown and boot.

The loaders and the helper also export
`SSH_AUTH_SOCK=/run/host-services/ssh-auth.sock` when that socket exists. The
socket exists when the controller has enabled Lima's agent forwarding
(`forward_agent` in `.devvm.toml`, default on). No private keys are installed
in the guest.

This role is part of a full run. It can also be applied alone:

```bash
./lima-debian-provision/provision.sh --tags project-session
```

## Docker

Lima's rootless containerd and nerdctl in `/usr/local/bin` stay as they are.
The `docker` role adds Docker CE as a system daemon next to them. The two have
separate images and containers. If Debian's `docker.io`, `containerd` or
`runc` packages are present, the role stops with a message instead of removing
them.

The user is added to the `docker` group, which is root-equivalent. Existing
sessions need a new login, or `newgrp docker`.

Java Testcontainers works inside the VM against `/var/run/docker.sock`. No
`DOCKER_HOST`, no socket forwarding. To prove it:

```bash
./lima-debian-provision/provision.sh --tags testcontainers-smoke
```

This copies `tests/testcontainers/` into the guest and runs Maven as the user.
It starts nginx through Testcontainers with Ryuk enabled, hits the mapped port,
and checks that the container is gone afterwards. It pulls images and is not
part of a normal run.

## Checks without a VM

```bash
just test       # includes the inventory tests
just lint       # shellcheck, yamllint, ansible-lint
just validate   # playbook syntax
```

After a real run, run the playbook a second time. The recap should report no
changes.
