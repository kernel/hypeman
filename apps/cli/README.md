# Hypeman CLI

The official CLI for [Hypeman](https://github.com/kernel/hypeman/).

## Installation

### Installing with Homebrew

```sh
brew install kernel/tap/hypeman
```

### Installing with Go

<!-- x-release-please-start-version -->

```sh
go install 'github.com/kernel/hypeman-cli/cmd/hypeman@latest'
```

### Running Locally

<!-- x-release-please-start-version -->

```sh
go run cmd/hypeman/main.go
```

<!-- x-release-please-end -->

## Usage

```sh
# Pull an image
hypeman pull nginx:alpine

# Boot a new VM (auto-pulls image if needed)
hypeman run --name my-app nginx:alpine

# List running VMs
hypeman ps
# show all VMs
hypeman ps -a

# View logs of your app
# All commands support using VM name, ID, or partial ID
hypeman logs my-app
hypeman logs -f my-app

# Execute a command in a running VM
hypeman exec my-app whoami
# Shell into the VM
hypeman exec -it my-app /bin/sh

# VM lifecycle
# Turn off the VM
hypeman stop my-app
# Boot the VM that was turned off
hypeman start my-app
# Put the VM to sleep (paused)
hypeman standby my-app
# Awaken the VM (resumed)
hypeman restore my-app

# Create a reverse proxy ("ingress") from the host to your VM
hypeman ingress create --name my-ingress my-app --hostname my-nginx-app --port 80 --host-port 8081

# List ingresses
hypeman ingress list

# Curl nginx through your ingress
curl --header "Host: my-nginx-app" http://127.0.0.1:8081

# Delete an ingress
hypeman ingress delete my-ingress

# Delete all VMs
hypeman rm --force --all
```

More ingress features:
- Automatic certs
- Subdomain-based routing

```bash
# Make your VM if not already present
hypeman run --name my-app nginx:alpine

# This requires configuring the Hypeman server with DNS credentials
# Change --hostname to a domain you own
hypeman ingress create --name my-tls-ingress my-app --hostname hello.hypeman-development.com -p 80 --host-port 7443 --tls

# Curl through your TLS-terminating reverse proxy configuration
curl \
  --resolve hello.hypeman-development.com:7443:127.0.0.1 \
  https://hello.hypeman-development.com:7443

# OR... Ingress also supports subdomain-based routing
hypeman ingress create --name my-tls-subdomain-ingress '{instance}' --hostname '{instance}.hypeman-development.com' -p 80 --host-port 8443 --tls

# Curling through the subdomain-based routing
curl \
  --resolve my-app.hypeman-development.com:8443:127.0.0.1 \
  https://my-app.hypeman-development.com:8443

# Delete all ingress
hypeman ingress delete --all
```

More logging features:
- Cloud Hypervisor logs
- Hypeman operational logs

```bash
# View Cloud Hypervisor logs for your VM
hypeman logs --source vmm my-app
# View Hypeman logs for your VM
hypeman logs --source hypeman my-app
```

For details about specific commands, use the `--help` flag.

The CLI also provides resource-based commands for more advanced usage:

```sh
hypeman [resource] [command] [flags]
```

## Resource Management

### Viewing Server Resources

Check available server capacity, current allocations, and GPU availability:

```bash
# Show server resource status (CPU, memory, disk, network, GPU)
hypeman resources

# Show resources as JSON
hypeman resources --format json

# Show only GPU information
hypeman resources --transform gpu
```

### Per-VM Resource Limits

Control resource allocation for instances:

```bash
# Set disk I/O limit
hypeman run --disk-io 100MB/s --name io-limited myimage:latest

# Set network bandwidth limits
hypeman run --bandwidth-down 1Gbps --bandwidth-up 500Mbps --name bw-limited myimage:latest

# Combine multiple resource options
hypeman run \
  --cpus 4 \
  --memory 8GB \
  --gpu-profile L40S-2Q \
  --disk-io 200MB/s \
  --bandwidth-down 10Gbps \
  --name ml-training \
  pytorch:latest
```

## GPU support


### GPU Passthrough

For full GPU passthrough (entire GPU dedicated to one VM):

```bash
# Discover available passthrough-capable devices
hypeman device available

# Register a GPU for passthrough
hypeman device register --pci-address 0000:a2:00.0 --name my-gpu

# List registered devices
hypeman device list

# Run an instance with the GPU attached
hypeman run --device my-gpu --hypervisor qemu --name gpu-workload cuda:12.0

# When done, unregister the device
hypeman device delete my-gpu
```

### Nvidia vGPU

Use NVIDIA vGPU to share a physical GPU across multiple VMs:

```bash
# Run with a vGPU profile
hypeman run --gpu-profile L40S-1Q --name ml-workload pytorch:latest

# Run with more vGPU resources
hypeman run --gpu-profile L40S-4Q --cpus 8 --memory 32GB --name training-job tensorflow:latest
```

### Hypervisor Selection

Choose between Cloud Hypervisor (default) and QEMU:

```bash
# Run with QEMU (more compatible with some features like vGPU)
hypeman run --hypervisor qemu --name qemu-vm myimage:latest

# Run with Cloud Hypervisor (default, faster boot)
hypeman run --hypervisor cloud-hypervisor --name ch-vm myimage:latest
```

## Global Flags

- `--debug` - Enable debug logging (includes HTTP request/response details)
- `--version`, `-v` - Show the CLI version

## Development

### Testing Preview Branches

When developing features in the main [hypeman](https://github.com/kernel/hypeman) repo, Stainless automatically creates preview branches in `stainless-sdks/hypeman-cli` with your API changes. You can check out these branches locally to test the CLI changes:

```bash
# Checkout preview/<branch> (e.g., if working on "devices" branch in hypeman)
./scripts/checkout-preview devices

# Checkout an exact branch name
./scripts/checkout-preview -b main
./scripts/checkout-preview -b preview/my-feature
```

The script automatically adds the `stainless` remote if needed and also updates `go.mod` to point the `hypeman-go` SDK dependency to the corresponding preview branch in `stainless-sdks/hypeman-go`.

> **Warning:** The `go.mod` and `go.sum` changes from `checkout-preview` are for local testing only. Do not commit these changes.

After checking out a preview branch, you can build and test the CLI:

```bash
go build -o hypeman ./cmd/hypeman
./hypeman --help
```

You can also point the SDK dependency independently:

```bash
# Point hypeman-go to a specific branch
./scripts/use-sdk-preview preview/my-feature

# Point to a specific commit
./scripts/use-sdk-preview abc1234def567
```
