<p align="center">

```
 ██╗  ██╗  ██╗   ██╗  ██████╗   ███████╗  ███╗   ███╗   █████╗   ███╗   ██╗
 ██║  ██║  ╚██╗ ██╔╝  ██╔══██╗  ██╔════╝  ████╗ ████║  ██╔══██╗  ████╗  ██║
 ███████║   ╚████╔╝   ██████╔╝  █████╗    ██╔████╔██║  ███████║  ██╔██╗ ██║
 ██╔══██║    ╚██╔╝    ██╔═══╝   ██╔══╝    ██║╚██╔╝██║  ██╔══██║  ██║╚██╗██║
 ██║  ██║     ██║     ██║       ███████╗  ██║ ╚═╝ ██║  ██║  ██║  ██║ ╚████║
 ╚═╝  ╚═╝     ╚═╝     ╚═╝       ╚══════╝  ╚═╝     ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚═══╝
```

</p>

<p align="center">
  <strong>Run containerized workloads in VMs, powered by <a href="https://github.com/cloud-hypervisor/cloud-hypervisor">Cloud Hypervisor</a>.</strong>
 <img alt="GitHub License" src="https://img.shields.io/github/license/kernel/hypeman">
  <a href="https://discord.gg/FBrveQRcud"><img src="https://img.shields.io/discord/1342243238748225556?logo=discord&logoColor=white&color=7289DA" alt="Discord"></a>
</p>

---

## Requirements

### Linux (Production)
Hypeman server runs on **Linux** with **KVM** virtualization support. Supports Cloud Hypervisor and QEMU as hypervisors.

### macOS (Experimental)
Hypeman also supports **macOS** (11.0+) using Apple's **Virtualization.framework** via the `vz` hypervisor. See [macOS Support](#macos-support) below.

The CLI can run locally on the server or connect remotely from any machine.

## Quick Start

Install Hypeman on your Linux server:

```bash
curl -fsSL https://get.hypeman.sh | bash
```

This installs both the Hypeman server and CLI. The installer handles all dependencies, KVM access, and network configuration automatically.

## CLI Installation (Remote Access)

To connect to a Hypeman server from another machine, install just the CLI:

**Homebrew:**
```bash
brew install kernel/tap/hypeman
```

**Go:**
```bash
go install 'github.com/kernel/hypeman-cli/cmd/hypeman@latest'
```

**Configure remote access:**

1. On the server, generate an API token:
```bash
hypeman-token
```

2. On your local machine, set the environment variables:
```bash
export HYPEMAN_API_KEY="<token-from-server>"
export HYPEMAN_BASE_URL="http://<server-ip>:8080"
```

## Usage

```bash
# Pull an image
hypeman pull nginx:alpine

# Boot a new VM (auto-pulls image if needed)
hypeman run --name my-app nginx:alpine

# List running VMs
hypeman ps

# Show all VMs
hypeman ps -a

# View logs (supports VM name, ID, or partial ID)
hypeman logs my-app
hypeman logs -f my-app

# Execute a command in a running VM
hypeman exec my-app whoami

# Shell into the VM
hypeman exec -it my-app /bin/sh
```

### VM Lifecycle

```bash
# Stop the VM
hypeman stop my-app

# Start a stopped VM
hypeman start my-app

# Put the VM to sleep (paused)
hypeman standby my-app

# Wake the VM (resumed)
hypeman restore my-app

# Delete all VMs
hypeman rm --force --all
```

### Ingress (Reverse Proxy)

Create a reverse proxy from the host to your VM:

```bash
# Create an ingress
hypeman ingress create --name my-ingress my-app --hostname my-nginx-app --port 80 --host-port 8081

# List ingresses
hypeman ingress list

# Test it
curl --header "Host: my-nginx-app" http://127.0.0.1:8081

# Delete an ingress
hypeman ingress delete my-ingress
```

### TLS & Subdomain Routing

```bash
# TLS-terminating ingress (requires DNS credentials in server config)
hypeman ingress create --name my-tls-ingress my-app \
  --hostname hello.example.com -p 80 --host-port 7443 --tls

# Test TLS
curl --resolve hello.example.com:7443:127.0.0.1 https://hello.example.com:7443

# Subdomain-based routing
hypeman ingress create --name subdomain-ingress '{instance}' \
  --hostname '{instance}.example.com' -p 80 --host-port 8443 --tls

# Delete all ingresses
hypeman ingress delete --all
```

### Advanced Logging

```bash
# View Cloud Hypervisor logs
hypeman logs --source vmm my-app

# View Hypeman operational logs
hypeman logs --source hypeman my-app
```

For all available commands, run `hypeman --help`.

## macOS Support

Hypeman supports macOS using Apple's Virtualization.framework through the `vz` hypervisor. This provides native virtualization on Apple Silicon Macs (Intel Macs are not supported).

### Requirements

- macOS 11.0+ (macOS 14.0+ required for snapshot/restore on ARM64)
- Apple Silicon (M1/M2/M3) recommended
- Caddy: `brew install caddy`
- e2fsprogs: `brew install e2fsprogs` (for ext4 disk images)

### Quick Start (macOS)

```bash
# Install dependencies
brew install caddy e2fsprogs

# Add e2fsprogs to PATH (it's keg-only)
export PATH="/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:$PATH"

# Configure environment
cp .env.darwin.example .env

# Create data directory
mkdir -p ~/Library/Application\ Support/hypeman

# Run with hot reload (auto-detects macOS, builds, signs, and runs)
make dev
```

The `make dev` command automatically detects macOS and handles building with vz support and signing with required entitlements.

### Key Differences from Linux

| Feature | Linux | macOS |
|---------|-------|-------|
| Hypervisors | Cloud Hypervisor, QEMU | vz (Virtualization.framework) |
| Networking | TAP devices, bridges, iptables | NAT (built-in, automatic) |
| Rate Limiting | HTB/tc | Not supported |
| GPU Passthrough | VFIO | Not supported |
| Disk Format | qcow2, raw | raw only |
| Snapshots | Always available | macOS 14+ ARM64 only |

### Limitations

- **Networking**: macOS uses NAT networking automatically. No manual bridge/TAP configuration needed, but ingress requires discovering the VM's NAT IP.
- **Rate Limiting**: Network and disk I/O rate limiting is not available on macOS.
- **GPU**: PCI device passthrough is not supported on macOS.
- **Disk Images**: qcow2 format is not directly supported; use raw disk images.
- **Snapshots**: Requires macOS 14.0+ on Apple Silicon (ARM64).

For detailed development setup, see [DEVELOPMENT.md](DEVELOPMENT.md).

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for build instructions, configuration options, and contributing guidelines.

## License

See [LICENSE](LICENSE).
