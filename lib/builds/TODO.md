# Build System TODOs

Outstanding issues and improvements for the build system.

## ✅ High Priority - Security & Bugs (Completed)

### 1. ~~IP Spoofing Vulnerability~~ ✅ FIXED

**File:** `lib/middleware/oapi_auth.go`

**Issue:** The `isInternalVMRequest` function was reading the `X-Real-IP` header directly from the client request.

**Fix:** Changed to only use `r.RemoteAddr` as the authoritative source. Added security comment explaining why headers should not be trusted.

---

### 2. ~~Registry Token Scope Leakage~~ ✅ FIXED

**File:** `lib/middleware/oapi_auth.go`

**Issue:** Registry tokens could potentially be used on non-registry endpoints.

**Fix:** Both `JwtAuth` middleware and `OapiAuthenticationFunc` now reject tokens with registry-specific claims (`repos`, `scope`, `build_id`) when used for non-registry API authentication.

---

### 3. ~~Missing Read Deadline on Vsock~~ ✅ ALREADY FIXED

**File:** `lib/builds/manager.go`

**Issue:** The `waitForResult` function blocked indefinitely on `decoder.Decode()`.

**Status:** Already implemented with goroutine pattern + connection close on context cancellation (lines 455-486).

---

## 🟡 Medium Priority - Implementation TODOs

### 4. ~~SSE Streaming Implementation~~ ✅ DONE

**Files:** `cmd/api/api/builds.go`, `lib/builds/manager.go`, `lib/builds/types.go`

**Status:** Implemented proper SSE streaming with:
- `BuildEvent` type with `log`, `status`, and `heartbeat` event types
- `StreamBuildEvents` method in Manager with real-time log tailing via `tail -f`
- Status subscription system for broadcasting status changes to SSE clients
- Heartbeat events every 30 seconds in follow mode
- `follow` query parameter support
- Unit tests for all streaming scenarios

---

### 5. ~~Build Secrets~~ ✅ DONE

**Files:** `lib/builds/manager.go`, `lib/builds/builder_agent/main.go`, `lib/builds/file_secret_provider.go`, `cmd/api/api/builds.go`

**Status:** Implemented secure secret injection via vsock:
- Host sends `host_ready` message when connected to builder agent
- Agent requests secrets with `get_secrets` message containing secret IDs
- Host responds with `secrets_response` containing secret values from `SecretProvider`
- Agent writes secrets to `/run/secrets/{id}` for BuildKit consumption
- `FileSecretProvider` reads secrets from a configurable directory
- Unit tests for `FileSecretProvider` with path traversal protection
- **Fixed vsock protocol deadlock** - agent now proactively sends `build_result` when complete
- Added `secrets` field to POST `/builds` API endpoint (JSON array of `{"id": "..."}` objects)
- E2E tested: builds complete successfully and logs stream via SSE

---

## 🟢 Low Priority - Improvements

### 6. ~~E2E Test Enhancement~~ ✅ DONE

**File:** `scripts/e2e-build-test.sh`

**Status:** Enhanced to run a VM with the built image after successful build. The test now:
- Creates an instance from the built image
- Waits for the instance to start
- Executes a test command inside the instance
- Cleans up the instance
- Use `--skip-run` flag to skip the VM test

### 7. ~~Build Manager Unit Tests~~ ✅ DONE

**File:** `lib/builds/manager_test.go`

**Status:** Added comprehensive unit tests with mocked dependencies:
- `TestCreateBuild_Success` - Happy path build creation
- `TestCreateBuild_WithBuildPolicy` - Build with custom policy
- `TestGetBuild_Found/NotFound` - Build retrieval
- `TestListBuilds_Empty/WithBuilds` - Listing builds
- `TestCancelBuild_*` - Cancel scenarios (queued, not found, completed)
- `TestGetBuildLogs_*` - Log retrieval
- `TestBuildQueue_ConcurrencyLimit` - Queue concurrency
- `TestUpdateStatus_*` - Status updates with errors
- `TestRegistryTokenGeneration` - Token generation verification
- `TestCreateBuild_MultipleConcurrent` - Concurrent build creation

### 8. Enable cgroups for BuildKit Secrets

**Issue:** When `--secret` flags are passed to BuildKit, runc requires cgroup mounts that aren't present in the microVM.

**Error:** `runc run failed: no cgroup mount found in mountinfo`

**Status:** The secrets API flow works correctly (host → vsock → agent → BuildKit flags), but BuildKit execution fails due to missing cgroups.

**Potential Fixes:**
- Enable cgroup support in the microVM kernel/rootfs
- Use BuildKit in rootless mode without cgroup dependency
- Configure BuildKit to use a different runc execution mode

**Workaround:** Builds without secrets work fine. The secrets code is ready once cgroups are enabled.

### 9. Guest Agent on Builder VMs

**Suggestion:** Run the guest-agent on builder VMs to enable `exec` into failed builds for debugging.

### 10. Builder Image Tooling

**File:** `lib/builds/images/README.md`

**Suggestion:** Create a script or tooling for building and publishing new builder images.

---

## ✅ Completed

- [x] Remove deprecated `RuntimeNodeJS20` and `RuntimePython312` constants
- [x] Remove `Runtime` field from API and storage
- [x] Remove `ToolchainVersion` from `BuildProvenance`
- [x] Update OpenAPI spec to remove runtime field
- [x] Rename `/builds/{id}/logs` to `/builds/{id}/events` with typed events
- [x] Remove unused `deref` function
- [x] Update documentation (README.md, PLAN.md)
- [x] Fix context leak in volume cleanup (use `context.Background()`)
- [x] Fix incorrect error wrapping in config volume setup
- [x] Fix IP spoofing vulnerability in `isInternalVMRequest`
- [x] Add registry token rejection to `OapiAuthenticationFunc`
- [x] Verify vsock read deadline handling (already fixed with goroutine pattern)
- [x] E2E test enhancement - run VM with built image
- [x] Build manager unit tests with mocked dependencies
- [x] SSE streaming implementation with typed events, follow mode, and heartbeats
- [x] Build secrets via vsock with FileSecretProvider

