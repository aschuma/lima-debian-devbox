# Quick start

This guide takes you from an empty Lima VM to a working project shell.

## What you need

- macOS with [Lima](https://lima-vm.io/) installed.
- A running Debian 12 ARM64 Lima instance. This guide calls it `devbox`.
- A user inside the VM with passwordless sudo and `/usr/bin/python3`. Lima
  creates such a user by default. This guide calls it `aschuma`.

If your names differ, set `LIMA_INSTANCE` and `LIMA_USER` for provisioning.
The CLI reads the instance name from each project's `.devvm.toml`.

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

The last command changes the VM. It installs apt packages, Docker, mise with
the toolchain from `group_vars/all/tool_versions.yml`, zsh with Starship, and
the helpers the controller needs.

It overwrites `~/.zshenv` and `~/.zshrc` in the guest. Keep personal zsh
settings in `~/.zshrc.local`. A Starship config is offered as
`~/.config/starship.toml.example`; copy it over if you want it.

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

**Credentials.** The resolver is any program. It runs on the host, in the
project directory, without a terminal. It must print one `KEY=value` per line.
Comments, blank lines and `export ` prefixes are fine. Nothing is evaluated:
the value is everything after the first `=`, as is. A typical resolver reads a
1Password item or an encrypted file. Unlock it first; the script cannot
prompt. Leave `resolver` out if the project has no secrets.

**SSH.** `git` and `ssh` in the guest use your host SSH agent. Lima forwards
the agent into the VM; no private key is copied. This is on by default. To
turn it off for a project, add `forward_agent = false` at the top of
`.devvm.toml`. Keep your agent unlocked on the host.

**File watching.** Edits you save on the Mac raise inotify events inside the
VM, so the IDE backend and build watchers notice them. This is on by default
(`mount_inotify = true`). Set it to `false` if it misbehaves.

Details: [SSH agent](lima-debian-ctl/README.md#ssh-agent),
[port forwards](lima-debian-ctl/README.md#forwards).

Commit `.devvm.toml`. It contains no secrets.

## 5. Work

```bash
lima-debian-ctl plan       # what activate would do; no changes
lima-debian-ctl activate   # asks before stopping the VM or replacing mounts
lima-debian-ctl shell      # banner, then a login shell in /work/service-a
lima-debian-ctl exec -- ./gradlew test
```

Activation replaces the VM's mounts with the project's. It also turns off
Lima's automatic port forwarding; only the forwards in `.devvm.toml` are
opened. If the VM is running with other mounts, it is stopped and started
again (about 15 s). Anything running in the VM dies with it.

Run `mise install` or whatever the project needs yourself, inside the shell.

**IntelliJ Remote Development.** Activate first, then start a fresh backend.
Login shells load the environment. If your backend is launched some other
way, start it through `lima-debian-ctl exec -- ...`. A running backend or
Gradle daemon keeps its old environment until you restart it.

## 6. Refresh, switch, stop

```bash
lima-debian-ctl provision   # rerun the resolver, replace the env file; no restart

cd ~/Projects/other
lima-debian-ctl activate    # switch: stop, swap mounts, start, provision

lima-debian-ctl deactivate  # delete the env file, close tunnels, stop the VM
```

The env file lives on the guest disk at
`~/.config/lima-debian-ctl/environment.sh`. It is deleted on `deactivate`, on
a clean shutdown, and on the next boot after an unclean one. Processes that
already read it keep their values until they exit.

If `activate` fails halfway, `status` says why. Fix the cause and run
`activate` again. Nothing is marked ready until mounts, env and tunnels all
check out.

After editing `.devvm.toml`, run `activate`, not `provision`. It restarts the
VM only if the mounts, `forward_agent` or `mount_inotify` changed.
