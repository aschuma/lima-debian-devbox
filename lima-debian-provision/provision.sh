#!/usr/bin/env bash
# Provision an existing running Lima VM. Never creates, starts, or stops it.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$ROOT/provision.env" ]]; then
  # Trusted, local shell configuration; do not put secrets here.
  # shellcheck source=/dev/null
  source "$ROOT/provision.env"
fi
export LIMA_INSTANCE="${LIMA_INSTANCE:-devbox}"
export LIMA_USER="${LIMA_USER:-aschuma}"
export INSTALL_DOCKER="${INSTALL_DOCKER:-true}"
export CONFIGURE_SHELL="${CONFIGURE_SHELL:-true}"
export UPGRADE_PACKAGES="${UPGRADE_PACKAGES:-false}"
export ANSIBLE_CONFIG="$ROOT/ansible.cfg"
export ANSIBLE_COLLECTIONS_PATH="$ROOT/.ansible/collections"

case "${1:-}" in
  --install-deps)
    cd "$ROOT"
    exec ansible-galaxy collection install -r requirements.yml -p "$ANSIBLE_COLLECTIONS_PATH"
    ;;
esac
command -v limactl >/dev/null || { echo 'limactl is required' >&2; exit 1; }
command -v python3 >/dev/null || { echo 'python3 is required on the host' >&2; exit 1; }
mkdir -p "$ROOT/.runtime"
WORK="$(mktemp -d "$ROOT/.runtime/run.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
limactl list --json > "$WORK/instances.json"
python3 "$ROOT/inventory.py" "$WORK/instances.json" > "$WORK/inventory.json"
case "${1:-}" in
  --print-target) python3 -m json.tool "$WORK/inventory.json"; exit 0 ;;
  --preflight) shift; set -- --tags preflight "$@" ;;
esac
cd "$ROOT"
ansible-playbook -i "$WORK/inventory.json" playbook.yml "$@"
