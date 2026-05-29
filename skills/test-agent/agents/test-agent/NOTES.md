# Test Agent Notes

## 2026-04-30 - Deft firewall flakes in PR #203 CI

### Reported flake signatures
- CI job `73797479810` on runner `deft-6` failed with VM-to-host TCP timeouts:
  - `TestEgressProxyRewritesHTTPSHeaders`: curl exit 28 connecting from guest through the egress proxy to a host listener.
  - `TestFirecrackerNetworkLifecycle`: curl exit 28 probing a local server bound to the test bridge gateway.

### Root cause
- `kernel/infra` commit `ac9d62b` applied the `nftables_firewall` role to `deft-kernel-dev`.
- Deft's `inet kernel_firewall input` chain had policy `drop` and allowed only loopback, established traffic, ICMP, Tailscale SSH, and Tailscale mosh.
- Hypeman CI creates ephemeral test bridges named `hm*` and guest VMs must initiate TCP connections to host gateway services on random ports. Those packets hit the host input chain and were dropped by nftables before the test listeners saw them.
- Confirmed during a failed Firecracker run:
  - Host listener was bound and reachable locally on the bridge gateway.
  - Guest had a default route via the bridge gateway and a reachable ARP neighbor for it.
  - Guest TCP connect to the gateway timed out.
- PR #203 did not touch networking, so this was server configuration, not that PR.

### Fixes
- Infra fix in `kernel/infra`:
  - Added `nftables_trusted_input_interfaces` to the firewall role.
  - Set `deft-kernel-dev` to trust `hm*` input interfaces so Hypeman CI test VMs can reach host-local gateway services.
- Hypeman test hygiene fix:
  - `newParallelTestNetworkConfig` now deletes its `testNetworkByName` entry during cleanup so `go test -count=N` does not reuse a released network lease.
  - Firecracker test manager setup now registers `cleanupOrphanedProcesses` with `t.Cleanup`, so failed lifecycle tests do not leave VMM helper processes around.

### Validation
- Pre-fix root run on `deft-kernel-dev` reproduced `TestFirecrackerNetworkLifecycle` curl exit 28 immediately.
- With temporary infra-equivalent nft rule `iifname "hm*" accept`:
  - `sudo env ... go test -count=3 -v -tags containers_image_openpgp -run '^(TestFirecrackerNetworkLifecycle|TestEgressProxyRewritesHTTPSHeaders)$' -timeout=45m ./lib/instances`
  - Result after Hypeman test hygiene patch: pass, package runtime 63.526s.
- Infra validation:
  - `uv run ansible-playbook --syntax-check playbooks/manage-servers.yml --limit deft-kernel-dev --tags firewall`
  - `uv run ansible-playbook playbooks/manage-servers.yml --limit deft-kernel-dev --tags firewall --check --diff`
  - Check-mode diff rendered the expected `iifname "hm*" accept` rule and completed successfully.

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

## 2026-03-23 - Current branch verification (`codex/pr43-fix-image-and-reclaim`)

### What initially looked flaky but was actually command-shape drift
- A fresh direct run without test-prewarm env exported failed in `lib/instances` with several image waits timing out at 60s:
  - `TestEntrypointEnvVars`
  - `TestQEMUEntrypointEnvVars`
  - `TestQEMUForkFromRunningNetwork`
  - `TestQEMUStandbyAndRestore`
- Symptom:
  - image status still `pending` after 60 seconds
- Root cause:
  - I ran `go run ./cmd/test-prewarm` but forgot to export:
    - `HYPEMAN_TEST_PREWARM_DIR=/root/.cache/hypeman-ci/linux-x86_64`
    - `HYPEMAN_TEST_REGISTRY=127.0.0.1:5001`
  - Without those env vars, `integrationTestImageRef` in `lib/instances/test_prewarm_test.go` does not remap Docker Hub refs to the local registry mirror, so several tests pay cold remote-pull + queue time and the 60s readiness assumptions become invalid.
- Conclusion:
  - This was not a repository flake on the current branch; it was an incorrect manual command shape.

### Fresh-cache CI-like runs on `deft-kernel-dev`
- Remote workspace:
  - `~/hm43`
- Bootstrap:
  - `make ensure-ch-binaries ensure-firecracker-binaries ensure-caddy-binaries build-embedded`
- Smoke runs with fresh Go caches and correct prewarm env:
  - flow:
    - `go mod download`
    - `make oapi-generate`
    - `make build`
    - `go run ./cmd/test-prewarm`
    - `make test TEST_TIMEOUT=20m`
  - results:
    - Run 1: 221s (pass)
    - Run 2: 208s (pass)
- Strict no-cache runs with fresh Go caches and direct test execution:
  - flow:
    - `go mod download`
    - `make oapi-generate`
    - `make build`
    - `go run ./cmd/test-prewarm`
    - `go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`
  - results:
    - Run 1: 211s (pass)
    - Run 2: 210s (pass)

### Current slowest individual tests (`lib/instances`, `go test -json`)
- `TestCloudHypervisorStandbyRestoreCompressionScenarios`: 51.34s
- `TestForkCloudHypervisorFromRunningNetwork`: 44.64s
- `TestFirecrackerForkFromRunningNetwork`: 34.00s
- `TestFirecrackerStopClearsStaleSnapshot`: 31.59s
- `TestCreateInstanceWithNetwork`: 30.80s
- `TestCompressSnapshotMemoryFileReturnsContextCanceledWhenNativeProcessIsKilled`: 30.00s
- `TestQEMUStandbyRestoreCompressionScenarios`: 29.74s
- `TestFirecrackerNetworkLifecycle`: 28.98s
- `TestFirecrackerStandbyRestoreCompressionScenarios`: 27.91s
- `TestQEMUForkFromRunningNetwork`: 27.02s

### Assessment
- No current-branch flakes reproduced once the command shape matched the repo’s intended prewarm setup.
- The remaining cost is concentrated in `lib/instances` hypervisor integration coverage and looks mostly non-redundant:
  - running-fork coverage is split across CH / Firecracker / QEMU
  - standby/restore compression scenarios are split across hypervisors
  - network lifecycle / create-instance / end-to-end tests each cover different surfaces
- One environmental contributor on `deft-kernel-dev`:
  - `lz4` and `zstd` are not installed, so snapshot compression tests fall back to the Go implementation, which likely inflates compression-scenario timings.
- I did not make a test-quality code change in this pass because I did not find a low-risk redundancy or speed improvement that was clearly justified after the no-cache runs came back clean.

## 2026-03-25 - `hypeman` flake round on `~/hm4943`

### Reported flake signatures
- `TestVolumeMultiAttachReadOnly`
  - `exec-agent not ready for instance ... within 15s (last state: Initializing)`
- `TestVolumeFromArchive`

## 2026-04-06 - PR #184 standby compression delay branch (`codex/standby-compression-delay`)

### CI red signature
- Linux `test` job on PR [#184](https://github.com/kernel/hypeman/pull/184) failed while the other checks passed.
- Observed failures from the GitHub Actions log:
  - `TestQEMUStandbyRestoreCompressionScenarios`
  - `TestQEMUStandbyAndRestore`
  - `TestBasicEndToEnd`
  - `TestForkCloudHypervisorFromRunningNetwork`
- Failure shapes were integration stalls, not deterministic assertion failures:
  - `instance ... did not reach Running within 20s (last state: Initializing)`
  - `rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL`

### Investigation
- Initial stopgap of removing `t.Parallel()` from the new restart-recovery tests was rejected; that was the wrong direction and was not kept.
- Reproduced the branch on `deft-kernel-dev` using the CI-like Linux/root flow with correct prewarm env:
  - `go mod download`
  - `make oapi-generate`
  - `make build`
  - `go run ./cmd/test-prewarm`
  - `sudo env ... go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`
- Tight loop on the exact CI-failing tests did not reproduce a flake once the command shape matched CI and prewarm settings were correct.

### Root cause and fix
- The new standby compression recovery tests were unit-style tests but used `setupTestManager`, which pulls in much heavier integration-style manager setup than needed.
- That extra setup was unnecessary for these tests and added avoidable load to an already heavy `lib/instances` package.
- Fix:
  - Added a lightweight `newSnapshotCompressionTestManager` helper in `lib/instances/snapshot_compression_test.go`
  - Moved the new delayed-job and restart-recovery tests to that lightweight fixture
  - Restored `t.Parallel()` on the new recovery tests and subtests
- This keeps coverage and parallelism intact while removing needless setup cost.

### Validation
- Targeted stress loop after the fixture change:
  - `go test -count=20 -run '^(TestRecoverPendingStandbyCompressionJobs|TestStartCompressionJobDelayedCancellationRecordsSkipped)$' ./lib/instances`
  - Result: pass
- Deft full fresh-cache CI-like runs after the fix:
  - Run 1: pass (`lib/instances` 193.279s)
  - Run 2: pass (`lib/instances` 261.633s)
  - Run 3: pass (`lib/instances` 173.573s)

## 2026-04-07 - PR #184 follow-up CI round on `codex/standby-compression-delay`

### Initial CI red signature
- Linux `test` job failed on `TestDockerForwardChainRestored`.
- Failure:
  - `ensureDockerForwardJump should have restored the DOCKER-FORWARD jump`
  - raw `iptables -C FORWARD -j DOCKER-FORWARD` exited non-zero in the test after re-initialization.

### Root cause and fix
- The Docker-forward recovery path and the test both used plain `iptables` invocations with no wait for the xtables lock.
- Under parallel CI activity, a transient lock holder can cause checks/deletes/inserts to fail immediately and make the test observe a missing rule even though the recovery logic is otherwise correct.
- Fix:
  - Added a small `newIPTablesCommand` helper in `lib/network/bridge_linux.go` that uses `iptables -w 5 ...` with the existing `CAP_NET_ADMIN` setup.
  - Switched the bridge NAT/FORWARD rule management and `ensureDockerForwardJump` commands to that helper.
  - Updated `TestDockerForwardChainRestored` in `lib/instances/network_test.go` to use `iptables -w 5` for its direct host-global mutations/checks.

### Secondary flake surfaced during Deft reruns
- A subsequent Deft full-suite rerun exposed a post-restore guest exec race in `TestCloudHypervisorStandbyRestoreCompressionScenarios`:
  - `receive response (stdout=0, stderr=0): rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL`
- The compression integration harness was only waiting for the exec agent socket and then issuing marker reads/writes immediately after restore.
- Fix:
  - Added a no-op post-restore guest exec readiness probe in `waitForRunningAndExecReady`.
  - Added a small retry wrapper for the compression integration test’s guest marker read/write commands so transient post-restore transport resets do not fail the scenario immediately.

### Validation
- Deft targeted loop:
  - `go test -count=20 -run '^TestDockerForwardChainRestored$' -v ./lib/instances`
  - Result: pass
- Deft targeted loop:
  - `go test -count=10 -run '^TestCloudHypervisorStandbyRestoreCompressionScenarios$' -tags containers_image_openpgp -timeout=30m ./lib/instances`
  - Result: pass
- Local sanity:
  - `go test ./lib/instances -count=1`
  - Result: pass (`117.538s`)
  - `exec-agent not ready for instance ... within 15s (last state: Initializing)`

### Additional flakes reproduced during Deft full-suite verification
- `TestQEMUForkFromRunningNetwork`
  - source/fork instance did not reach `Running` within 20s on a busy Deft host
  - cleanup also risked a nil-pointer panic because the cleanup closure captured `source.Id` after `source` could be reassigned on failure
- `TestCreateInstance_AutoPullImage`
  - `start cloud-hypervisor: ... text file busy`

### Root causes
- Volume-backed integration tests still used a 15s exec-agent readiness budget even though guest-agent/vsock readiness can lag while the instance remains in `Initializing` under full-suite host contention.
- `waitForExecAgent` previously refused to probe until the manager reported `Running`, even though exec could already be reachable while boot-marker hydration was still catching up.
- QEMU running-fork tests had a too-tight 20s host-side `Running` budget for full-suite contention.
- Cloud Hypervisor startup could race a freshly extracted binary and hit transient `ETXTBSY`.

### Fixes
- `lib/instances/exec_test.go`
  - allowed `waitForExecAgent` to probe while instance state is either `Initializing` or `Running`
- `lib/instances/volumes_test.go`
  - widened exec-agent waits from 15s to 30s for:
    - writer in `TestVolumeMultiAttachReadOnly`
    - both readers in `TestVolumeMultiAttachReadOnly`
    - archive-backed instance in `TestVolumeFromArchive`
- `lib/instances/qemu_test.go`
  - captured `sourceID` before `t.Cleanup(...)`
  - widened three `waitForInstanceState(..., StateRunning, ...)` calls from 20s to 45s in `TestQEMUForkFromRunningNetwork`
- `lib/vmm/client.go`
  - added bounded retry-on-`ETXTBSY` / `"text file busy"` when starting Cloud Hypervisor

### Validation on `deft-kernel-dev`
- Targeted volume stress:
  - `go test -count=20 -v -tags containers_image_openpgp -run '^(TestVolumeMultiAttachReadOnly|TestVolumeFromArchive)$' -timeout=60m ./lib/instances`
  - result: pass, package time `169.911s`
- Targeted QEMU fork verification:
  - `go test -count=4 -v -tags containers_image_openpgp -run '^TestQEMUForkFromRunningNetwork$' -timeout=45m ./lib/instances`
  - result: pass, package time `57.699s`
- Targeted API verification for CH startup retry:
  - `go test -count=10 -v -tags containers_image_openpgp -run '^TestCreateInstance_AutoPullImage$' -timeout=45m ./cmd/api/api`
  - result: pass, package time `34.761s`
- Fresh-cache full-suite verification (`go mod download`, `make oapi-generate`, `make build`, `go run ./cmd/test-prewarm`, `go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`)
  - Run 1: `215s` (pass)
  - Run 2: `212s` (pass)

### Verification note
- One intermediate Deft rerun failed before tests because the remote scratch workspace had stray untracked files at repo-root `lib/` (`client.go`, `exec_test.go`, `qemu_test.go`) that created a mixed-package directory.
- After removing those remote-only artifacts, the same fresh-cache full-suite gate passed twice.

## 2026-03-25 - Follow-up flake: CH compression standby/restore exec reset

### Flake signature
- `TestCloudHypervisorStandbyRestoreCompressionScenarios`
  - failure while writing a guest marker before standby:
    - `receive response (stdout=0, stderr=0): rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL`

### Root cause
- Standby/restore keeps the same guest-agent vsock identity for Cloud Hypervisor (`ch:<vsock-socket-path>`), and guest gRPC connections are pooled by that key.
- `standbyInstance` did not evict the pooled guest-agent connection when the VM transitioned to `Standby`, so post-restore execs could briefly reuse a connection tied to the dead pre-standby VM.
- That stale-connection reuse is consistent with the observed one-shot gRPC stream reset in `writeGuestMarker`.

### Fix
- `lib/instances/standby.go`
  - after shutting down the hypervisor and cleaning stale vsock sockets, explicitly remove the pooled guest-agent connection with `guest.CloseConn(dialer.Key())`
  - this forces restore-time execs to establish a fresh gRPC connection against the resumed VM

### Validation on `deft-kernel-dev`
- Targeted stress:
  - `go test -count=20 -v -tags containers_image_openpgp -run '^TestCloudHypervisorStandbyRestoreCompressionScenarios$' -timeout=90m ./lib/instances`
  - result: pass, package time `740.546s`
- I also started a fresh-cache full-suite Deft verification run after this fix, but it was intentionally interrupted before completion when switching to commit/push.

## 2026-03-25 - Follow-up CI flakes after `771018f`

### Flake signatures
- `cmd/api/api/TestCreateInstance_AutoPullImage`
  - auto-pull failed while fetching Docker Hub auth token:
    - `resolve manifest: fetch manifest: Get "https://auth.docker.io/token?...": context deadline exceeded`
- `lib/builds/TestGetBuild_Found`
  - expected `queued`, got `building`

### Root causes
- `TestCreateInstance_AutoPullImage` still used the raw Docker Hub ref instead of the API test registry mirror helper, so it depended on live Docker Hub auth latency even though CI had prewarm + local registry configured.
- `TestGetBuild_Found` asserted the build must still be `queued`, but the queue worker can legitimately transition it to `building` before the test reads it back.

### Fixes
- `cmd/api/api/instances_test.go`
  - switched the auto-pull image ref to `apiTestImageRef(t, "docker.io/library/alpine:latest")`
- `lib/builds/manager_test.go`
  - relaxed the status assertion to accept either `queued` or `building`

### Validation on `deft-kernel-dev`
- `go test -count=10 -v -tags containers_image_openpgp -run '^TestCreateInstance_AutoPullImage$' -timeout=45m ./cmd/api/api`
  - pass, package time `28.484s`
- `go test -count=50 -run '^TestGetBuild_Found$' ./lib/builds`
  - pass

## 2026-05-29 - Main branch Linux flake sweep from `34e1032`

### Starting point
- Local workspace was detached on `aa65a64` and behind `origin/main` by two commits.
- Fetched `origin/main` and switched to detached `origin/main` at `34e1032` (`Count shared snapshot extents separately (#244)`), then branched for fixes.
- Recent main CI failures on `kernel/hypeman` showed Linux flake signatures around guest/vsock readiness:
  - latest main Test run `26531491771`: `TestVolumeFromArchive` failed waiting for exec-agent, last state `Stopped`, EOF
  - prior main failures included `TestQEMUBasicEndToEnd`, `TestCpDirectoryToInstance`, `TestInstanceLifecycle_StopStart`, `TestExecInstanceNonTTY`, and `TestVolumeMultiAttachReadOnly`

### Deft host notes
- `deft-kernel-dev` had plenty of disk/RAM (`/mnt/data` around 3.4T free, memory around 765Gi available), but high CPU load during investigation.
- The standard shared prewarm dir `~/.cache/hypeman-ci/linux-amd64` failed due a permission issue under the cache tree, so this run used workspace-local prewarm:
  - `HYPEMAN_TEST_PREWARM_DIR=$PWD/.hypeman-prewarm/linux-amd64`
  - `HYPEMAN_TEST_REGISTRY=127.0.0.1:5001`
  - `HYPEMAN_TEST_PREWARM_STRICT=1`
- With permission, cleaned only stale Hypeman temp hypervisor artifacts matching `/tmp/Test*` or `/tmp/hmcmp-*`. No Docker/global cleanup was done.

### Fixes
- `lib/guest/client.go`
  - no-wait exec now evicts the pooled guest gRPC connection on retryable connection errors, matching the existing `WaitForAgent` retry path
  - prevents outer test/helper retry loops from repeatedly reusing a poisoned connection after transient vsock EOF/timeouts
- `lib/guest/client_test.go`
  - added `TestExecIntoInstanceNoWaitClosesRetryableConnection`
- `lib/builds/manager.go`
  - `waitForResult` treats a nil vsock dialer with nil error as unavailable instead of panicking
- `lib/builds/manager_test.go`
  - mock `GetVsockDialer` now returns an explicit error unless configured
- `lib/instances/restart_policy.go`
  - stable-window reset uses the later of `StartedAt` and `RestartStatus.LastAttemptAt`
  - prevents controller reconciliation from clearing attempts while a health-check restart attempt is still in flight
- `lib/instances/restart_policy_test.go`
  - added coverage for old `StartedAt` plus recent `LastAttemptAt`
- `lib/instances/firecracker_test.go`
  - `TestFirecrackerForkIsolation` now measures disk delta using per-workspace `diskutilization.Collect` instead of global `statfs` free-space changes, avoiding unrelated shared-host disk activity

### Targeted validation on `deft-kernel-dev`
- Reproduced `TestVolumeFromArchive` before the guest connection-pool fix:
  - `go test -count=5 -v -tags containers_image_openpgp -run '^TestVolumeFromArchive$' -timeout=20m ./lib/instances`
  - failed 1/5 with vsock EOF from exec-agent readiness
- After guest connection-pool fix:
  - `go test ./lib/guest`
  - pass
  - `go test -count=10 -v -tags containers_image_openpgp -run '^TestVolumeFromArchive$' -timeout=30m ./lib/instances`
  - pass, package time `242.742s`
- After build nil-dialer fix:
  - `go test ./lib/guest ./lib/builds`
  - pass, `lib/builds` package time `86.525s`
- API/image failures from an intermediate full run were not durable:
  - `go test -count=3 -v -tags containers_image_openpgp -run '^(TestCpToAndFromInstance|TestExecWithDebianMinimal|TestRegistryLayerCaching)$' -timeout=20m ./cmd/api/api`
  - pass, package time `128.899s`
- Restart policy network flake:
  - pre-fix `go test -count=5 -v -tags containers_image_openpgp -run '^TestCreateInstanceWithNetwork$' -timeout=20m ./lib/instances` exposed `restart policy stable window reached` while restart attempt was still stopping/starting
  - post-fix `go test -count=1 -v -tags containers_image_openpgp -run '^TestShouldResetRestartAttempts|^TestCreateInstanceWithNetwork$' -timeout=20m ./lib/instances`
  - pass, package time `79.157s`
- Firecracker fork isolation:
  - pre-fix full suite failed with `consumed=5864284160 guestMem=1073741824 reflink=true`
  - post-fix `go test -count=3 -v -tags containers_image_openpgp -run '^TestFirecrackerForkIsolation$' -timeout=20m ./lib/instances`
  - pass, package time `60.894s`, per-workspace deltas around 137-150MiB

### Full-suite validation on `deft-kernel-dev`
- Intermediate full run after the first three fixes:
  - `go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`
  - failed only `lib/instances/TestFirecrackerForkIsolation` due global `statfs` free-space measurement on shared host
  - log: `full-suite-20260529-173833.log`, duration `256s`
- Full run after Firecracker disk measurement fix:
  - `go test -count=1 -tags containers_image_openpgp -timeout=20m ./...`
  - Run 1: `258s` (pass)
  - log: `full-suite-20260529-174512.log`

## 2026-05-30 - PR #247 Linux CI follow-up

### CI failure
- Draft PR #247 failed the Linux `test` job on run `26663998916`, job `78593114552`, runner `deft-8`, commit `f17ed89`.
- The same signature appeared on the previous pre-amend run `26663971132` on `deft-14`.
- Failure was in `lib/instances/TestBasicEndToEnd`:
  - `manager_test.go:373: Nginx should have started worker processes within 20 seconds`
  - The test then successfully routed through Caddy and got an nginx response (`Got response from nginx through Caddy: 615 bytes`).
- Deft did not show disk or RAM exhaustion in the CI evidence. The useful signal was a log-readiness race: the nginx process was serving, but the test made the startup log line a hard assertion before the ingress probe.

### Fixes
- `lib/instances/manager_test.go`
  - changed the nginx startup log wait in `TestBasicEndToEnd` from a hard assertion into diagnostic logging
  - kept the external ingress HTTP request as the authoritative readiness/behavior check
- `lib/instances/qemu_test.go`
  - made the same change in `TestQEMUBasicEndToEnd`, which had the same brittle log assertion pattern

### Targeted validation on `deft-kernel-dev`
- `go test -count=3 -v -tags containers_image_openpgp -run "^(TestBasicEndToEnd|TestQEMUBasicEndToEnd)$" -timeout=30m ./lib/instances`
  - pass, package time `27.583s`
