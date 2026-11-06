"""Generate inventory from Lima metadata, not hard-coded ports or keys."""

import json
import os
from pathlib import Path
import shlex
import sys


def inventory(text, environ):
    name = environ.get("LIMA_INSTANCE", "devbox")
    user = environ.get("LIMA_USER", "aschuma")
    if not user or any(c.isspace() or c in "\r\n\0" for c in user):
        raise ValueError("LIMA_USER must be a nonempty account name without whitespace")
    instances = [json.loads(line) for line in text.splitlines() if line.strip()]
    instance = next((row for row in instances if row["name"] == name), None)
    if instance is None:
        raise ValueError(f"No Lima instance named {name!r}; create it separately")
    if instance["status"] != "Running":
        raise ValueError(f"Lima instance {name!r} must already be running")
    config = Path(instance["sshConfigFile"])
    if not config.is_file():
        raise ValueError(f"Lima SSH config does not exist: {config}")
    variables = {
        "ansible_host": instance["hostname"],
        "ansible_user": user,
        "ansible_connection": "ssh",
        "ansible_ssh_common_args": f"-F {shlex.quote(str(config))}",
        "ansible_python_interpreter": "/usr/bin/python3",
        "lima_user": user,
    }
    for setting, key, default in [
        ("INSTALL_DOCKER", "install_docker", "true"),
        ("CONFIGURE_SHELL", "configure_shell", "true"),
        ("UPGRADE_PACKAGES", "upgrade_packages", "false"),
    ]:
        value = environ.get(setting, default).lower()
        if value not in ("true", "false"):
            raise ValueError(f"{setting} must be true or false")
        variables[key] = value == "true"
    return {"all": {"hosts": {"lima_guest": variables}}}


if __name__ == "__main__":
    try:
        print(json.dumps(inventory(Path(sys.argv[1]).read_text(), os.environ)))
    except (ValueError, KeyError, OSError) as error:
        sys.exit(str(error))
