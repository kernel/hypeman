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

### Linux
**KVM** virtualization support required. Supports Cloud Hypervisor and QEMU as hypervisors.

### macOS
**macOS 11.0+** on Apple Silicon. Uses Apple's Virtualization.framework via the `vz` hypervisor.

## Quick Start

Install Hypeman (Linux and macOS supported):

```bash
curl -fsSL https://get.hypeman.sh | bash
```

This installs both the Hypeman server and CLI. The installer handles all dependencies and configuration automatically.

## CLI Installation

To use Hypeman via the CLI on a separate machine:

**Homebrew:**
```bash
brew install kernel/tap/hypeman
```

**Go:**
```bash
go install 'github.com/kernel/hypeman-cli/cmd/hypeman@latest'
```

**Configure CLI access:**

```bash
export HYPEMAN_API_KEY="<token>"
export HYPEMAN_BASE_URL="http://<host>:8080"
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

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for build instructions, configuration options, and contributing guidelines.

## License

See [LICENSE](LICENSE).
