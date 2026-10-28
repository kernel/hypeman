import importlib.util
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


pipeline = load("pipeline", "pipeline.py")
audit = load("audit", "audit-base.py")
DIGEST = "sha256:" + "a" * 64
REF = "registry.example/base@" + DIGEST


class Tests(unittest.TestCase):
    def test_pins(self):
        self.assertTrue(pipeline.pinned(REF))
        for ref in (None, "repo:latest", REF + "\n", REF.upper(), "repo@sha256:bad"):
            self.assertFalse(pipeline.pinned(ref))

    def test_api_security(self):
        for url in ("http://example.com", "https://u:p@example.com", "https://example.com/path", "https://example.com/?token=secret"):
            with self.assertRaises(pipeline.PipelineError):
                pipeline.API(url, "secret", time.monotonic() + 10)
        pipeline.API("http://localhost:8080", "secret", time.monotonic() + 10)
        pipeline.API("https://example.com", "secret", time.monotonic() + 10)
        with self.assertRaises(pipeline.PipelineError):
            pipeline.NoRedirect().redirect_request(None)

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

    def test_build_multipart_and_provenance(self):
        class Fake:
            def request(self, path, data, headers):
                self.body = data.read()
                self.headers = headers
                return {"id": "build-123"}

            def wait(self, path, status):
                return {"image_ref": REF, "image_digest": DIGEST,
                        "provenance": {"base_image_digest": DIGEST, "source_hash": source_hash}}
        with tempfile.TemporaryDirectory() as root:
            source = Path(root) / "source.tar.gz"
            source.write_bytes(b"source")
            source_hash = audit.digest(source)
            fake = Fake()
            self.assertEqual(pipeline.build(fake, REF, source, 60), ("build-123", REF, source_hash))
            self.assertEqual(int(fake.headers["Content-Length"]), len(fake.body))
            self.assertIn(b'name="machine_base_image"', fake.body)
            self.assertIn(REF.encode(), fake.body)
            self.assertNotIn(b"Authorization", fake.body)
            self.assertEqual(fake.body.count(b'name="machine_network_mode"'), 1)
            self.assertIn(b'name="machine_network_mode"\r\n\r\nisolated\r\n', fake.body)
            self.assertEqual(pipeline.build(fake, REF, source, 60, "egress"), ("build-123", REF, source_hash))
            self.assertEqual(fake.body.count(b'name="machine_network_mode"'), 1)
            self.assertIn(b'name="machine_network_mode"\r\n\r\negress\r\n', fake.body)
            self.assertEqual(int(fake.headers["Content-Length"]), len(fake.body))
            source_hash = "b" * 64
            with self.assertRaises(pipeline.PipelineError):
                pipeline.build(fake, REF, source, 60)

    def test_invalid_network_mode_before_submission(self):
        with tempfile.TemporaryDirectory() as root:
            source = Path(root) / "source.tar.gz"
            source.write_bytes(b"source")
            for mode in ("", "nat", "allowlist", "egress\r\ninjected"):
                with self.subTest(mode=mode):
                    with self.assertRaisesRegex(pipeline.PipelineError, "invalid-network-mode"):
                        pipeline.build(None, REF, source, 60, mode)

    def test_promote_normal_endpoints(self):
        class Fake:
            def __init__(self):
                self.calls = []

            def pull(self, ref):
                self.calls.append(("pull", ref))

            def request(self, path, data):
                self.calls.append((path, data))
                return {"id": "push-123", "digest": DIGEST}

            def wait(self, path, status):
                return {"digest": DIGEST}
        fake = Fake()
        promoted = "registry.example/release@" + DIGEST
        self.assertEqual(pipeline.promote(fake, REF, "registry.example/release:candidate"), ("push-123", promoted))
        self.assertEqual(fake.calls[1][0], "/pushes")
        self.assertTrue(fake.calls[3][0].endswith("/tag"))
        self.assertEqual(fake.calls[3][1], {"target": "registry.example/release:candidate"})
        for bad in ("repo:latest", "https://registry.example/repo:tag", "registry.example/repo@" + DIGEST):
            with self.assertRaises(pipeline.PipelineError):
                pipeline.promote(fake, REF, bad)

    def test_wait_failure_and_deadline(self):
        api = pipeline.API("https://example.com", "secret", time.monotonic() + 10)
        with patch.object(api, "request", return_value={"status": "failed", "error": "secret"}):
            with self.assertRaises(pipeline.PipelineError) as ctx:
                api.wait("/builds/id", "ready")
            self.assertNotIn("secret", str(ctx.exception))
        api.deadline = time.monotonic() - 1
        with self.assertRaises(pipeline.PipelineError):
            api.request("/images")


if __name__ == "__main__":
    unittest.main()
