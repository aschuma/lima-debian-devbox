# Lima provisioners and project credentials

## Scope

Design findings for the existing Debian 12 `devbox`, using Lima 2.2.1.
This research note does not itself change VM configuration. The subsequent Go
implementation is in `lima-debian-ctl/`, with guest helpers under
`lima-debian-provision/roles/project_session/`.

## Findings

- Lima documents provisioning as a per-boot workflow. `system` scripts run as root, followed by `user` scripts without root privileges. Scripts must be idempotent. `boot` is an earlier cloud-init stage, not a shutdown hook. [1]
- The documented provisioning modes do not include a shutdown mode. Use guest lifecycle facilities or ephemeral storage rather than assuming a Lima shutdown provisioner exists. [1]
- External provisioner `file` content is read at instance creation and stored under `script`; it is not a live host-side credential resolver. [1]
- Provisioner scripts and data are included in Lima's cidata layout. Therefore, dynamically embedding resolved credentials into YAML, scripts, or `mode: data` can leave secret-bearing host-side configuration or generated artifacts. Deleting only the guest output file does not address these copies. [2]
- The default template uses `set -x` in examples. Do not copy that setting into secret-handling scripts: shell tracing can reveal expanded values.
- Read-only check on the current guest: `limactl shell devbox -- findmnt -no TARGET,FSTYPE /run` returned `/run tmpfs`.

## Agreed direction (implemented; live lifecycle validation pending)

The user accepts on-disk credential persistence with host-side rendering and SSH
transfer, rather than requiring tmpfs storage or embedding secrets in Lima YAML.

1. Resolve inputs on the host, parse as data, render shell-quoted exports, and stream the result over SSH into a guest environment file. Accept CR/LF/CRLF-delimited `KEY=value` resolver output, splitting at the first equals sign. This is implemented in `lima-debian-ctl/environment.go` and the guest helper.
2. Store the active environment outside host-mounted source folders, under `~/.config/lima-debian-ctl/environment.sh`. Use restrictive developer-owned permissions and atomic replacement.
3. Install static shell loaders so new shells can read the generated file. Cover zsh and Bash deliberately; `/etc/profile.d` alone does not cover all shell startup modes. Explicitly load the environment when starting IntelliJ's remote backend or project commands where shell startup files are bypassed. Existing processes retain their inherited values until restarted.
4. For the agreed single-active-project model, replace rather than merge the active environment when switching projects. Existing project processes do not lose their inherited environment automatically; switching projects restarts the VM.
5. Offer guest systemd lifecycle cleanup that removes the environment file during orderly shutdown, plus startup cleanup of stale files after an unclean shutdown. A host wrapper also removes it on explicit deactivation. Forced stops cannot guarantee immediate on-disk deletion; the user accepts this limitation. Do not promise secure erasure.
6. Keep secret values out of logs and command arguments, and do not execute resolver output directly. Disk persistence is accepted, not a reason to enable shell tracing or log credentials.
7. Use Lima provisioning or Ansible only to install loaders and cleanup machinery. Refresh project credentials directly over SSH while the shared VM is running; reboot is not required merely to rerun a provisioner.


## Switching project mounts without restarting

For installed Lima 2.2.1, the supported `limactl edit` implementation rejects a
running instance with `cannot edit a running instance`. There is no `limactl
mount` command in this installation. Changing configured host mounts therefore
requires stopping, editing, and starting the instance. [3]

The current `devbox` uses VZ; Lima documents virtiofs as the default mount type
for VZ. [4] A guest bind mount can rearrange a directory that is already exposed
to the guest, but cannot expose a new host directory by itself.

Options for the design:

- Mount one or two dedicated host workspace roots once, then switch guest
  working directories (or guest bind mounts) without restarting. All projects
  beneath those roots are accessible to the guest; this is broader access than
  mounting only the active project, but does not require sharing the host home.
- If only the selected project's arbitrary host folders may be exposed, use the
  supported stop/edit/start workflow.
- Custom live SSHFS/network mounts would be a separate mechanism, not Lima
  configured-mount hot swapping. Validate authentication, filesystem semantics,
  performance, and disconnect handling in a prototype before adopting it.

## Primary sources

1. [Lima v2.2.1 default template: provisioning semantics and examples](https://github.com/lima-vm/lima/blob/v2.2.1/templates/default.yaml#L202-L308).
2. [Lima v2.2.1 cidata generation: scripts/data included in layout](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/cidata/cidata.go#L435-L460).
3. [Lima v2.2.1 edit command: running-instance rejection](https://github.com/lima-vm/lima/blob/v2.2.1/cmd/limactl/edit.go#L68-L69).
4. [Lima v2.2.1 filesystem mount documentation](https://github.com/lima-vm/lima/blob/v2.2.1/website/content/en/docs/config/mount.md).
