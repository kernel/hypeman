# Cloud Hypervisor VMM Client

Thin Go wrapper around Cloud Hypervisor's HTTP API (v0.3.0) with embedded binaries.

## Features

- Embedded Cloud Hypervisor binaries (v48.0, v49.0) for x86_64 and aarch64
- Generated HTTP client from official OpenAPI spec
- Automatic binary extraction to data directory
- Unix socket communication
- Version detection and validation

## Requirements

**System:**
- Linux with KVM support (`/dev/kvm`)
- User must be in `kvm` group or run as root

**Add user to kvm group:**
```bash
sudo usermod -aG kvm $USER
# Then log out and back in, or use: sg kvm -c "your-command"
```

## Usage

### Start a VMM Process

```go
import "github.com/onkernel/hypeman/lib/vmm"

ctx := context.Background()
dataDir := "/var/lib/hypeman"
socketPath := "/tmp/vmm.sock"

// Start Cloud Hypervisor VMM (extracts binary if needed)
err := vmm.StartProcess(ctx, dataDir, vmm.V48_0, socketPath)
if err != nil {
    log.Fatal(err)
}
```

### Connect to Existing VMM

```go
// Create client for existing VMM socket
client, err := vmm.NewVMM(socketPath)
if err != nil {
    log.Fatal(err)
}

// All generated API methods available directly
resp, err := client.GetVmmPingWithResponse(ctx)
```

### Check Binary Version

```go
binaryPath, _ := vmm.GetBinaryPath(dataDir, vmm.V48_0)
version, err := vmm.ParseVersion(binaryPath)
fmt.Println(version) // "v48.0"
```

## Architecture

```
lib/vmm/
├── vmm.go              # Generated from OpenAPI spec (DO NOT EDIT)
├── client.go           # Thin wrapper: NewVMM, StartProcess
├── binaries.go         # Binary embedding and extraction
├── version.go          # Version parsing utilities
├── binaries/           # Embedded Cloud Hypervisor binaries
│   └── cloud-hypervisor/
│       ├── v48.0/
│       │   ├── x86_64/cloud-hypervisor    (4.5MB)
│       │   └── aarch64/cloud-hypervisor   (3.3MB)
│       └── v49.0/
│           ├── x86_64/cloud-hypervisor    (4.5MB)
│           └── aarch64/cloud-hypervisor   (3.3MB)
└── client_test.go      # Tests with real Cloud Hypervisor
```

## Supported Versions

- Cloud Hypervisor v48.0 (API v0.3.0)
- Cloud Hypervisor v49.0 (API v0.3.0)

## Regenerating Client

```bash
# Download latest spec (if needed)
make download-ch-spec

# Regenerate client from spec
make generate-vmm-client
```

## Testing

Tests run against real Cloud Hypervisor binaries (not mocked).

```bash
# Must be in kvm group
sg kvm -c "go test ./lib/vmm/..."
```

## Binary Extraction

Binaries are automatically extracted from embedded FS to:
```
{dataDir}/system/binaries/{version}/{arch}/cloud-hypervisor
```

Extraction happens once per version. Subsequent calls reuse the extracted binary.

## API Methods

All Cloud Hypervisor API methods available via embedded `*ClientWithResponses`:

- `GetVmmPingWithResponse()` - Check VMM health
- `ShutdownVMMWithResponse()` - Shutdown VMM
- `CreateVMWithResponse()` - Create VM configuration
- `BootVMWithResponse()` - Boot configured VM
- `PauseVMWithResponse()` - Pause running VM
- `ResumeVMWithResponse()` - Resume paused VM
- `VmSnapshotPutWithResponse()` - Create VM snapshot
- `VmRestorePutWithResponse()` - Restore from snapshot
- `VmInfoGetWithResponse()` - Get VM info
- And many more...

See generated `vmm.go` for full API surface.

