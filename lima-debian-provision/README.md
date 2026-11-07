# lima-debian-provision

Ansible playbook that turns an existing Debian 12 ARM64 Lima VM into a dev
box: Docker, a mise toolchain, zsh with Starship, and the guest helpers for
[`lima-debian-ctl`](../lima-debian-ctl/README.md).

It expects a running instance (default `devbox`) and a user (default
`aschuma`) with passwordless sudo and `/usr/bin/python3`. It does not create
users, change SSH, start or stop the VM, or mount anything. The guest home
and group are read from `getent`, so a Lima home like `/home/aschuma.guest`
is fine.

## Run

```bash
./lima-debian-provision/provision.sh --install-deps   # Ansible collections
./lima-debian-provision/provision.sh --preflight      # facts and checks only
./lima-debian-provision/provision.sh                  # apply
```

The wrapper takes the SSH config from `limactl list --json` and builds a
temporary inventory under `.runtime/`. Any extra arguments go to
`ansible-playbook`, e.g. `--tags`, `--check --diff`, `-e`.

Applying changes the guest. Besides packages it writes `~/.zshenv`,
`~/.zshrc`, `~/.config/starship.toml`, `~/.config/mise/config.toml` and adds
lines to `~/.bashrc`. Put personal zsh settings in `~/.zshrc.local`.

Settings, as environment variables or in a gitignored `provision.env`:

| Variable | Default | |
| --- | --- | --- |
| `LIMA_INSTANCE` | `devbox` | instance to provision |
| `LIMA_USER` | `aschuma` | existing guest user |
| `INSTALL_DOCKER` | `true` | Docker CE + Compose |
| `CONFIGURE_SHELL` | `true` | zsh, Starship, completions, login shell |
| `UPGRADE_PACKAGES` | `false` | `apt dist-upgrade` first |

## Roles and tags

Order: `common-tools` → `docker` → `mise` → `runtimes` → `cli-tools` →
`jdks` → `shell` → `project-session` → `verify`. Preflight always runs. Tags can be
rerun on their own, but later roles assume earlier ones have been applied.

```bash
./lima-debian-provision/provision.sh --tags mise,runtimes,cli-tools,verify
```

Tool versions live in `group_vars/all/tool_versions.yml` (Java, Node, Python,
GraalPy, Gradle, Maven, Kotlin, gh, aws, terraform, uv, ruff, just, Pi, …).
mise skips versions that are already installed and does not remove tools you
drop from the list. `latest` moves; pin versions if you need reproducibility.
`MISE_GITHUB_TOKEN` from the host is passed to `mise install` to avoid rate
limits.

mise trusts only the guest home by default. For project files elsewhere:

```bash
./lima-debian-provision/provision.sh -e '{"mise_trusted_config_paths":["/home/aschuma.guest","/work"]}'
```

`jdks` creates `~/.jdks` with symlinks `mise-<version>` to every mise-installed
JDK, which is where IntelliJ looks for JDKs. The links are made by a justfile in
that directory; after installing another JDK run `just -f ~/.jdks/justfile`
in the guest to refresh them.

`project-session` installs `/usr/local/bin/lima-debian-ctl-guest`, the shell
loaders for the controller's env file, and a systemd unit that deletes that
file at shutdown and boot. It is part of a full run and can be applied alone.

## Docker

Lima's rootless containerd/nerdctl in `/usr/local/bin` stays as it is. The
`docker` role adds Docker CE as a system daemon next to it; the two have
separate images and containers. If Debian's `docker.io`, `containerd` or
`runc` packages are present the role stops with a message instead of removing
them.

The user is added to the `docker` group (root-equivalent). Existing sessions
need a new login, or `newgrp docker`.

Java Testcontainers works inside the VM against `/var/run/docker.sock`; no
`DOCKER_HOST`, no socket forwarding. To prove it:

```bash
./lima-debian-provision/provision.sh --tags testcontainers-smoke
```

This copies `tests/testcontainers/` into the guest, runs Maven as the user,
starts nginx through Testcontainers with Ryuk enabled, hits the mapped port and
checks the container is gone afterwards. It pulls images and is not part of a
normal run.

## Checks without a VM

```bash
just test       # inventory tests
just lint       # shellcheck, yamllint, ansible-lint
just validate   # playbook syntax
```

After a real run, run the playbook a second time: the recap should report no
changes.
