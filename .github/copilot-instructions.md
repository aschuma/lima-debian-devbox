# Repository guidance

This repository provisions an existing Debian 12 ARM64 Lima VM and controls one
active project's mounts, credentials, and SSH tunnels with a Go CLI.

## Where to work

- Controller behavior/configuration: `lima-debian-ctl/`. Read its `README.md`
  before changing command semantics, environment parsing, or recovery behavior.
- Base guest setup: `lima-debian-provision/`. Read its `README.md` before changing
  packages, guest account assumptions, or provisioning tags.
- Project environment loaders and shutdown/startup cleanup:
  `lima-debian-provision/roles/project_session/`.
- Design rationale and pending acceptance checks: `docs/devvm-plan.md`.
- Lima provisioner/mount facts and primary sources:
  `docs/research/lima-project-environment.md`.

## Safety and conventions

- Ordinary tests use fake VM operations. Run `just test`, `just validate`, and
  `just lint` after relevant changes. Validation requires Ansible collections
  installed by `just deps`, but does not require a Lima instance.
- `just provision`, `just smoke`, and controller activation/stop commands modify
  a real VM. Run those only with explicit user authorization.
- Base provisioning discovers the existing user's home/group and uses Lima's
  generated SSH configuration. Preserve existing authentication and VM lifecycle.
- Controller activation owns the complete project mount set and disables broad
  automatic forwarding. A project switch stops/restarts the shared VM; refreshing
  credentials for the active project does not. Keep instance mutation locking
  and fail-closed readiness checks.
- Run a configured credential resolver on the host. Parse stdout as literal
  CR/LF/CRLF-delimited `KEY=value` records, safely render exports, and stream over
  SSH. Keep values out of logs, state JSON, Lima YAML, and process arguments.
- Credential persistence on the guest disk is accepted. Keep atomic replacement,
  restrictive permissions, and guest shutdown/startup cleanup. Existing processes
  retain inherited environment values until restarted.
- The controller leaves project mise install/trust/tasks and project mise files
  to the developer. Base tool versions belong in
  `lima-debian-provision/group_vars/all/tool_versions.yml`; add corresponding
  version checks in the appropriate role defaults when adding a base tool.
- Keep Ansible changes idempotent and preserve personal shell customization
  outside the files/blocks explicitly managed by provisioning.
- Keep external controller operations behind `Runner` for deterministic tests.
  Use `limactl`/OpenSSH subprocesses; avoid an additional daemon or SSH library.
