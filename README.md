# Lima Debian dev VM

One Debian 12 (ARM64) Lima VM, reused across projects. Only the active
project's folders are mounted, and only its credentials and SSH keys are
copied into the guest. The host home is never mounted.

| Directory | What it does |
| --- | --- |
| [`lima-debian-provision/`](lima-debian-provision/README.md) | Ansible. Installs Docker, mise-managed tools, zsh/Starship and the guest helpers into an existing VM. |
| [`lima-debian-ctl/`](lima-debian-ctl/README.md) | Go CLI. Switches the VM between projects: mounts, credentials, SSH tunnels, shell. |

## How it works

- Provisioning runs against a VM and user that already exist. It never creates
  or starts VMs and never changes SSH access.
- `lima-debian-ctl activate` makes the current project the VM's only project.
  Switching projects stops the VM, replaces its mounts and starts it again
  (Lima cannot change mounts while running). Refreshing credentials for the
  active project needs no restart.
- Credentials come from a script you configure. It runs on the host and prints
  `KEY=value` lines. The controller writes them, quoted, to a `0600` file in the
  guest home; login shells load it. SSH keys listed in the project config are
  copied to the guest's `~/.ssh`. A systemd unit deletes both at shutdown.
- Project tooling (`mise.toml`) is yours to run. The controller never touches it.

## Getting started

Follow [QUICKSTART.md](QUICKSTART.md). In short:

```bash
./lima-debian-provision/provision.sh --preflight
./lima-debian-provision/provision.sh          # changes the guest
just install                                  # builds lima-debian-ctl

cd ~/Projects/service-a
lima-debian-ctl init                          # writes .devvm.toml
lima-debian-ctl activate
lima-debian-ctl shell
```

## Development

```bash
just test       # Go tests (race, vet, staticcheck) + Python inventory tests
just lint       # shellcheck, yamllint, ansible-lint
just validate   # ansible-playbook --syntax-check
just build      # lima-debian-ctl/bin/lima-debian-ctl
```

None of these touch a VM. `just provision` and `just smoke` do.

Design notes: [docs/devvm-plan.md](docs/devvm-plan.md),
[docs/research/lima-project-environment.md](docs/research/lima-project-environment.md).

## License

[MIT](LICENSE)
