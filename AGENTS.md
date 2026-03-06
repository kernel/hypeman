# Hypeman — Agent Development Guide

This guide is written for AI coding agents running inside a **Hypeman VM** (i.e., you are a Hypeship agent running on a Hypeman-managed machine). It covers how to build, run, and test hypeman from source in that environment.

For human developer setup, see [DEVELOPMENT.md](DEVELOPMENT.md). This document covers the agent-specific quirks that aren't obvious from DEVELOPMENT.md.

---

## What environment you're in

You are running inside a Hypeman VM (a KVM guest managed by an outer hypeman server). The outer server's URL and API key are in your environment:

```bash
echo $HYPEMAN_BASE_URL   # e.g. https://hypeman.dev-yul-hypeman-1.kernel.sh
echo $HYPEMAN_API_KEY    # JWT token for the outer hypeman API
```

The pre-installed `hypeman` CLI at `/usr/local/bin/hypeman` is already configured for the outer server. You can use it to exec commands as root in this VM — which is the key to bootstrapping permissions.

Your VM's instance ID and IP:

```bash
# Find your own instance ID by matching your IP to the instances list
MY_IP=$(ip route get 1.1.1.1 | awk '{print $7; exit}')
/usr/local/bin/hypeman ps  # look for your IP in the list
```

---

## Prerequisites

The VM image doesn't have Go or some required tools pre-installed. Here's how to get them.

### Go

```bash
# Download and install Go locally (no sudo needed)
mkdir -p ~/go-sdk
curl -fsSL https://go.dev/dl/go1.25.4.linux-amd64.tar.gz -o /tmp/go.tar.gz
# or if tar/gzip isn't available, use python:
# python3 -c "import tarfile; tarfile.open('/tmp/go.tar.gz').extractall('$HOME/go-sdk')"
tar -C ~/go-sdk -xzf /tmp/go.tar.gz

export PATH="$HOME/go-sdk/go/bin:$HOME/bin:$PATH"
go version  # should show go1.25.4 or later
```

### erofs-utils and dnsmasq

`apt-get` requires sudo and the package cache is often empty. Download the debs directly and extract:

```bash
mkdir -p ~/bin ~/local-pkgs
cd ~/local-pkgs

# Get package filenames from Ubuntu archive
EROFS_PKG=$(curl -s http://archive.ubuntu.com/ubuntu/dists/noble/universe/binary-amd64/Packages.gz \
  | gunzip | awk '/^Package: erofs-utils$/{found=1} found && /^Filename:/{print; exit}' | cut -d' ' -f2)
DNSMASQ_PKG=$(curl -s http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz \
  | gunzip | awk '/^Package: dnsmasq-base$/{found=1} found && /^Filename:/{print; exit}' | cut -d' ' -f2)

curl -sLO "http://archive.ubuntu.com/ubuntu/$EROFS_PKG"
curl -sLO "http://archive.ubuntu.com/ubuntu/$DNSMASQ_PKG"

dpkg-deb -x erofs-utils_*.deb erofs-extract
dpkg-deb -x dnsmasq-base_*.deb dnsmasq-extract

cp erofs-extract/usr/bin/mkfs.erofs ~/bin/
cp dnsmasq-extract/usr/sbin/dnsmasq ~/bin/

# Also copy to /usr/local/bin so the hypeman server can find them when running as root
/usr/local/bin/hypeman exec <YOUR_INSTANCE_NAME> -- bash -c \
  "cp /home/agent/bin/mkfs.erofs /usr/local/bin/ && cp /home/agent/bin/dnsmasq /usr/local/bin/ && chmod +x /usr/local/bin/mkfs.erofs /usr/local/bin/dnsmasq"
```

---

## Permission bootstrap (the important part)

Hypeman needs KVM access and network capabilities. You can't sudo inside this VM, but you can use the outer hypeman's exec API to run commands as root.

First, find your instance name (e.g. `hswp-36bfc47fe0`):

```bash
MY_INSTANCE=$(/usr/local/bin/hypeman ps --format json 2>/dev/null | \
  python3 -c "import json,sys; [print(i['name']) for i in json.load(sys.stdin) if i.get('network',{}).get('ip') == '$(ip route get 1.1.1.1 | awk \"{print \$7; exit}\")'"]" 2>/dev/null \
  || /usr/local/bin/hypeman ps | grep Running | awk '{print $2}' | head -1)
echo "My instance: $MY_INSTANCE"
```

Then bootstrap all required permissions in one shot:

```bash
# 1. Add agent to kvm and docker groups
/usr/local/bin/hypeman exec $MY_INSTANCE -- usermod -aG kvm,docker agent

# 2. Set capabilities on the built binary (re-run after each rebuild)
/usr/local/bin/hypeman exec $MY_INSTANCE -- setcap 'cap_net_admin,cap_net_bind_service=+eip' /home/agent/hypeman/bin/hypeman

# 3. Enable IPv4 forwarding
/usr/local/bin/hypeman exec $MY_INSTANCE -- sysctl -w net.ipv4.ip_forward=1

# 4. Switch iptables to legacy backend (the VM kernel doesn't support nft)
/usr/local/bin/hypeman exec $MY_INSTANCE -- bash -c \
  "update-alternatives --set iptables /usr/sbin/iptables-legacy; touch /run/xtables.lock; chmod 666 /run/xtables.lock"

# 5. Copy tools to system PATH for use when running as root
/usr/local/bin/hypeman exec $MY_INSTANCE -- bash -c \
  "cp /home/agent/bin/mkfs.erofs /home/agent/bin/dnsmasq /usr/local/bin/ && chmod +x /usr/local/bin/mkfs.erofs /usr/local/bin/dnsmasq"
```

> **Note on group membership**: Adding yourself to the kvm group updates `/etc/group` but doesn't affect running processes. The binary capabilities (`setcap`) handle KVM access. Run hypeman as root (via the outer exec API) or use `sg kvm -c "..."` for interactive shells.

---

## Build

```bash
export PATH="$HOME/go-sdk/go/bin:$HOME/hypeman/bin:$HOME/bin:$PATH"

# Download embedded binaries (cloud-hypervisor, firecracker, caddy)
make download-firecracker-binaries  # ~3MB
make download-ch-binaries           # ~9MB
make build-caddy                    # builds caddy with cloudflare DNS plugin (~50MB, takes ~2min)

# Build embedded guest binaries (cross-compiled for Linux)
make build-embedded

# Build the hypeman binary
go build -tags containers_image_openpgp -o bin/hypeman ./cmd/api

# Re-apply capabilities after each build
/usr/local/bin/hypeman exec $MY_INSTANCE -- setcap 'cap_net_admin,cap_net_bind_service=+eip' /home/agent/hypeman/bin/hypeman
```

> **Note**: The `make install-tools` and `make dev` targets expect `go` in PATH. If running via `make`, pass PATH explicitly: `PATH="$HOME/go-sdk/go/bin:..." make build`. Alternatively just call `go build` directly as shown above.

---

## Configuration

Create a local config (don't use system paths on a shared VM):

```bash
mkdir -p .tmp/hypeman-data
cat > .tmp/hypeman.config.yaml << 'EOF'
jwt_secret: "dev-secret-local-testing-only"
data_dir: /home/agent/hypeman/.tmp/hypeman-data
port: 8080

network:
  bridge_name: vmbr0
  subnet_cidr: 10.100.0.0/16
  uplink_interface: ens4   # use: ip route get 1.1.1.1 | awk '{print $5; exit}'
  dns_server: 1.1.1.1

logging:
  level: debug
EOF
```

---

## Running the server

The server must run as root (or with capabilities active) for KVM and networking. Use the outer hypeman exec to start it as a background daemon:

```bash
cat > /tmp/start-hypeman.sh << 'SCRIPT'
#!/bin/bash
export CONFIG_PATH=/home/agent/hypeman/.tmp/hypeman.config.yaml
nohup /home/agent/hypeman/bin/hypeman > /tmp/hypeman-server.log 2>&1 &
echo "PID: $!"
SCRIPT
chmod +x /tmp/start-hypeman.sh

/usr/local/bin/hypeman exec $MY_INSTANCE -- bash /tmp/start-hypeman.sh
```

Wait ~8 seconds for startup, then verify:

```bash
curl -s http://localhost:8080/health
# → {"status":"ok"}
```

Check startup logs:

```bash
cat /tmp/hypeman-server.log | grep -E '"level":"(ERROR|WARN|INFO)"' | head -30
```

To stop:

```bash
/usr/local/bin/hypeman exec $MY_INSTANCE -- pkill -f 'hypeman/bin/hypeman'
```

---

## Launching a VM

```bash
# Generate an auth token
export PATH="$HOME/go-sdk/go/bin:$HOME/hypeman/bin:$HOME/bin:$PATH"
TOKEN=$(CONFIG_PATH=.tmp/hypeman.config.yaml go run ./cmd/gen-jwt -user-id dev 2>&1)

# Pull an image (nginx:alpine is a good quick test)
curl -s -X POST http://localhost:8080/images \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "nginx:alpine"}'

# Wait for image to be ready (status: "ready")
until curl -s http://localhost:8080/images \
    -H "Authorization: Bearer $TOKEN" | \
    python3 -c "import json,sys; imgs=json.load(sys.stdin); exit(0 if any(i['status']=='ready' for i in imgs) else 1)"; do
  sleep 5
done
echo "image ready"

# Launch a VM
curl -s -X POST http://localhost:8080/instances \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "test-vm", "image": "nginx:alpine", "vcpus": 1, "memory": "512MB"}' \
  | python3 -m json.tool
```

The response includes `state: "Running"` and an IP in the `10.100.0.0/16` range. Verify it's working:

```bash
INSTANCE_ID=<id from above>

# Get VM logs (boot output + app stdout)
curl -s "http://localhost:8080/instances/$INSTANCE_ID/logs?lines=50" \
  -H "Authorization: Bearer $TOKEN"

# Ping the VM
ping -c 3 <VM_IP>

# Hit nginx inside the VM
curl http://<VM_IP>
```

---

## Known issues in nested-VM environments

The VM kernel (`ch-6.12.8-kernel-1.4-202602101`) has a minimal iptables setup:

| Issue | Symptom | Fix in code |
|-------|---------|-------------|
| `xt_comment` module missing | `iptables: Extension comment revision 0 not supported` | Removed `-m comment` from NAT rules (this repo, `lib/network/bridge_linux.go`) |
| `filter` table not available | `can't initialize iptables table 'filter'` | FORWARD rule failures downgraded to warnings — kernel default policy handles forwarding |
| `iptables-nft` fails | `Failed to initialize nft: Protocol not supported` | Switch to legacy: `update-alternatives --set iptables /usr/sbin/iptables-legacy` |

These are already fixed in this branch. If you see iptables errors on a fresh checkout of main, apply the fix:

```bash
git cherry-pick <commit from this branch that fixes bridge_linux.go>
```

---

## Useful commands

```bash
# List running VMs
curl -s http://localhost:8080/instances -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Get VM stats (CPU, memory)
curl -s http://localhost:8080/instances/$INSTANCE_ID/stats -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Stop a VM
curl -s -X POST http://localhost:8080/instances/$INSTANCE_ID/stop -H "Authorization: Bearer $TOKEN"

# Delete a VM
curl -s -X DELETE http://localhost:8080/instances/$INSTANCE_ID -H "Authorization: Bearer $TOKEN"

# List images
curl -s http://localhost:8080/images -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Server logs (live)
tail -f /tmp/hypeman-server.log

# Cloud-hypervisor process (one per running VM)
ps aux | grep cloud-hypervisor
```

---

## Architecture note: nested VMs

When running hypeman inside a Hypeman VM, you get two layers of KVM virtualization:

```
outer hypeman server
  └── your VM (this machine)  ←  runs hypeman from source
        ├── hypeman API (port 8080)
        ├── caddy (ingress)
        └── cloud-hypervisor VMs  ←  actual KVM guests you launch
```

This works because the outer hypeman VM exposes `/dev/kvm` to guests (nested virtualization). Performance is fine for development — the nested VMs boot in ~2–3 seconds.
