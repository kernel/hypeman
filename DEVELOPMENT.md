# Development Guide

This document covers development setup, configuration, and contributing to Hypeman.

## Prerequisites

### Linux (Default)

**Go 1.25.4+**, **KVM**, **erofs-utils**, **dnsmasq**

### macOS (Experimental)

See [macOS Development](#macos-development) below for native macOS development using Virtualization.framework.

---

**Linux Prerequisites:**

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

**Network Capabilities:**

Before running or testing Hypeman, ensure IPv4 forwarding is enabled:

```bash
# Enable IPv4 forwarding (temporary - until reboot)
sudo sysctl -w net.ipv4.ip_forward=1

# Enable IPv4 forwarding (persistent across reboots)
echo 'net.ipv4.ip_forward=1' | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

**Why:** Required for routing traffic between VM network and external network.

The hypeman binary needs network administration capabilities to create bridges and TAP devices:

```bash
# After building, grant network capabilities
sudo setcap 'cap_net_admin,cap_net_bind_service=+eip' /path/to/hypeman

# For development builds
sudo setcap 'cap_net_admin,cap_net_bind_service=+eip' ./bin/hypeman

# Verify capabilities
getcap ./bin/hypeman
```

**Note:** The `i` (inheritable) flag allows child processes spawned by hypeman (like `ip` and `iptables` commands) to inherit capabilities via the ambient capability set.

**Note:** These capabilities must be reapplied after each rebuild. For production deployments, set capabilities on the installed binary. For local testing, this is handled automatically in `make test`.

**File Descriptor Limits:**

Caddy (used for ingress) requires a higher file descriptor limit than the default on some systems. If you see "Too many open files" errors, increase the limit:

```bash
# Check current limit (also check with: sudo bash -c 'ulimit -n')
ulimit -n

# Increase temporarily (current session)
ulimit -n 65536

# For persistent changes, add to /etc/security/limits.conf:
*  soft  nofile  65536
*  hard  nofile  65536
root  soft  nofile  65536
root  hard  nofile  65536
```

## Configuration

### Config files

Hypeman reads configuration from a YAML config file. See `config.example.yaml` (Linux) and `config.example.darwin.yaml` (macOS) for all available settings with comments.

The config file is searched in these locations (first match wins):
- Path specified by `CONFIG_PATH` environment variable
- `/etc/hypeman/config.yaml` (Linux)
- `~/.config/hypeman/config.yaml` (all platforms)

### Environment variable overrides

Any config key can be overridden via environment variables. Top-level keys use `SCREAMING_SNAKE_CASE` directly. Nested keys use double-underscore (`__`) as the nesting separator:

| YAML path | Env var | Default |
|-----------|---------|---------|
| `port` | `PORT` | `8080` |
| `data_dir` | `DATA_DIR` | `/var/lib/hypeman` |
| `jwt_secret` | `JWT_SECRET` | _(empty)_ |
| `env` | `ENV` | `unset` |
| `network.bridge_name` | `NETWORK__BRIDGE_NAME` | `vmbr0` |
| `network.subnet_cidr` | `NETWORK__SUBNET_CIDR` | `10.100.0.0/16` |
| `network.uplink_interface` | `NETWORK__UPLINK_INTERFACE` | _(auto-detect)_ |
| `network.dns_server` | `NETWORK__DNS_SERVER` | `1.1.1.1` |
| `caddy.listen_address` | `CADDY__LISTEN_ADDRESS` | `0.0.0.0` |
| `caddy.admin_address` | `CADDY__ADMIN_ADDRESS` | `127.0.0.1` |
| `caddy.admin_port` | `CADDY__ADMIN_PORT` | `2019` |
| `caddy.stop_on_shutdown` | `CADDY__STOP_ON_SHUTDOWN` | `false` |
| `logging.level` | `LOGGING__LEVEL` | `info` |
| `otel.enabled` | `OTEL__ENABLED` | `false` |
| `otel.endpoint` | `OTEL__ENDPOINT` | `127.0.0.1:4317` |
| `limits.max_concurrent_builds` | `LIMITS__MAX_CONCURRENT_BUILDS` | `1` |
| `limits.max_overlay_size` | `LIMITS__MAX_OVERLAY_SIZE` | `100GB` |
| `acme.email` | `ACME__EMAIL` | _(empty)_ |
| `acme.dns_provider` | `ACME__DNS_PROVIDER` | _(empty)_ |
| `acme.cloudflare_api_token` | `ACME__CLOUDFLARE_API_TOKEN` | _(empty)_ |
| `build.docker_socket` | `BUILD__DOCKER_SOCKET` | `/var/run/docker.sock` |

The pattern is: nest with `__`, uppercase everything. For example, `caddy.listen_address` becomes `CADDY__LISTEN_ADDRESS`.

**Important: Subnet Configuration**

The default subnet `10.100.0.0/16` is chosen to avoid common conflicts. Hypeman will detect conflicts with existing routes on startup and fail with guidance.

If you need a different subnet, set `network.subnet_cidr` in your config file (or `NETWORK__SUBNET_CIDR` env var). The gateway is automatically derived as the first IP in the subnet (e.g., `10.100.0.0/16` → `10.100.0.1`).

**Alternative subnets if needed:**

- `172.30.0.0/16` - Private range between common Docker (172.17.x.x) and cloud provider (172.31.x.x) ranges
- `10.200.0.0/16` - Another private range option

**Example:**

```yaml
# In your config.yaml
network:
  subnet_cidr: 172.30.0.0/16
```

**Finding the uplink interface (`network.uplink_interface`)**

`network.uplink_interface` (env: `NETWORK__UPLINK_INTERFACE`) tells Hypeman which host interface to use for routing VM traffic to the outside world (for iptables MASQUERADE rules). On many hosts this is `eth0`, but laptops and more complex setups often use Wi-Fi or other names.

**Quick way to discover it:**

```bash
# Ask the kernel which interface is used to reach the internet
ip route get 1.1.1.1
```

Look for the `dev` field in the output, for example:

```text
1.1.1.1 via 192.168.12.1 dev wlp2s0 src 192.168.12.98
```

In this case, `wlp2s0` is the uplink interface, so you would set:

```bash
UPLINK_INTERFACE=wlp2s0
```

You can also inspect all routes:

```bash
ip route show
```

Pick the interface used by the default route (usually the line starting with `default`). Avoid using local bridges like `docker0`, `br-...`, `virbr0`, or `vmbr0` as the uplink; those are typically internal virtual networks, not your actual internet-facing interface.

### TLS Ingress (HTTPS)

Hypeman uses Caddy with automatic ACME certificates for TLS termination. Certificates are issued via DNS-01 challenges (Cloudflare).

To enable TLS ingresses:

1. Configure ACME credentials in your `config.yaml`:

```yaml
# Required for any TLS ingress
acme_email: admin@example.com

# For Cloudflare
acme_dns_provider: cloudflare
cloudflare_api_token: your-api-token
```

2. Create an ingress with TLS enabled:

```bash
curl -X POST http://localhost:8080/v1/ingresses \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-https-app",
    "rules": [{
      "match": {"hostname": "app.example.com", "port": 443},
      "target": {"instance": "my-instance", "port": 8080},
      "tls": true,
      "redirect_http": true
    }]
  }'
```

Certificates are stored in `$DATA_DIR/caddy/data/` and auto-renewed by Caddy.

### Setup

```bash
cp config.example.yaml ~/.config/hypeman/config.yaml
# Edit config.yaml and set jwt_secret and other configuration values
```

### Data directory

Hypeman stores data in a configurable directory. Configure permissions for this directory.

```bash
sudo mkdir /var/lib/hypeman
sudo chown $USER:$USER /var/lib/hypeman
```

### Dockerhub login

Requires Docker Hub authentication to avoid rate limits when running the tests:

```bash
docker login
```

Docker itself isn't required to be installed. `~/.docker/config.json` is a standard used for handling registry authentication.

## Build

```bash
make build
```

## Running the Server

1. Generate a JWT token for testing (optional):

```bash
make gen-jwt
```

2. Start the server with hot-reload for development:

```bash
make dev
```

The server will start on port 8080 (configurable via `PORT` environment variable).

### Setting Up the Builder Image (for Dockerfile builds)

The builder image is required for `hypeman build` to work. When `BUILDER_IMAGE` is unset or empty, the server will automatically build and push the builder image on startup using Docker. This is the easiest way to get started — just ensure Docker is available and run `make dev`. If a build is requested while the builder image is still being prepared, the server returns a clear error asking you to retry shortly.

On macOS with Colima, set the Docker socket path:
```bash
DOCKER_SOCKET=$HOME/.colima/default/docker.sock
```

### Local OpenTelemetry (optional)

To collect traces and metrics locally, run the Grafana LGTM stack (Loki, Grafana, Tempo, Mimir):

```bash
# Start Grafana LGTM (UI at http://localhost:3000, login: admin/admin)
# Note, if you are developing on a shared server, you can use the same LGTM stack as your peer(s)
# You will be able to sort your metrics, traces, and logs using the ENV configuration (see below)
BIND=127.0.0.1
# YOLO=1  # Uncomment to expose ports externally
if [ -n "$YOLO" ]; then BIND=0.0.0.0; fi

docker run -d --name lgtm \
  -p $BIND:3000:3000 \
  -p $BIND:4317:4317 \
  -p $BIND:4318:4318 \
  -p $BIND:9090:9090 \
  -p $BIND:4040:4040 \
  grafana/otel-lgtm:latest

# If developing on a remote server, forward the port to your local machine (or YOLO):
# ssh -L 3001:localhost:3000 your-server  (then open http://localhost:3001)

# Enable OTel in config.yaml (set env to your name to filter your telemetry)
# Add to your config.yaml:
#   otel_enabled: true
#   env: yourname

# Restart dev server
make dev
```

Open http://localhost:3000 to view traces (Tempo), metrics (Mimir), and logs (Loki) in Grafana.

**Import the Hypeman dashboard:**

1. Go to Dashboards → New → Import
2. Upload `dashboards/hypeman.json` or paste its contents
3. Select the Prometheus datasource and click Import

Use the Environment/Instance dropdowns to filter by `deployment.environment` or `service.instance.id`.

## Testing

Network tests require elevated permissions to create bridges and TAP devices.

```bash
make test
```

The test command compiles test binaries, grants capabilities via `sudo setcap`, then runs tests as the current user (not root). You may be prompted for your sudo password during the capability grant step.

## Code Generation

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

## macOS Development

Hypeman supports native macOS development using Apple's Virtualization.framework (via the `vz` hypervisor).

### Requirements

- **macOS 11.0+** (Big Sur or later)
- **Apple Silicon** (M1/M2/M3) recommended
- **macOS 14.0+** (Sonoma) required for snapshot/restore (ARM64 only)
- **Go 1.25.4+**
- **Caddy** (for ingress): `brew install caddy`
- **e2fsprogs** (for ext4 disk images): `brew install e2fsprogs`

### Quick Start

```bash
# 1. Install dependencies
brew install caddy e2fsprogs

# 2. Add e2fsprogs to PATH (it's keg-only)
export PATH="/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:$PATH"
# Add to ~/.zshrc for persistence

# 3. Configure environment
mkdir -p ~/.config/hypeman
cp config.example.darwin.yaml ~/.config/hypeman/config.yaml
# Edit config.yaml as needed (defaults work for local development)

# 4. Create data directory
mkdir -p ~/Library/Application\ Support/hypeman

# 5. Run in development mode (auto-detects macOS, builds, signs, and runs with hot reload)
make dev
```

The `make dev` command automatically detects macOS and:
- Builds with vz support
- Signs with required entitlements
- Runs with hot reload (no sudo required)

### Key Differences from Linux Development

| Aspect | Linux | macOS |
|--------|-------|-------|
| Hypervisor | Cloud Hypervisor, QEMU | vz (Virtualization.framework) |
| Binary signing | Not required | Automatic via `make dev` or `make sign-darwin` |
| Networking | TAP + bridge + iptables | Automatic NAT (no setup needed) |
| Root/sudo | Required for networking | Not required |
| Caddy | Embedded binary | Install via `brew install caddy` |
| DNS port | 5353 | 5354 (avoids mDNSResponder conflict) |

### macOS-Specific Configuration

The following config keys work differently on macOS (see `config.example.darwin.yaml`):

| Config key | Linux | macOS |
|----------|-------|-------|
| `hypervisor.default` | `cloud-hypervisor` | `vz` |
| `data_dir` | `/var/lib/hypeman` | `~/Library/Application Support/hypeman` |
| `caddy.internal_dns_port` | `5353` | `5354` (5353 is used by mDNSResponder) |
| `network.bridge_name` | Used | Ignored (NAT) |
| `network.subnet_cidr` | Used | Ignored (NAT) |
| `network.uplink_interface` | Used | Ignored (NAT) |
| Network rate limiting | Supported | Not supported |
| GPU passthrough | Supported (VFIO) | Not supported |

### Code Organization

Platform-specific code uses Go build tags:

```
lib/network/
├── bridge_linux.go      # Linux networking (TAP, bridges, iptables)
├── bridge_darwin.go     # macOS stubs (uses NAT)
└── ip.go                # Shared utilities

lib/devices/
├── discovery_linux.go   # Linux PCI device discovery
├── discovery_darwin.go  # macOS stubs (no passthrough)
├── mdev_linux.go        # Linux vGPU (mdev)
├── mdev_darwin.go       # macOS stubs
├── vfio_linux.go        # Linux VFIO binding
├── vfio_darwin.go       # macOS stubs
└── types.go             # Shared types

lib/hypervisor/
├── cloudhypervisor/     # Cloud Hypervisor (Linux)
├── qemu/                # QEMU (Linux, vsock_linux.go)
└── vz/                  # Virtualization.framework (macOS only)
    ├── starter.go       # VMStarter implementation
    ├── hypervisor.go    # Hypervisor interface
    └── vsock.go         # VsockDialer via VirtioSocketDevice
```

### Testing on macOS

```bash
# Verify vz package compiles correctly
make test-vz-compile

# Run unit tests (Linux-specific tests like networking will be skipped)
go test ./lib/hypervisor/vz/...
go test ./lib/resources/...
go test ./lib/images/...
```

Note: Full integration tests require Linux. On macOS, focus on unit tests and manual API testing.

### Known Limitations

1. **Disk Format**: vz only supports raw disk images (not qcow2). The image pipeline handles conversion automatically.

2. **Snapshots**: Not currently supported on the vz hypervisor.

### Troubleshooting

**"binary needs to be signed with entitlements"**
```bash
make sign-darwin
# Or just use: make dev (handles signing automatically)
```

**"caddy binary is not embedded on macOS"**
```bash
brew install caddy
```

**"address already in use" on port 5353**
- Port 5353 is used by mDNSResponder (Bonjour) on macOS
- Use port 5354 instead: set `caddy.internal_dns_port: 5354` in `config.yaml`
- The `config.example.darwin.yaml` already has this configured correctly

**"Virtualization.framework is not available"**
- Ensure you're on macOS 11.0+
- Check if virtualization is enabled in Recovery Mode settings

**"snapshot not supported"**
- Requires macOS 14.0+ on Apple Silicon
- Check: `sw_vers` and `uname -m` (should be arm64)

**VM fails to start**
- Check serial log: `$DATA_DIR/instances/<id>/serial.log`
- Ensure kernel and initrd paths are correct in config

**IOMMU/VFIO warnings at startup**
- These are expected on macOS and can be ignored
- GPU passthrough is not supported on macOS
