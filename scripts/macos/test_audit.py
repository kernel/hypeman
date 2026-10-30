import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


audit = load("audit", "audit-base.py")


class AuditTests(unittest.TestCase):
    def test_homes_fail_closed(self):
        with tempfile.TemporaryDirectory() as root:
            home = Path(root)
            audit.audit_homes([home], {})
            private = home / "preferences"
            private.write_text("never-print-this")
            with self.assertRaises(audit.Rejected) as ctx:
                audit.audit_homes([home], {})
            self.assertNotIn("never-print-this", str(ctx.exception))
            baseline = {str(private): audit.digest(private)}
            audit.audit_homes([home], baseline)
            private.write_text("changed")
            with self.assertRaises(audit.Rejected):
                audit.audit_homes([home], baseline)
            private.unlink()
            private.symlink_to("/etc/passwd")
            with self.assertRaises(audit.Rejected):
                audit.audit_homes([home], baseline)

    def test_credentials_cannot_be_baselined(self):
        with tempfile.TemporaryDirectory() as root:
            home = Path(root)
            credentials = home / ".netrc"
            credentials.write_text("never-print-this")
            with self.assertRaises(audit.Rejected):
                audit.audit_homes([home], {str(credentials): audit.digest(credentials)})
        with patch.object(audit.os, "walk", side_effect=lambda *args, **kwargs: kwargs["onerror"](OSError())):
            with tempfile.TemporaryDirectory() as root:
                with self.assertRaises(audit.Rejected):
                    audit.audit_homes([Path(root)], {})

    def test_audit_inputs_outside_homes(self):
        with tempfile.TemporaryDirectory() as root:
            home = Path(root) / "home"
            home.mkdir()
            outside = Path(root) / "audit.py"
            outside.write_text("reviewed")
            audit.audit_inputs_outside_homes([home], [outside])
            inside = home / "baseline.json"
            inside.write_text("{}")
            with self.assertRaisesRegex(audit.Rejected, "audit-input-in-home"):
                audit.audit_inputs_outside_homes([home], [inside])
            alias = Path(root) / "alias.json"
            alias.symlink_to(inside)
            with self.assertRaisesRegex(audit.Rejected, "audit-input-in-home"):
                audit.audit_inputs_outside_homes([home], [alias])


if __name__ == "__main__":
    unittest.main()
