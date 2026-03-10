# Test Agent Notes

## 2026-03-07 - Linux CI flake in `lib/instances`

### Flake signature
- Intermittent failure in `TestBasicEndToEnd`:
  - `start caddy: fork/exec .../system/binaries/caddy/v2.10.2/x86_64/caddy: text file busy`
- Observed during second full no-cache CI-equivalent run on `deft-kernel-dev` as root.

### Root cause
- Integration tests run in parallel and `prepareIntegrationTestDataDir` symlinks `tmpDir/system/binaries` to a shared prewarm directory.
- `lib/ingress/ExtractCaddyBinary` previously wrote directly to final binary path with `os.WriteFile`, so concurrent extraction/startup could race and produce ETXTBUSY.

### Fix
- In `lib/ingress/binaries_linux.go`:
  - Added extraction lock (`<binary>.lock` + `syscall.Flock`).
  - Switched binary + hash writes to temp-file + atomic rename.
  - Re-check binary/hash after acquiring lock.

### Validation commands used
- Tight loop:
  - `go test -tags containers_image_openpgp -run '^TestBasicEndToEnd$' -count=6 -timeout=25m ./lib/instances`
  - `go test -tags containers_image_openpgp -run '^(TestBasicEndToEnd|TestQEMUBasicEndToEnd)$' -count=4 -timeout=30m ./lib/instances`
- Full CI-equivalent flow (`go mod download`, `make oapi-generate`, `make build`, `go run ./cmd/test-prewarm`, `make test TEST_TIMEOUT=20m`) run with fresh caches each time.

### Full run durations (fresh caches)
- Pre-fix baseline:
  - Run 1: 181s (pass)
  - Run 2: 142s (flake)
- Post-fix full-suite verification:
  - Run 1: 139s (pass)
  - Run 2: 143s (pass)
  - Run 3: 141s (pass)

## 2026-03-07 - Additional no-cache flake under direct `go test`

### Flake signatures
- `TestFirecrackerNetworkLifecycle` intermittent failure 1:
  - `allocate network: get default network: network not found`
- `TestFirecrackerNetworkLifecycle` intermittent failure 2:
  - curl exit code `28` (timeout) when probing `https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html`.

### Root causes
- Bridge state readiness race after self-heal re-initialization could still fail immediate lookup.
- External internet dependency (S3 endpoint) introduced network flakiness unrelated to core networking behavior.

### Fixes
- `lib/network/allocate.go`
  - Added `getDefaultNetworkWithSelfHeal` with bounded short polling (2s total, 100ms interval) after self-heal init.
  - Applied to both `CreateAllocation` and `RecreateAllocation`.
- `lib/instances/firecracker_test.go`
  - Replaced remote S3 curl dependency with local deterministic probe server bound to the bridge gateway.
  - Kept pre/post-standby connectivity assertions through guest `curl` with retry.

### Final required gate (no-cache, 3 consecutive full runs)
- Command shape per run:
  - `go mod download`
  - `make oapi-generate`
  - `make build`
  - `go run ./cmd/test-prewarm`
  - `go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`
- Durations:
  - Run 1: 118s (pass)
  - Run 2: 230s (pass)
  - Run 3: 153s (pass)

## 2026-03-07 - Rerun round: redundancy + longest-test speed improvements

### Fresh full no-cache baseline before new changes
- Full flow (same as CI prep + direct no-cache test):
  - `go mod download`
  - `make oapi-generate`
  - `make build`
  - `go run ./cmd/test-prewarm`
  - `go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`
- Results:
  - Run 1: 143s (pass)
  - Run 2: 153s (pass)

### Slow test analysis (>2s)
- Package-level bottlenecks were `lib/images` (~6-8s) and `lib/instances` (~99s+).
- Longest individual tests (single-test baseline):
  - `TestForkCloudHypervisorFromRunningNetwork`: 53.35s
  - `TestQEMUForkFromRunningNetwork`: 46.87s
  - `TestFirecrackerForkFromRunningNetwork`: 36.69s

### Redundancy found and removed
- Duplicate source reachability assertions in running-fork tests:
  - `lib/instances/fork_test.go` (CloudHypervisor case)
  - `lib/instances/qemu_test.go`
  - `lib/instances/firecracker_test.go`
- Removed one duplicate `assertHostCanReachNginx(sourceAfterFork...)` in each.

### Longest-test speed fix
- In `lib/instances/fork_test.go`, reduced per-attempt guest-agent wait in `execInInstance`:
  - `WaitForAgent: 30s` -> `5s`
- Why it mattered:
  - `assertGuestHasOnlyExpectedIPv4` already does bounded polling. A 30s wait per attempt caused large stalls in the longest test while guest-agent was still coming up.

### Tight-loop validation after changes
- `go test -count=1 -tags containers_image_openpgp -run '^(TestForkCloudHypervisorFromRunningNetwork|TestQEMUForkFromRunningNetwork|TestFirecrackerForkFromRunningNetwork)$' -count=3 -timeout=30m ./lib/instances`
  - Pass, package time 84.182s.

### Post-fix single-test durations
- `TestForkCloudHypervisorFromRunningNetwork`: 24.51s (from 53.35s)
- `TestQEMUForkFromRunningNetwork`: 11.18s (from 46.87s)
- `TestFirecrackerForkFromRunningNetwork`: 28.50s (from 36.69s)

### Required pre-commit gate (3 consecutive full no-cache runs)
- Run 1: 82s (pass)
- Run 2: 103s (pass)
- Run 3: 97s (pass)
- `lib/instances` package runtime in those runs:
  - 57.806s, 79.853s, 73.199s

## 2026-03-08 - Rerun round (again): focused longest-test tuning

### Fresh baseline no-cache full runs (before new changes)
- Run 1: 88s (pass)
- Run 2: 98s (pass)

### What was analyzed
- Re-profiled slow tests in `lib/instances`; longest remained running-network fork integration tests.
- Tried a broader change (parallel source/fork reachability checks + additional guest-agent log wait) and observed regression/flakiness in tight loop (`[guest-agent] listening` log not reliably present in streamed logs). That experiment was reverted.

### Final change kept
- `lib/instances/fork_test.go`
  - In `execInInstance`, changed `WaitForAgent` from `5s` to `2s`.
  - This path is used by `assertGuestHasOnlyExpectedIPv4` in the Cloud Hypervisor running-fork test and still uses bounded polling around command execution.

### Tight-loop validation for targeted long tests
- Command:
  - `go test -count=1 -tags containers_image_openpgp -run '^(TestForkCloudHypervisorFromRunningNetwork|TestQEMUForkFromRunningNetwork|TestFirecrackerForkFromRunningNetwork)$' -count=3 -timeout=30m ./lib/instances`
- Result:
  - Pass; package runtime 102.528s.

### Isolated longest-test samples after final change
- `TestForkCloudHypervisorFromRunningNetwork`: 26.14s
- `TestQEMUForkFromRunningNetwork`: 11.09s
- `TestFirecrackerForkFromRunningNetwork`: 27.58s

### Required pre-commit gate (3 consecutive full no-cache runs)
- Run 1: 121s (pass)
- Run 2: 141s (pass)
- Run 3: 96s (pass)
- `lib/instances` runtime in those runs:
  - 97.618s, 117.392s, 71.886s
