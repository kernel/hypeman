#!/usr/bin/env python3
"""Operator/CI orchestration over normal authenticated Hypeman APIs.
API tokens come only from HYPEMAN_TOKEN / HYPEMAN_DEST_TOKEN. No response bodies
or guest logs are printed. Does not install macOS or certify guest bootability.
"""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import uuid


class PipelineError(Exception):
    pass


def require(ok, code):
    if not ok:
        raise PipelineError(code)


def pinned(ref):
    return isinstance(ref, str) and re.fullmatch(r"[a-zA-Z0-9.-]+(?::[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}", ref) is not None


def tagged(ref):
    return isinstance(ref, str) and re.fullmatch(r"[a-zA-Z0-9.-]+(?::[0-9]+)?/[a-z0-9._/-]+:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}", ref) is not None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise PipelineError("redirect-rejected")


class API:
    def __init__(self, url, token, deadline):
        parsed = urllib.parse.urlsplit(url)
        require(parsed.hostname and not parsed.username and not parsed.password and not parsed.query
                and not parsed.fragment and parsed.path in ("", "/"), "invalid-api-url")
        loopback = parsed.hostname == "localhost"
        try:
            loopback = loopback or ipaddress.ip_address(parsed.hostname).is_loopback
        except ValueError:
            pass
        require(parsed.scheme == "https" or (parsed.scheme == "http" and loopback), "https-required")
        require(token and "\n" not in token and "\r" not in token, "token-required")
        self.url, self.token, self.deadline = url.rstrip("/"), token, deadline
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, path, data=None, headers=None):
        require(time.monotonic() < self.deadline, "deadline-exceeded")
        headers = dict(headers or {})
        if isinstance(data, dict):
            data = json.dumps(data).encode()
            headers["Content-Type"] = "application/json"
        headers["Authorization"] = "Bearer " + self.token
        request = urllib.request.Request(self.url + path, data=data, headers=headers)
        with self.opener.open(request, timeout=max(1, min(120, self.deadline-time.monotonic()))) as response:
            body = response.read(1024 * 1024 + 1)
            require(len(body) <= 1024 * 1024, "response-too-large")
            return json.loads(body)

    def wait(self, path, terminal):
        while True:
            result = self.request(path)
            if result.get("status") == terminal:
                return result
            require(result.get("status") in {"queued", "building", "pushing", "pending", "pulling", "converting"},
                    "job-not-successful")
            require(time.monotonic() + 2 < self.deadline, "deadline-exceeded")
            time.sleep(2)

    def pull(self, ref):
        require(pinned(ref), "immutable-image-required")
        self.request("/images", {"name": ref, "platform": "darwin/arm64"})
        image = self.wait("/images/" + urllib.parse.quote(ref, safe=""), "ready")
        require(image.get("digest") == ref.split("@")[1] and image.get("platform") == "darwin/arm64",
                "image-verification-failed")
        return image


def build(api, ref, source, timeout, network_mode="isolated"):
    require(network_mode in {"isolated", "egress"}, "invalid-network-mode")
    require(source.is_file() and source.stat().st_size <= 64 << 20, "invalid-source")
    boundary = "hypeman-" + uuid.uuid4().hex
    h = hashlib.sha256()
    with tempfile.TemporaryFile() as body:
        for key, value in (("machine_base_image", ref), ("timeout_seconds", str(timeout)),
                           ("machine_network_mode", network_mode)):
            body.write((f"--{boundary}\r\nContent-Disposition: form-data; name=\"{key}\"\r\n\r\n{value}\r\n").encode())
        body.write((f"--{boundary}\r\nContent-Disposition: form-data; name=\"source\"; filename=\"source.tar.gz\"\r\nContent-Type: application/gzip\r\n\r\n").encode())
        copied = 0
        with source.open("rb") as f:
            for chunk in iter(lambda: f.read(1024 * 1024), b""):
                copied += len(chunk)
                require(copied <= 64 << 20, "source-too-large")
                body.write(chunk)
                h.update(chunk)
        body.write((f"\r\n--{boundary}--\r\n").encode())
        length = body.tell()
        body.seek(0)
        job = api.request("/builds", body, {"Content-Type": "multipart/form-data; boundary=" + boundary,
                                          "Content-Length": str(length)})
    job_id = job.get("id", "")
    require(re.fullmatch(r"[a-zA-Z0-9-]+", job_id), "invalid-build-id")
    job = api.wait("/builds/" + job_id, "ready")
    output = job.get("image_ref")
    require(pinned(output) and job.get("image_digest") == output.split("@")[1], "invalid-build-output")
    provenance = job.get("provenance") or {}
    require(provenance.get("base_image_digest") == ref.split("@")[1]
            and provenance.get("source_hash") == h.hexdigest(), "build-provenance-mismatch")
    return job_id, output, h.hexdigest()


def promote(api, image, target):
    require(tagged(target), "tagged-target-required")
    api.pull(image)
    job = api.request("/pushes", {"image": image, "target": target})
    job_id = job.get("id", "")
    require(re.fullmatch(r"[a-zA-Z0-9-]+", job_id), "invalid-push-id")
    job = api.wait("/pushes/" + job_id, "pushed")
    # Machine output is OCI; normal push must preserve the manifest digest.
    require(job.get("digest") == image.split("@")[1], "promotion-digest-mismatch")
    promoted = target.rsplit(":", 1)[0] + "@" + job["digest"]
    api.pull(promoted)
    alias = api.request("/images/" + urllib.parse.quote(promoted, safe="") + "/tag", {"target": target})
    require(alias.get("digest") == job["digest"], "local-tag-mismatch")
    return job_id, promoted


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--api", required=True)
    p.add_argument("--base-receipt", type=Path, required=True)
    p.add_argument("--source", type=Path, required=True)
    p.add_argument("--target", required=True, help="candidate registry tag, never an unreviewed production alias")
    p.add_argument("--destination-api", required=True, help="Mac host that must pre-pull the promoted digest")
    p.add_argument("--output", required=True, type=Path)
    p.add_argument("--timeout", type=int, default=7200)
    p.add_argument("--network-mode", choices=("isolated", "egress"), default="isolated",
                   help="guest network policy; egress is unrestricted VZ NAT (default: isolated)")
    args = p.parse_args()
    require(1 <= args.timeout <= 86400, "invalid-timeout")
    require(tagged(args.target), "tagged-target-required")
    require(args.source.is_file() and args.source.stat().st_size <= 64 << 20, "invalid-source")
    deadline = time.monotonic() + args.timeout
    # Reserve evidence before any side effects; no overwriting a prior run.
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    complete = False
    try:
        base = json.loads(args.base_receipt.read_text())
        ref = base.get("reference")
        require(base.get("version") == 1 and pinned(ref) and base.get("digest") == ref.split("@")[1]
                and base.get("audit", {}).get("status") == "pass", "invalid-base-receipt")
        api = API(args.api, os.environ.get("HYPEMAN_TOKEN"), deadline)
        dest = API(args.destination_api, os.environ.get("HYPEMAN_DEST_TOKEN"), deadline)
        api.pull(ref)
        build_id, image, source_hash = build(api, ref, args.source, args.timeout, args.network_mode)
        push_id, promoted = promote(api, image, args.target)
        dest.pull(promoted)
        evidence = {"version": 1, "base": ref, "source_sha256": source_hash,
                    "machine_network_mode": args.network_mode,
                    "build_id": build_id, "build_image": image, "push_id": push_id,
                    "promoted_image": promoted, "destination_prepull": "verified",
                    "boot_validation": "required-before-production-alias"}
        with os.fdopen(fd, "w") as f:
            fd = None
            json.dump(evidence, f, sort_keys=True)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        complete = True
    finally:
        if fd is not None:
            os.close(fd)
        if not complete:
            args.output.unlink(missing_ok=True)


if __name__ == "__main__":
    try:
        main()
    except PipelineError as exc:
        print("pipeline rejected: " + str(exc), file=sys.stderr)
        sys.exit(1)
    except Exception:
        print("pipeline failed; inspect authenticated job status privately; no successful evidence", file=sys.stderr)
        sys.exit(1)
