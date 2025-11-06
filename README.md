# Cloud Hypervisor Dataplane API

A generic dataplane API for managing VM lifecycle using Cloud Hypervisor with OCI-based workloads. This project provides a clean, REST-ful interface for creating, managing, and orchestrating VMs from container images.

## Features

- **Generic Design**: Works with any OCI-compatible container image, not specific to browsers or any particular workload
- **OpenAPI 3.1 Specification**: Fully documented API with auto-generated server code
- **Cloud Hypervisor States**: Mirrors native CH states (Created, Running, Paused, Shutdown) plus Stopped and Standby for snapshot management
- **Instance Management**: Create, list, get, delete, standby, and restore VM instances
- **Image Management**: Pull OCI images and convert to bootable disks
- **Volume Management**: Create and attach persistent volumes
- **Resource Control**: Configure memory (with hotplug), vCPUs, environment variables
- **Snapshot Support**: Standby (pause + snapshot + delete VMM) and restore operations

## Architecture

### Instance States

- `Created` - VMM created but not started (Cloud Hypervisor native)
- `Running` - VM is actively running (Cloud Hypervisor native)
- `Paused` - VM is paused (Cloud Hypervisor native)
- `Shutdown` - VM shut down but VMM exists (Cloud Hypervisor native)
- `Stopped` - No VMM running, no snapshot exists
- `Standby` - No VMM running, snapshot exists (can be restored)

### Project Structure

```
cloud-hypervisor-poc/
├── cmd/dataplane/          # Main application entry point
│   ├── main.go            # Server setup and routing
│   └── config/            # Configuration management
├── lib/
│   ├── oapi/              # Generated OpenAPI code
│   ├── dataplane/         # Service implementation
│   ├── images/            # Image manager (stub)
│   ├── instances/         # Instance manager (stub)
│   └── volumes/           # Volume manager (stub)
├── openapi.yaml           # OpenAPI 3.1 specification
├── oapi-codegen.yaml      # Code generation config
└── Makefile               # Build automation
```

## Getting Started

### Prerequisites

- Go 1.25.4+
- Cloud Hypervisor
- containerd (for image operations)

### Installation

1. Clone the repository:
```bash
cd /home/debianuser/cloud-hypervisor-poc
```

2. Install dependencies and generate code:
```bash
make install-tools
make oapi-generate
```

3. Build the binary:
```bash
make build
```

### Running the Server

Start the server:
```bash
./bin/dataplane
```

Or use hot-reload for development:
```bash
make dev
```

The server will start on port 8080 (configurable via `PORT` environment variable).

### Configuration

Configure via environment variables:

- `PORT` - HTTP server port (default: 8080)
- `DATA_DIR` - Data directory for VMs, images, volumes (default: /var/lib/cloud-hypervisor-dataplane)
- `BRIDGE_NAME` - Network bridge name (default: vmbr0)
- `SUBNET_CIDR` - Subnet CIDR (default: 192.168.100.0/24)
- `SUBNET_GATEWAY` - Gateway IP (default: 192.168.100.1)
- `CONTAINERD_SOCKET` - containerd socket path (default: /run/containerd/containerd.sock)
- `JWT_SECRET` - JWT secret for authentication (optional)
- `DNS_SERVER` - DNS server IP (default: 1.1.1.1)

## API Documentation

### OpenAPI Specification

The full OpenAPI spec is available at:
- YAML: http://localhost:8080/spec.yaml
- JSON: http://localhost:8080/spec.json

### Quick API Reference

#### Health Check
```bash
GET /health
```

#### Images
```bash
GET  /images              # List all images
POST /images              # Pull and convert OCI image
GET  /images/{id}         # Get image details
DELETE /images/{id}       # Delete image
```

#### Instances
```bash
GET  /instances           # List all instances
POST /instances           # Create and start instance
GET  /instances/{id}      # Get instance details
DELETE /instances/{id}    # Stop and delete instance
POST /instances/{id}/standby   # Put in standby
POST /instances/{id}/restore   # Restore from standby
GET  /instances/{id}/logs      # Get logs (SSE)
POST /instances/{id}/volumes/{volumeId}    # Attach volume
DELETE /instances/{id}/volumes/{volumeId}  # Detach volume
```

#### Volumes
```bash
GET  /volumes             # List all volumes
POST /volumes             # Create volume
GET  /volumes/{id}        # Get volume details
DELETE /volumes/{id}      # Delete volume
```

### Example: Create Instance

```bash
curl -X POST http://localhost:8080/instances \
  -H "Content-Type: application/json" \
  -d '{
    "id": "my-instance",
    "name": "My Workload",
    "image": "img-nginx",
    "memory_mb": 2048,
    "memory_max_mb": 4096,
    "vcpus": 2,
    "env": {
      "NODE_ENV": "production"
    }
  }'
```

## Development

### Code Generation

After modifying `openapi.yaml`, regenerate the Go code:

```bash
make oapi-generate
```

### Testing

```bash
make test
```

### Building

```bash
make build
```

### Hot Reload Development

```bash
make dev
```

## Implementation Status

### ✅ Phase 1: API Specification (Complete)
- [x] OpenAPI 3.1 specification
- [x] Code generation setup
- [x] Server with graceful shutdown
- [x] Spec serving (/spec.yaml, /spec.json)
- [x] Health endpoint
- [x] Stub implementations for all managers

### 🚧 Phase 2: Implementation (Next Steps)
- [ ] Image Manager: OCI pull and conversion (containerd integration)
- [ ] Instance Manager: VM lifecycle with Cloud Hypervisor
- [ ] Network Manager: Bridge, TAP devices, IP allocation
- [ ] Standby/Restore: Snapshot management
- [ ] Volume Manager: Persistent storage
- [ ] Logging: Serial console capture and streaming

## POC Scripts

The `scripts/` directory contains working POC scripts that demonstrate the core functionality:
- `build-initrd.sh` - Build initial ramdisk and rootfs from OCI images
- `setup-host-network.sh` - Configure host networking
- `setup-vms.sh` - Create VM configurations
- `start-all-vms.sh` - Start VMs
- `standby-vm.sh` - Pause, snapshot, and delete VMM
- `restore-vm.sh` - Restore from snapshot

These scripts serve as the reference implementation for the actual managers.

## Design Principles

1. **Generic & Reusable**: Not tied to any specific workload type
2. **Clean State Model**: Direct mapping to Cloud Hypervisor states
3. **File-Based Storage**: Filesystem is the source of truth
4. **No Persistence Logic**: Dataplane is stateless; control plane handles persistence decisions
5. **OpenAPI-First**: Spec-driven development with code generation
6. **Incremental Implementation**: Build piece by piece with full tests

## License

TBD - Suitable for open source

## Contributing

This project follows the patterns established in `kernel/packages/api` for consistency with the existing codebase.

