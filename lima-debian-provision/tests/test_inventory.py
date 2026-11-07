import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("inventory", ROOT / "inventory.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.config = Path(self.temp.name) / "path with spaces" / "ssh.config"
        self.config.parent.mkdir()
        self.config.touch()
        self.row = {"name": "devbox", "status": "Running", "hostname": "lima-devbox",
                    "sshConfigFile": str(self.config)}

    def make(self, env=None):
        return module.inventory(json.dumps(self.row), env or {})["all"]["hosts"]["lima_guest"]

    def test_defaults_use_lima_ssh_and_docker(self):
        result = self.make()
        self.assertEqual(result["ansible_user"], "aschuma")
        self.assertEqual(result["ansible_host"], "lima-devbox")
        self.assertEqual(result["ansible_python_interpreter"], "/usr/bin/python3")
        self.assertIn("'", result["ansible_ssh_common_args"])
        self.assertTrue(result["install_docker"])
        self.assertFalse(result["upgrade_packages"])
        self.assertNotIn("devbox_home", result)

    def test_overrides(self):
        self.row["name"] = "other"
        result = self.make({"LIMA_INSTANCE": "other", "LIMA_USER": "someone",
                            "INSTALL_DOCKER": "false", "CONFIGURE_SHELL": "false"})
        self.assertEqual(result["lima_user"], "someone")
        self.assertFalse(result["install_docker"])
        self.assertFalse(result["configure_shell"])

    def test_missing_instance(self):
        with self.assertRaisesRegex(ValueError, "No Lima instance"):
            self.make({"LIMA_INSTANCE": "missing"})

    def test_stopped_instance(self):
        self.row["status"] = "Stopped"
        with self.assertRaisesRegex(ValueError, "already be running"):
            self.make()

    def test_missing_ssh_config(self):
        self.config.unlink()
        with self.assertRaisesRegex(ValueError, "SSH config does not exist"):
            self.make()

    def test_invalid_boolean(self):
        with self.assertRaisesRegex(ValueError, "must be true or false"):
            self.make({"INSTALL_DOCKER": "maybe"})


if __name__ == "__main__":
    unittest.main()
