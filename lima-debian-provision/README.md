# Provision an existing Debian 12 Lima VM

Ansible provisioning for an existing Lima development VM. This folder contains
the base toolchain, Docker setup, shell configuration, project-session helpers,
and provisioning tests. Project activation is handled separately by the Go
controller in [`../lima-debian-ctl/`](../lima-debian-ctl/README.md).

## Assumptions and safety

- An existing **running Debian 12 ARM64** Lima instance, default `devbox`.
- An existing SSH-accessible developer account, default `aschuma`, with
  passwordless sudo and `/usr/bin/python3` already installed.
- **No host-home mount or other shared filesystem is required.** Ansible
  transfers files over SSH. Guest home and primary group come from `getent`, not
  from host paths or `/home/<user>` assumptions. On the current instance the
  guest home is `/home/aschuma.guest`.
- No VM creation/start/stop, account creation/password/key changes, SSH server
  changes, mounts, image compaction, cache deletion, or MOTD replacement.
- Debian distribution upgrades are disabled by default.

Provisioning is not read-only: it installs apt packages and mise tools, configures
Git defaults and shell activation, and (by default) installs Docker and changes
the existing user's login shell to zsh. It manages `.zshenv`, `.zshrc`, and
`.config/starship.toml`; **back up existing customizations before applying**.
Use `.zshrc.local` for personal zsh additions. mise's global config is also
managed here. Roles add mise/Starship activation lines to `.bashrc`.

## Quick start

On macOS, install Ansible and ensure Lima and Python 3 are available:

```bash
brew install ansible

# From the repository root:
./lima-debian-provision/provision.sh --install-deps
./lima-debian-provision/provision.sh --print-target
./lima-debian-provision/provision.sh --syntax-check
./lima-debian-provision/provision.sh --preflight

# APPLY: installs tools/Docker and configures the guest shell.
./lima-debian-provision/provision.sh
```

The wrapper works from any directory. It obtains the generated SSH config and
host alias from `limactl list --json`; it does not hard-code ports or keys,
rewrite your host SSH config, or use deprecated `limactl show-ssh`.

Collections are installed under this directory's ignored `.ansible/collections`.
Temporary inventory lives under ignored `.runtime/` and is removed on exit.
`--install-deps`, `--print-target`, `--syntax-check`, and `--list-tags` do not
connect to the guest. `--preflight` only gathers facts and checks prerequisites
(it uses Ansible's normal temporary remote files).

If Python or passwordless sudo is absent, preflight fails; repair those
prerequisites explicitly rather than having provisioning recreate the account.

## Configuration

Environment variables override the built-in defaults:

| Variable | Default | Meaning |
| --- | --- | --- |
| `LIMA_INSTANCE` | `devbox` | Existing running Lima instance |
| `LIMA_USER` | `aschuma` | Existing guest SSH/developer account |
| `INSTALL_DOCKER` | `true` | Install rootful Docker Engine and Compose |
| `CONFIGURE_SHELL` | `true` | Configure zsh, Starship, completions, login shell |
| `UPGRADE_PACKAGES` | `false` | Allow apt distribution upgrade |

Boolean values must be `true` or `false`. For local persistent settings, copy
`provision.env.example` to `provision.env` (gitignored). This is trusted shell
configuration, not a secrets file. The example preserves environment overrides.

```bash
LIMA_INSTANCE=another LIMA_USER=anotheruser \
  ./lima-debian-provision/provision.sh --preflight

CONFIGURE_SHELL=false ./lima-debian-provision/provision.sh
```

If selecting another account, that account must already have SSH access using
Lima's generated connection settings and passwordless sudo. The wrapper does
not provision credentials or change Lima's configured user.

Tool versions live in `group_vars/all/tool_versions.yml`, including Java, Node,
Python, GraalPy, build tools, cloud/AI CLIs, and `just`. Pi is installed through
mise's `pi` registry entry with version `latest` and verified with `pi --version`.
The GraalPy asset currently targets ARM64.

mise trusts only the discovered guest home by default. To trust projects
elsewhere, pass an explicit guest-local path list:

```bash
./lima-debian-provision/provision.sh \
  -e '{"mise_trusted_config_paths":["/home/aschuma.guest","/srv/projects"]}'
```

`MISE_GITHUB_TOKEN`, if exported on the host, is forwarded for mise installation
to avoid GitHub API rate limits. It is not written to the guest mise config.

## Incremental provisioning

```bash
./lima-debian-provision/provision.sh --tags common-tools,mise
./lima-debian-provision/provision.sh --tags runtimes,cli-tools
./lima-debian-provision/provision.sh --tags docker,shell
./lima-debian-provision/provision.sh --tags verify
```

Normal order: common-tools → Docker → mise → runtimes → cli-tools → shell →
verify. Preflight runs even for tagged operations. Partial runs assume their
prerequisites have already been applied: for example, shell completions require
the CLI tools, and developer-user tasks require `acl` from common-tools.

To update tool specifications, apply `mise` along with the relevant tool role:

```bash
./lima-debian-provision/provision.sh --tags mise,runtimes,cli-tools,verify
```

Re-running skips installed mise versions. `latest` is a moving specification;
use explicit versions for reproducibility. This does not uninstall tools removed
from the registry. Check mode (`--check --diff`) is advisory: fresh installations
cannot fully simulate missing tools, generated completions, or service checks.
`--diff` may print existing user configuration; review before sharing output.

## Docker, Lima containerd, and Java Testcontainers

The existing VM has Lima-managed **rootless containerd and BuildKit** under the
developer's user services, with binaries in `/usr/local/bin`. They remain intact.

Docker CE installs a separate **system** Docker daemon and packaged containerd.
The role does not remove distro packages, overwrite Lima binaries, or stop user
services. If conflicting Debian `docker.io`, `containerd`, or `runc` packages
exist, it fails with an explicit migration message instead of removing them.
Docker and nerdctl have separate image stores and container inventories.

The existing developer is added to the `docker` group. **Docker group access is
root-equivalent.** Ansible reconnects for the new group membership; open a new
login for existing terminals. Verify as the guest user, without sudo:

```bash
limactl shell devbox -- docker info
limactl shell devbox -- docker compose version
```

Run Java builds/tests **inside the VM**. Testcontainers uses the guest's local
`/var/run/docker.sock`; no host socket forwarding or `DOCKER_HOST` setting is
necessary. Remove stale Docker/Podman environment overrides from your project
if discovery selects the wrong runtime. Keep Ryuk enabled for normal cleanup.
Old Testcontainers versions may be incompatible with newer Docker API minimums;
upgrade your project's Testcontainers dependency if needed. The smoke project
pins Testcontainers 2.0.5.

Disabling `INSTALL_DOCKER` skips installation and Docker checks; it does not
uninstall an existing Docker setup. Java Testcontainers cannot use nerdctl's
containerd API directly.

### Explicit runtime smoke test

After successful provisioning:

```bash
./lima-debian-provision/provision.sh --tags testcontainers-smoke
```

This **downloads Maven dependencies and nginx/Ryuk images and runs containers**.
It is not part of ordinary provisioning, syntax validation, or host unit tests.
It copies only the smoke project's source/POM over SSH to a guest temporary
directory and runs Maven as the existing developer. Maven launches the smoke
class in a separate JVM (`exec:exec`), avoiding `exec:java`'s interruption of
Testcontainers' background threads during shutdown. It requires enabled Ryuk,
starts nginx through Testcontainers, checks HTTP through a mapped port, then
verifies the nginx container is removed. Guest project files are removed even
on failure. Maven/image caches are retained. The test exercises Ryuk startup;
it is not a test of cleanup after a forcibly killed JVM.

## Host validation

From the repository root:

```bash
bash -n lima-debian-provision/provision.sh
shellcheck lima-debian-provision/provision.sh
python3 -m unittest discover -s lima-debian-provision/tests -v
./lima-debian-provision/provision.sh --syntax-check

# Optional tools (uvx avoids installing into your system Python):
(cd lima-debian-provision && uvx --from yamllint yamllint .)
(cd lima-debian-provision && uvx --from ansible-lint ansible-lint playbook.yml)

# Compile smoke test only; does not contact Docker:
mvn -f lima-debian-provision/tests/testcontainers/pom.xml compile
```

Inventory tests cover defaults, overrides, SSH paths containing spaces, invalid
flags, missing instances/configs, and stopped-instance rejection. After applying
to a real VM, run provisioning a second time and review the recap for unexpected
changes. Installs, group changes, and shell edits have not been validated until
that apply and second run succeed.

## Project-scoped Lima controller

The separate Go controller in [`../lima-debian-ctl/`](../lima-debian-ctl/README.md)
manages one active project's mounts, environment, and SSH tunnels on this VM.
It never runs project mise setup. Prepare its guest helpers explicitly with:

```bash
./lima-debian-provision/provision.sh --tags project-session
```

The `project-session` role also runs during full provisioning. It installs
Bash/zsh environment loaders, an SSH execution helper, and a systemd service
that removes the controller's environment file at startup and orderly shutdown.
Starting the service for the first time clears that file if it already exists.
It does not resolve or provision project credentials itself.

## Out of scope

This provisioning directory does not create, clone, delete, start, or stop VMs,
or change Lima mount settings. Project environment injection, mounts, and
forwarding belong to the separate controller. Nothing here mounts your macOS
home directory.
