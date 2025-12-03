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

### Configuration

#### Environment variables

Hypeman can be configured using the following environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `PORT` | HTTP server port | `8080` |
| `DATA_DIR` | Directory for storing VM images, volumes, and other data | `/var/lib/hypeman` |
| `BRIDGE_NAME` | Name of the network bridge for VM networking | `vmbr0` |
| `SUBNET_CIDR` | CIDR notation for the VM network subnet (gateway derived automatically) | `10.100.0.0/16` |
| `UPLINK_INTERFACE` | Host network interface to use for VM internet access | _(auto-detect)_ |
| `JWT_SECRET` | Secret key for JWT authentication (required for production) | _(empty)_ |
| `DNS_SERVER` | DNS server IP address for VMs | `1.1.1.1` |
| `MAX_CONCURRENT_BUILDS` | Maximum number of concurrent image builds | `1` |
| `MAX_OVERLAY_SIZE` | Maximum size for overlay filesystem | `100GB` |
| `ENV` | Deployment environment (filters telemetry, e.g. your name for dev) | `unset` |
| `OTEL_ENABLED` | Enable OpenTelemetry traces/metrics | `false` |
| `OTEL_ENDPOINT` | OTLP gRPC endpoint | `127.0.0.1:4317` |
| `OTEL_SERVICE_INSTANCE_ID` | Instance ID for telemetry (differentiates multiple servers) | hostname |
| `LOG_LEVEL` | Default log level (debug, info, warn, error) | `info` |
| `LOG_LEVEL_<SUBSYSTEM>` | Per-subsystem log level (API, IMAGES, INSTANCES, NETWORK, VOLUMES, VMM, SYSTEM, EXEC) | inherits default |

**Important: Subnet Configuration**

The default subnet `10.100.0.0/16` is chosen to avoid common conflicts. Hypeman will detect conflicts with existing routes on startup and fail with guidance.

If you need a different subnet, set `SUBNET_CIDR` in your environment. The gateway is automatically derived as the first IP in the subnet (e.g., `10.100.0.0/16` → `10.100.0.1`).

**Alternative subnets if needed:**
- `172.30.0.0/16` - Private range between common Docker (172.17.x.x) and AWS (172.31.x.x) ranges
- `10.200.0.0/16` - Another private range option

**Example:**
```bash
# In your .env file
SUBNET_CIDR=172.30.0.0/16
```

**Finding the uplink interface (`UPLINK_INTERFACE`)**

`UPLINK_INTERFACE` tells Hypeman which host interface to use for routing VM traffic to the outside world (for iptables MASQUERADE rules). On many hosts this is `eth0`, but laptops and more complex setups often use Wi‑Fi or other names.

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

**Setup:**

```bash
cp .env.example .env
# Edit .env and set JWT_SECRET and other configuration values
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

1. Generate a JWT token for testing (optional):
```bash
make gen-jwt
```

2. Start the server with hot-reload for development:
```bash
make dev
```
The server will start on port 8080 (configurable via `PORT` environment variable).

#### Local OpenTelemetry (optional)

To collect traces and metrics locally, run the Grafana LGTM stack (Loki, Grafana, Tempo, Mimir):

```bash
# Start Grafana LGTM (UI at http://localhost:3000, login: admin/admin)
# Note, if you are developing on a shared server, you can use the same LGTM stack as your peer(s)
# You will be able to sort your metrics, traces, and logs using the ENV configuration (see below)
docker run -d --name lgtm \
  -p 127.0.0.1:3000:3000 \
  -p 127.0.0.1:4317:4317 \
  -p 127.0.0.1:4318:4318 \
  -p 127.0.0.1:9090:9090 \
  -p 127.0.0.1:4040:4040 \
  grafana/otel-lgtm:latest

# If developing on a remote server, forward the port to your local machine:
# ssh -L 3001:localhost:3000 your-server  (then open http://localhost:3001)

# Enable OTel in .env (set ENV to your name to filter your telemetry)
echo "OTEL_ENABLED=true" >> .env
echo "ENV=yourname" >> .env

# Restart dev server
make dev
```

Open http://localhost:3000 to view traces (Tempo) and metrics (Mimir) in Grafana.

**Import the Hypeman dashboard:**
1. Go to Dashboards → New → Import
2. Upload `dashboards/hypeman.json` or paste its contents
3. Select the Prometheus datasource and click Import

Use the Environment/Instance dropdowns to filter by `deployment.environment` or `service.instance.id`.

### Testing

Network tests require elevated permissions to create bridges and TAP devices.

```bash
make test
```

The test command compiles test binaries, grants capabilities via `sudo setcap`, then runs tests as the current user (not root). You may be prompted for your sudo password during the capability grant step.

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
