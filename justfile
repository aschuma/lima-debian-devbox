# Host tasks for the Go Lima controller and existing-guest Ansible provisioning.
# Tests, validation, and lint never start/stop or provision a real VM.
set shell := ["bash", "-euo", "pipefail", "-c"]

root := justfile_directory()

default:
    @just --list

# Build the host controller (requires Go 1.23+)
build:
    mkdir -p "{{root}}/lima-debian-ctl/bin"
    cd "{{root}}/lima-debian-ctl" && go build -o bin/lima-debian-ctl .

# Install the controller into GOBIN or GOPATH/bin
install:
    cd "{{root}}/lima-debian-ctl" && go install .

# Go formatting, race tests, vet, staticcheck, and Python inventory tests; no VM required
test:
    cd "{{root}}/lima-debian-ctl" && test -z "$(gofmt -l .)"
    cd "{{root}}/lima-debian-ctl" && go test -race -cover ./...
    cd "{{root}}/lima-debian-ctl" && go vet ./...
    cd "{{root}}/lima-debian-ctl" && go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
    python3 -m unittest discover -s "{{root}}/lima-debian-provision/tests" -v

# Install Ansible collections into the provisioning directory
deps:
    "{{root}}/lima-debian-provision/provision.sh" --install-deps

# Ansible syntax only, using localhost inventory; no Lima instance required
validate:
    cd "{{root}}/lima-debian-provision" && ANSIBLE_CONFIG="$PWD/ansible.cfg" ANSIBLE_COLLECTIONS_PATH="$PWD/.ansible/collections" ansible-playbook --syntax-check -i localhost, playbook.yml

# Shell, YAML, and Ansible lint (requires shellcheck and uv)
lint:
    shellcheck "{{root}}/lima-debian-provision/provision.sh" "{{root}}/lima-debian-provision/roles/project_session/files/lima-debian-ctl-guest" "{{root}}/lima-debian-provision/roles/project_session/files/lima-debian-ctl.sh"
    cd "{{root}}" && uvx --from yamllint yamllint .github .yamllint.yml
    cd "{{root}}/lima-debian-provision" && uvx --from yamllint yamllint .
    cd "{{root}}/lima-debian-provision" && uvx --from ansible-lint ansible-lint playbook.yml

# APPLY base provisioning to an existing running VM (optional comma-separated tags)
provision tags="all":
    "{{root}}/lima-debian-provision/provision.sh" --tags {{quote(tags)}}

# APPLY the explicit Java Testcontainers runtime check inside the guest
smoke:
    "{{root}}/lima-debian-provision/provision.sh" --tags testcontainers-smoke
