#!/usr/bin/env python3
"""Run INSIDE a fresh provisioning guest as root, preferably via GuestService.
Never emits command output, paths, file contents, account names or environment.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import stat
import subprocess
import sys


class Rejected(Exception):
    pass


def require(ok, code):
    if not ok:
        raise Rejected(code)


def command(*args):
    result = subprocess.run(args, capture_output=True, timeout=30, check=False)
    require(result.returncode == 0, "probe-failed")
    return result.stdout.decode("utf-8", errors="strict").strip()


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def trusted(path):
    path = Path(path)
    for parent in (path, *path.parents):
        s = parent.lstat()
        require(not stat.S_ISLNK(s.st_mode) and s.st_uid == 0 and not s.st_mode & 0o022,
                "untrusted-agent-path")
    require(path.is_file(), "agent-not-regular")


def walk_error(_error):
    raise Rejected("home-scan-failed")


FORBIDDEN = {".ssh", ".aws", ".azure", ".config", ".docker", ".kube", ".gnupg",
             ".npmrc", ".netrc", ".git-credentials", "Keychains", "Accounts", "Cookies",
             "Safari", "Google", "Chromium", "Firefox", "CloudStorage", "Mobile Documents",
             ".bash_history", ".zsh_history", ".python_history", ".lesshst", ".zsh_sessions"}


def audit_inputs_outside_homes(homes, paths):
    roots = [home.resolve(strict=True) for home in homes]
    for path in paths:
        resolved = Path(path).resolve(strict=True)
        require(not any(resolved == home or home in resolved.parents for home in roots),
                "audit-input-in-home")


def audit_homes(homes, baseline):
    seen = set()
    for home in homes:
        require(home.is_dir() and not home.is_symlink(), "invalid-home")
        for root, dirs, files in os.walk(home, followlinks=False, onerror=walk_error):
            for name in dirs + files:
                require(name not in FORBIDDEN, "credential-or-browser-state")
                path = Path(root) / name
                require(not path.is_symlink(), "home-link")
                if path.is_dir():
                    continue
                require(path.is_file(), "home-special-file")
                key = str(path)
                require(key in baseline and digest(path) == baseline[key], "unreviewed-home-state")
                seen.add(key)
    require(seen == set(baseline), "baseline-mismatch")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--agent", required=True)
    p.add_argument("--agent-sha256", required=True)
    p.add_argument("--plist", required=True)
    p.add_argument("--plist-sha256", required=True)
    p.add_argument("--users", required=True, help="exact comma-separated UID>=500 accounts, excluding nobody")
    p.add_argument("--home-baseline", required=True, help="reviewed JSON: absolute home file -> SHA256; {} allows no files")
    p.add_argument("--clean-transient", action="store_true", help="remove shell histories only, never credential files")
    p.add_argument("--runtime-state-prefix", action="append", default=[], help="additional /private/tmp, /private/var/tmp or /private/var/run entry prefix that must not exist; repeatable")
    args = p.parse_args()
    require(sys.platform == "darwin" and os.geteuid() == 0, "guest-root-required")
    require(command("/usr/bin/uname", "-m") == "arm64", "arm64-required")
    for value in (args.agent_sha256, args.plist_sha256):
        require(re.fullmatch(r"[0-9a-f]{64}", value), "invalid-pin")
    trusted(args.agent)
    trusted(args.plist)
    require(digest(args.agent) == args.agent_sha256 and digest(args.plist) == args.plist_sha256,
            "agent-pin-mismatch")
    with open(args.plist, "rb") as f:
        job = plistlib.load(f)
    require(job.get("UserName", "root") == "root" and job.get("ProgramArguments") == [args.agent]
            and not job.get("EnvironmentVariables") and not job.get("Program"), "agent-job-policy")
    label = job.get("Label", "")
    require(re.fullmatch(r"[a-zA-Z0-9.-]+", label), "invalid-agent-label")
    job_state = command("/bin/launchctl", "print", "system/" + label)
    require("state = running" in job_state and ("program = " + args.agent) in job_state,
            "agent-not-running")
    pid = re.search(r"\bpid = ([0-9]+)\b", job_state)
    require(pid is not None, "agent-pid-missing")
    process = command("/bin/ps", "-p", pid.group(1), "-o", "uid=", "-o", "comm=").split(None, 1)
    require(process == ["0", args.agent], "agent-process-mismatch")
    # A readiness file alone is not RPC proof. The operator must execute this
    # script over GuestService and require normal instance readiness before stop.
    users = command("/usr/bin/dscl", ".", "-list", "/Users", "UniqueID")
    actual = {line.split()[0] for line in users.splitlines()
              if int(line.split()[-1]) >= 500 and line.split()[0] != "nobody"}
    require(actual == set(args.users.split(",")), "unexpected-accounts")
    homes = [Path("/var/root"), Path("/Users/Shared")]
    # /var is a standard macOS symlink; homes themselves must be real directories.
    for user in sorted(actual):
        entry = command("/usr/bin/dscl", ".", "-read", "/Users/" + user, "NFSHomeDirectory")
        home = Path(entry.split(": ", 1)[1])
        require(home == Path("/Users") / user, "unexpected-home")
        homes.append(home)
    require({x.name for x in Path("/Users").iterdir()} <= actual | {"Shared", ".localized"},
            "unknown-home")
    for home in homes:
        require(home.is_dir() and not home.is_symlink(), "invalid-home")
    audit_inputs_outside_homes(homes, [__file__, args.home_baseline])
    if args.clean_transient:
        for home in homes:
            for name in (".bash_history", ".zsh_history", ".python_history", ".lesshst"):
                path = home / name
                require(not path.is_symlink(), "history-link")
                if path.exists():
                    require(path.is_file(), "history-not-file")
                    path.unlink()
    with open(args.home_baseline) as f:
        baseline = json.load(f)
    require(isinstance(baseline, dict) and all(isinstance(k, str) and isinstance(v, str)
            and re.fullmatch(r"[0-9a-f]{64}", v) for k, v in baseline.items()), "invalid-baseline")
    audit_homes(homes, baseline)
    for root in ("/private/tmp", "/private/var/tmp", "/private/var/run"):
        for item in Path(root).iterdir():
            require(not item.name.startswith(("hypeman-build-", "profile", *args.runtime_state_prefix)), "runtime-state")
    require(not list(Path("/private/etc/ssh").glob("ssh_host_*")), "host-ssh-keys")
    require(not Path("/Library/Preferences/com.apple.loginwindow.plist").exists(), "review-autologin-state")
    require(not Path("/Library/Managed Preferences").exists(), "managed-device-state")
    snapshots = command("/usr/bin/tmutil", "listlocalsnapshots", "/")
    require(not any("com.apple.TimeMachine." in line for line in snapshots.splitlines()), "local-snapshots")
    toolchain = command("/usr/bin/xcodebuild", "-version")
    require(re.fullmatch(r"Xcode [0-9.]+\nBuild version [a-zA-Z0-9]+", toolchain), "unknown-toolchain")
    tools = {"xcode": toolchain, "clang": command("/usr/bin/clang", "--version")}
    report = {"version": 1, "status": "pass", "architecture": "arm64",
              "os_version": command("/usr/bin/sw_vers", "-productVersion"),
              "os_build": command("/usr/bin/sw_vers", "-buildVersion"),
              "agent_sha256": args.agent_sha256, "plist_sha256": args.plist_sha256,
              "toolchain_sha256": hashlib.sha256(json.dumps(tools, sort_keys=True).encode()).hexdigest(),
              "toolchain_version": toolchain.replace("\n", "; "),
              "home_baseline_sha256": digest(args.home_baseline),
              "audit_script_sha256": digest(__file__)}
    print(json.dumps(report, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except Rejected as exc:
        print("base audit rejected: " + str(exc), file=sys.stderr)
        sys.exit(1)
    except Exception:
        print("base audit rejected: probe-error", file=sys.stderr)
        sys.exit(1)
