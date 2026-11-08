# Lima Debian dev VM

A development VM for macOS, built on [Lima](https://lima-vm.io/). One Debian 12
(ARM64) VM, called `devbox`, serves all your projects. You switch the VM from
one project to the next instead of running one VM per project.

## Why one shared VM

Tools, caches and Docker images live on the VM disk. They are installed once
and survive a project switch. What changes per project is small:

- which host folders are mounted,
- which credentials are present,
- which ports are forwarded.

The host home directory is never mounted. Only the active project's folders
are visible inside the VM.

## The three parts

| Directory | Purpose |
| --- | --- |
| [`devbox-lima-template.yml`](devbox-lima-template.yml) | Lima template. Creates the VM with the right type, size, image and Lima settings. Run once per VM. Contains no provisioning. |
| [`lima-debian-provision/`](lima-debian-provision/README.md) | Ansible. Installs Docker, a mise toolchain, zsh with Starship and the guest helpers into an existing VM. Run it once, and again when tool versions change. |
| [`lima-debian-ctl/`](lima-debian-ctl/README.md) | Go CLI. Switches the VM between projects: mounts, credentials, port forwards, shell. Run it every day. |

## How it works

**Provisioning** runs against a VM and user that already exist. It never
creates or starts a VM and never changes SSH access.

**Activating a project** (`lima-debian-ctl activate`) makes that project the
VM's only project. If the mounts change, the VM is stopped, edited and started
again, because Lima cannot change mounts while a VM runs. Refreshing
credentials for the active project needs no restart.

**Credentials** come from a script you write. It runs on the host and prints
`KEY=value` lines. The controller writes them to a `0600` file in the guest
home. Login shells load that file. A systemd unit deletes it at shutdown.

**SSH** inside the guest uses your host SSH agent through Lima's agent
forwarding. No private key is copied into the VM. This is off by default;
enable it with `lima.ssh.forwardAgent: true` in `.devvm.yaml`.

**Project tooling** such as `mise.toml` is yours to run. The controller does
not touch it.

## Getting started

Follow [QUICKSTART.md](QUICKSTART.md). In short:

```bash
limactl create --name devbox ./devbox-lima-template.yml
limactl start devbox
./lima-debian-provision/provision.sh --preflight
./lima-debian-provision/provision.sh          # changes the guest
just install                                  # builds lima-debian-ctl

cd ~/Projects/service-a
lima-debian-ctl init                          # writes .devvm.yaml
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

## Design notes

- [docs/devvm-plan.md](docs/devvm-plan.md) — requirements and design of the
  controller.
- [docs/research/lima-project-environment.md](docs/research/lima-project-environment.md)
  — why credentials are handled the way they are.
- [docs/research/lima-native-features.md](docs/research/lima-native-features.md)
  — which Lima features the project uses natively and which it builds itself.

## License

[MIT](LICENSE)
