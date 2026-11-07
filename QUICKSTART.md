# Quick start

You need a running Debian 12 ARM64 Lima instance with a user that has
passwordless sudo and `/usr/bin/python3`. This guide assumes the instance is
called `devbox` and the user is `aschuma`. For provisioning, set
`LIMA_INSTANCE` and `LIMA_USER` if yours differ; the CLI takes the instance
from each project's `.devvm.toml`.

## 1. Host tools

```bash
brew install lima go ansible just
limactl list            # devbox should be Running
```

Go 1.23+ is only needed to build the CLI. `shellcheck` and `uv` are optional,
for `just lint`.

## 2. Provision the guest

```bash
just deps
./lima-debian-provision/provision.sh --preflight
./lima-debian-provision/provision.sh
```

The last command changes the VM: apt packages, Docker, mise with the toolchain
from `group_vars/all/tool_versions.yml`, zsh with Starship, and the helpers the
controller needs. It overwrites `~/.zshenv` and `~/.zshrc` in the
guest — keep personal zsh settings in `~/.zshrc.local`. A Starship config is
offered as `~/.config/starship.toml.example`; copy it over if you want it.

Already provisioned before the controller existed? Install just the helpers:

```bash
./lima-debian-provision/provision.sh --tags project-session
```

## 3. Build the CLI

```bash
just install            # go install → $(go env GOPATH)/bin/lima-debian-ctl
lima-debian-ctl --help
```

If the second command says "not found", Go's bin directory is not on your
`PATH`. Add it to `~/.zshrc` and open a new shell:

```zsh
export PATH="$HOME/go/bin:$PATH"
```

## 4. Describe a project

```bash
cd ~/Projects/service-a
lima-debian-ctl init
```

This writes `.devvm.toml`. Edit it:

```toml
schema_version = 1
project = "service-a"
instance = "devbox"

[[mounts]]
host = "."
guest = "/work/service-a"
writable = true

[[mounts]]                       # optional, one more folder
host = "../shared-lib"
guest = "/work/shared-lib"
writable = false

[environment]
resolver = ["./scripts/dev-env"] # prints KEY=value lines
timeout_seconds = 60
```

The resolver is any program. It runs on the host, in the project directory,
without a terminal, and must print one `KEY=value` per line (CR, LF or CRLF).
Comments, blank lines and `export ` prefixes are fine. Nothing is evaluated:
the value is everything after the first `=`, verbatim.
A typical resolver reads a 1Password item or an encrypted file. Unlock it
beforehand; the script cannot prompt.

Leave `resolver` out if the project has no secrets.

SSH keys the project needs in the guest go in one `[[ssh_keys]]` block each:

```toml
[[ssh_keys]]
source = "~/.ssh/id_ed25519"

[[ssh_keys]]
source = "~/.ssh/id_ed25519_gh"
```

Details: [SSH keys](lima-debian-ctl/README.md#ssh-keys),
[port forwards](lima-debian-ctl/README.md#forwards).

Commit `.devvm.toml`. It contains no secrets.

## 5. Work

```bash
lima-debian-ctl plan       # what activate would do; no changes
lima-debian-ctl activate   # asks before stopping the VM or replacing mounts
lima-debian-ctl shell      # banner, then a login shell in /work/service-a
lima-debian-ctl exec -- ./gradlew test
```

Activation replaces the VM's mounts with the project's and stops Lima's
automatic port forwarding; only the forwards in `.devvm.toml` are opened. If
the VM is running with other mounts it is stopped and started again (about
15 s). Anything running in the VM dies with it.

Run `mise install` or whatever the project needs yourself, inside the shell.

IntelliJ Remote Development: activate first, then start a fresh backend. The
environment is loaded by login shells; if your backend is launched some other
way, start it through `lima-debian-ctl exec -- ...`. A running backend or
Gradle daemon keeps its old environment until you restart it.

## 6. Refresh, switch, stop

```bash
lima-debian-ctl provision   # rerun the resolver, replace env file and keys; no restart

cd ~/Projects/other
lima-debian-ctl activate    # switch: stop, swap mounts, start, provision

lima-debian-ctl deactivate  # delete env file and keys, close tunnels, stop the VM
```

The env file lives on the guest disk at
`~/.config/lima-debian-ctl/environment.sh`, the keys in `~/.ssh`. Both are
deleted on `deactivate`, on a clean shutdown, and on the next boot after an
unclean one. Processes that already read them keep their values until they
exit.

If `activate` fails halfway, `status` says why. Fix the cause and run
`activate` again; nothing is marked ready until mounts, env and tunnels all
check out. After editing `.devvm.toml`, run `activate`, not `provision`; it
restarts the VM only if the mounts changed.
