# Hypeman

[![Test](https://github.com/onkernel/hypeman/actions/workflows/test.yml/badge.svg)](https://github.com/onkernel/hypeman/actions/workflows/test.yml)

Run containerized workloads in VMs, powered by [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor).

## Getting Started

### Prerequisites

**Go 1.25.4+**, **Cloud Hypervisor**, **KVM**, **erofs-utils**

```bash
cloud-hypervisor --version
mkfs.erofs --version
```

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

```bash
make test
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
