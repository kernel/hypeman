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
  <strong>Run containerized workloads in VMs, powered by Cloud Hypervisor, Firecracker, QEMU, and Apple Virtualization.framework.</strong>
 <img alt="GitHub License" src="https://img.shields.io/github/license/kernel/hypeman">
  <a href="https://discord.gg/FBrveQRcud"><img src="https://img.shields.io/discord/1342243238748225556?logo=discord&logoColor=white&color=7289DA" alt="Discord"></a>
</p>

---

## Features

- **Docker-compatible CLI** — `run`, `exec`, `stop`, `ps`, `logs`, `pull` work like you'd expect
- **Multiple hypervisors** — Cloud Hypervisor, Firecracker, QEMU on Linux; Virtualization.framework on macOS
- **Standby & restore** — snapshot a VM to disk and resume it in milliseconds
- **Built-in ingress** — reverse proxy with TLS termination and subdomain routing
- **GPU passthrough** — vGPU and VFIO device support
- **OCI image support** — pull and run standard container images
- **Remote registry push** — export cached images to any OCI registry (AWS ECR, Docker Hub, ghcr, ...) with docker-style borrowed credentials
- **Remote API** — JWT-authenticated server with a separate CLI client

## Requirements

### Linux
**KVM** virtualization support required. Supports Cloud Hypervisor, Firecracker, and QEMU as hypervisors.

### macOS
**macOS 11.0+** on Apple Silicon. Uses Apple's Virtualization.framework via the `vz` hypervisor.
Install Rosetta to run `linux/amd64` images on Apple Silicon:

```bash
softwareupdate --install-rosetta --agree-to-license
```

## Quick Start

Install Hypeman (Linux and macOS supported):

```bash
curl -fsSL https://get.hypeman.sh | bash
```

This installs the Hypeman server, CLI, and token tool. The installer:
- Generates a YAML config file with a random JWT secret
- Starts the server as a system service (launchd on macOS, systemd on Linux)
- Creates a CLI config file (`~/.config/hypeman/cli.yaml`) with a pre-authenticated token

No environment variables needed -- just run `hypeman` commands immediately after install.

## Remote CLI Access

To use the Hypeman CLI from a **different machine** than the server:

**Homebrew (macOS):**
```bash
brew install kernel/tap/hypeman
```

**Install script (Linux & macOS):**
```bash
curl -fsSL https://get.hypeman.sh/cli | bash
```

**Go:**
```bash
go install 'github.com/kernel/hypeman-cli/cmd/hypeman@latest'
```

Then create a CLI config file at `~/.config/hypeman/cli.yaml`:

```yaml
base_url: http://<server-host>:4973
api_key: "<token>"
```

To generate a token, run `hypeman-token` on the server:

```bash
hypeman-token -user-id "my-user" -duration 8760h
```

Environment variables (`HYPEMAN_BASE_URL`, `HYPEMAN_API_KEY`) and CLI flags (`--base-url`) also work and take precedence over the config file.

## Configuration

Hypeman is configured via YAML config files.

| Component | Config File |
|-----------|-------------|
| Server | `/etc/hypeman/config.yaml` (Linux) or `~/.config/hypeman/config.yaml` (macOS) |
| CLI | `~/.config/hypeman/cli.yaml` |


See [`config.example.yaml`](config.example.yaml) (Linux) and [`config.example.darwin.yaml`](config.example.darwin.yaml) (macOS) for all available server options.

To expose the API through Caddy on a public HTTPS hostname, configure the hostname and TLS in the server config. The hostname must also be included in `acme.allowed_domains`:

```yaml
api:
  hostname: api.example.com
  tls: true
  redirect_http: true
```

With this configuration, use `https://api.example.com` as the API base URL. Without it, the API is available on the server's configured port (4973 by default).

## Usage

```bash
# Pull an image
hypeman pull nginx:alpine

# Create a local tag without pulling the image again
hypeman tag nginx:alpine my-registry.example.com/myapp:latest

# Boot a new VM (auto-pulls image if needed)
hypeman run --name my-app nginx:alpine

# On Linux amd64, use QEMU's minimal microvm backend.
# It cannot use PCI devices or hotplug memory.
hypeman run --hypervisor qemu-microvm --name my-microvm nginx:alpine

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

### Pushing Images to Remote Registries

A ready image can be exported asynchronously to any OCI registry through the
remote API. Set the API base URL and API key used by the examples below:

```bash
export HYPEMAN_BASE_URL="https://api.example.com"
export HYPEMAN_API_KEY="<api-key>"
```

Credentials use the Docker model:

- If `credentials` is provided, the client lends `username`/`password` or
  `registry_token` for this push only. Hypeman uses them in memory and never
  persists or logs them.
- If `credentials` is omitted, Hypeman uses the server's Docker keychain,
  including `/root/.docker/config.json` or the configured service user's
  `~/.docker/config.json` and any credential helpers.
- If Hypeman restarts while a push using borrowed credentials is running, that
  job fails instead of retrying without the original credentials. Anonymous
  interrupted jobs can be recovered with the server's keychain.

Use an HTTPS API URL when sending credentials. The `insecure` request field
controls only the connection from Hypeman to the destination registry.

For example, push to ECR using a short-lived login password:

```bash
export ECR_PASSWORD="$(aws ecr get-login-password --region us-east-1)"

curl --fail-with-body --silent --show-error \
  -X POST "$HYPEMAN_BASE_URL/pushes" \
  -H "Authorization: Bearer $HYPEMAN_API_KEY" \
  -H "Content-Type: application/json" \
  --data "{
    \"image\": \"myapp:latest\",
    \"target\": \"123456789.dkr.ecr.us-east-1.amazonaws.com/myapp:v1\",
    \"credentials\": {
      \"username\": \"AWS\",
      \"password\": \"$ECR_PASSWORD\"
    }
  }"
```

The response contains a push `id`. Poll it until the status is `pushed` or
`failed`:

```bash
curl --fail-with-body --silent --show-error \
  "$HYPEMAN_BASE_URL/pushes/<push-id>" \
  -H "Authorization: Bearer $HYPEMAN_API_KEY"
```

Push jobs move through `queued`, `pushing`, and `pushed` or `failed`.
`queue_position` is present while a job is queued; successful jobs report
`layers`, `bytes`, and `completed_at`, while failed jobs report `error`.
`GET /pushes` lists jobs newest first.

Set `"insecure": true` only when the destination registry uses plain HTTP.
HTTPS registries do not need this option. Invalid image names or targets return
`400`, a missing image returns `404`, and only images in the `ready` state can
be pushed (`409 image_not_ready` otherwise).

Layer blobs are preserved. OCI manifest digests are preserved too, while a
Docker v2 manifest is converted to OCI and can therefore receive a new digest;
use the returned `digest` as the destination manifest digest.

The default limit is two concurrent pushes. Increase or lower it with:

```yaml
limits:
  max_concurrent_pushes: 2
```

### VM Lifecycle

```bash
# Stop the VM
hypeman stop my-app

# Start a stopped VM
hypeman start my-app

# Put the VM in standby (snapshot to disk, stop hypervisor)
hypeman standby my-app

# Restore the VM from standby
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
