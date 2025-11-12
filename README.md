# Hypeman

[![Test](https://github.com/onkernel/hypeman/actions/workflows/test.yml/badge.svg)](https://github.com/onkernel/hypeman/actions/workflows/test.yml)

Run containerized workloads in VMs, powered by [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor).

## Getting Started

### Prerequisites

**Go 1.25.4+**, **KVM**, **erofs-utils**, **dnsmasq**

```bash
# Verify prerequisites
mkfs.erofs --version
dnsmasq --version
```

**Install on Debian/Ubuntu:**
```bash
sudo apt-get install erofs-utils dnsmasq
```

**KVM Access:** User must be in `kvm` group for VM access:
```bash
sudo usermod -aG kvm $USER
# Log out and back in, or use: newgrp kvm
```

**Network Capabilities:** The hypeman binary needs network administration capabilities to create bridges and TAP devices:
```bash
# After building, grant network capabilities
sudo setcap 'cap_net_admin,cap_net_bind_service=+ep' /path/to/hypeman

# For development builds
sudo setcap 'cap_net_admin,cap_net_bind_service=+ep' ./bin/hypeman

# Verify capabilities
getcap ./bin/hypeman
```

**Note:** These capabilities must be reapplied after each rebuild. For production deployments, set capabilities on the installed binary.

### Configuration

#### Environment variables

```bash
cp .env.example .env
# Edit .env and set JWT_SECRET
```

#### Data directory

Hypeman stores data in a configurable directory. Configure permissions for this directory.

```bash
sudo mkdir /var/lib/hypeman
sudo chown $USER:$USER /var/lib/hypeman
```

#### Dockerhub login

Requires Docker Hub authentication to avoid rate limits when running the tests:
```bash
docker login
```

Docker itself isn't required to be installed. `~/.docker/config.json` is a standard used for handling registry authentication.

### Build

```bash
make build
```
### Running the Server

1. Copy the example environment file and modify the values:
```bash
cp .env.example .env
# Edit .env and set JWT_SECRET and other configuration values
```

2. Generate a JWT token for testing (optional):
```bash
make gen-jwt
```

3. Start the server with hot-reload for development:
```bash
make dev
```
The server will start on port 8080 (configurable via `PORT` environment variable).

### Testing

Network tests require elevated permissions to create bridges and TAP devices.

```bash
make test
```

The test command compiles test binaries, grants capabilities via `sudo setcap`, then runs tests as the current user (not root). You may be prompted for your sudo password during the capability grant step.

**Cleanup stale resources** (if tests were killed with Ctrl+C):
```bash
./scripts/cleanup-test-networks.sh
```

### Code Generation

After modifying `openapi.yaml`, regenerate the Go code:

```bash
make oapi-generate
```

After modifying dependency injection in `cmd/api/wire.go` or `lib/providers/providers.go`, regenerate wire code:

```bash
make generate-wire
```

Or generate everything at once:

```bash
make generate-all
```
