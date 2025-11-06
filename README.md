# Cloud Hypervisor Dataplane API

[![Test](https://github.com/onkernel/cloud-hypervisor-dataplane/actions/workflows/test.yml/badge.svg)](https://github.com/onkernel/cloud-hypervisor-dataplane/actions/workflows/test.yml)

Run containerized workloads in VMs, powered by [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor).

## Getting Started

### Prerequisites

- Go 1.25.4+
- Cloud Hypervisor
- containerd

### Build

```bash
make build
```
### Running the Server

Start the server with hot-reload for development:
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
