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
