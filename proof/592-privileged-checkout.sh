#!/usr/bin/env bash
# Asserts the privileged (self-hosted) jobs in .github/workflows/test.yml test
# the commit the dispatch names, that only fork-checks checks out a foreign
# repository, and that the fork gate still skips every privileged job.
# Usage: bash proof/592-privileged-checkout.sh [repo-root]
set -euo pipefail
root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
python3 - "$root" <<'PY'
import sys, yaml

root = sys.argv[1].rstrip("/")
wf = yaml.safe_load(open(f"{root}/.github/workflows/test.yml"))
cmd = yaml.safe_load(open(f"{root}/.github/workflows/test-command.yml"))
jobs = wf["jobs"]
fails, checks = [], 0


def check(ok, what):
    global checks
    checks += 1
    print(("ok   " if ok else "FAIL ") + what)
    if not ok:
        fails.append(what)


def runners(job):
    r = job.get("runs-on", "")
    return r if isinstance(r, list) else [r]


def checkouts(job):
    return [s for s in job.get("steps", []) if "actions/checkout" in str(s.get("uses", ""))]


privileged = [n for n, j in jobs.items() if "self-hosted" in runners(j)]
check(privileged == ["test", "test-darwin", "e2e-install"],
      f"privileged jobs are test, test-darwin, e2e-install (got {privileged})")

REF = "${{ inputs.pr_head_sha || github.ref }}"
for name in privileged:
    cos = checkouts(jobs[name])
    check(len(cos) == 1, f"{name}: exactly one checkout")
    with_ = cos[0].get("with", {}) or {}
    check(with_.get("ref") == REF, f"{name}: checkout ref is '{REF}' (got {with_.get('ref')!r})")
    check("inputs.pr_head_repo" not in str(with_.get("repository", "")),
          f"{name}: never checks out inputs.pr_head_repo")
    cond = str(jobs[name].get("if", ""))
    check("github.event_name != 'workflow_dispatch'" in cond
          and "inputs.pr_head_repo == ''" in cond
          and "inputs.pr_head_repo == github.repository" in cond,
          f"{name}: fork gate unchanged (skips a dispatch that names a fork)")

foreign = [n for n, j in jobs.items()
           if any("inputs.pr_head_repo" in str((s.get("with") or {}).get("repository", ""))
                  for s in checkouts(j))]
check(foreign == ["fork-checks"], f"only fork-checks checks out a foreign repo (got {foreign})")
fc = checkouts(jobs["fork-checks"])[0].get("with", {})
check(fc.get("ref") == "${{ inputs.pr_head_sha }}", "fork-checks pins the dispatched sha")
check("self-hosted" not in runners(jobs["fork-checks"]), "fork-checks stays on a hosted runner")

# report-pr-test must not label a run with a commit it did not test.
rep = jobs["report-pr-test"]
check("test" in rep.get("needs", []), "report-pr-test waits for test")
env = rep["steps"][0].get("env", {})
check(env.get("TESTED_SHA") == "${{ needs.test.outputs.tested_sha }}",
      "report-pr-test reads the tested commit from test's output")
script = rep["steps"][0]["run"]
check("$TESTED_SHA" in script and "$EXPECTED_SHA" in script and "!= " in script,
      "report-pr-test compares tested commit with the reported commit")

t = jobs["test"]
check(t.get("outputs", {}).get("tested_sha") == "${{ steps.tested-sha.outputs.sha }}",
      "test exports the commit it checked out")
check(any(s.get("id") == "tested-sha" and "git rev-parse HEAD" in s.get("run", "")
          for s in t["steps"]), "test records HEAD after checkout")

# test-command.yml supplies the fields the binding depends on.
text = open(f"{root}/.github/workflows/test-command.yml").read()
for field in ("pr_head_repo", "pr_head_sha", "pr_number"):
    check(f"--field {field}=" in text, f"test-command.yml dispatches {field}")

# Expression shape: push -> pushed ref, same-repo dispatch -> PR commit.
def resolve(expr, inputs, github):
    for part in expr.replace("${{", "").replace("}}", "").split("||"):
        part = part.strip()
        ns, _, key = part.partition(".")
        val = {"inputs": inputs, "github": github}.get(ns, {}).get(key, "")
        if val:
            return val
    return ""

gh = {"ref": "refs/heads/feature"}
check(resolve(REF, {}, gh) == "refs/heads/feature", "push run resolves to the pushed ref")
check(resolve(REF, {"pr_head_sha": "a" * 40}, gh) == "a" * 40,
      "same-repo dispatch resolves to the PR commit, not main")
check(resolve(REF, {"pr_head_sha": "a" * 40}, gh) != "refs/heads/main",
      "same-repo dispatch is not reported from main")

print(f"\n{checks - len(fails)}/{checks} checks passed")
sys.exit(1 if fails else 0)
PY
