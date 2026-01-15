# UTM VM for Hypeman Development

Run Hypeman in a UTM virtual machine with nested KVM virtualization on Apple Silicon Macs.

> **Note:** In the future, this dev environment setup may be automated using Ansible or similar tooling.

## Requirements

- **macOS 15 (Sequoia)** or later
- **Apple M3 or newer** chip (required for nested virtualization)
- **UTM 4.6+**: `brew install --cask utm`

## Setup

### 1. Download Ubuntu Server ISO

Download Ubuntu Server for ARM from [Ubuntu's website](https://ubuntu.com/download/server/arm).

Or use the helper script:

```bash
./download-iso.sh
```

### 2. Create VM in UTM

Follow the [official UTM Ubuntu guide](https://docs.getutm.app/guides/ubuntu/):

1. Open UTM and click **+** to **Create a New Virtual Machine**
1. Start: select **Virtualize**
1. Operating System: select **Linux**
1. Hardware: Set RAM to **8192 MiB** and CPU cores to **4**
1. Linux: check the box to **Use Apple Virtualization**. **Boot from ISO** should be the default. Click **Browse** and select the Ubuntu Server ISO.
1. Storage: Set disk size to **100 GiB**
1. Shared Directory: skip this. The `bootstrap-dev-environment.sh` script will clone Hypeman onto the VM.
1. Summary: Name the VM **Hypeman** and click **Save**

**Important:** Before starting the VM:

- Right-click **HypemanDev** → **Edit**
- Go to **System** → Check **Use Hypervisor** (enables nested virtualization)
- Save

### 3. Install Ubuntu Server

Start the VM and follow the Ubuntu installer. You can select the defaults for everything except the following:

1. Profile configuration: set up with your name, a hostname like `hypeman` and a linux username and password you can remember.
1. **Install OpenSSH Server** ← Important to select this and then **Import SSH key** → **from GitHub** → Enter your GitHub username to import your public keys. The result is that, assuming your host machine has a private key that is uploaded to GitHub, you can re-use this for sshing into the VM.

Once installation is complete, select **Reboot now**.

### 4. Find VM IP Address

After reboot, the VM console will ask you to log in with the Linux username and password you set up.
Once you do that and have a shell, run:

```bash
ip addr
```

Look for an IP like `192.168.x.x` on the `enp0s1` interface.

Open a terminal window on your host machine and export it: `export HYPEMAN_VM_IP=192.168.x.x`

### 5. SSH into the VM

From your Mac terminal:

```bash
ssh -A -i ~/.ssh/id_ed25519 yourusername@${HYPEMAN_VM_IP}
```

### 6. Configure SSH (Recommended)

Add this to your `~/.ssh/config`, replacing `192.168.x.x` with your VM's IP and `yourusername` with your Ubuntu username:

```
Host hypeman
    HostName 192.168.x.x
    User yourusername
    IdentityFile ~/.ssh/id_ed25519
    ForwardAgent yes
```

Now you can simply run:

```bash
ssh hypeman
```

## Setting up the VM for Hypeman Development

Copy the `bootstrap-dev-environment.sh` script into the VM and run it:

```bash
scp bootstrap-dev-environment.sh hypeman:
ssh hypeman
./bootstrap-dev-environment.sh
```

This installs Go, erofs-utils, dnsmasq, and other dependencies needed for Hypeman.

It also clones the main Hypeman repositories into ~/code.

## Development Workflow

### Cursor Remote-SSH

Cursor can run on your host machine and connect to the VM to edit files and run commands.

1. Open Command Palette (`Cmd+Shift+P`)
2. Run **Remote-SSH: Connect to Host...**
3. Type **hypeman** (from your SSH config)
4. Open the `~/code/hypeman` directory.

### Run Hypeman

Follow the directions in [../../DEVELOPMENT.md](../../DEVELOPMENT.md).

You might want to set up your host machine's `/etc/hosts` file to resolve `hypeman.local` to your VM's IP:

```
192.168.x.x hypeman.local
```

This will let you, for example, run the LGTM stack in the VM (according to the instructions in [../../DEVELOPMENT.md](../../DEVELOPMENT.md)) and then on your host machine, open http://hypeman.local:3000 to view the Grafana dashboard.

It will also let you test ingresses with subdomain routing.

### Hypeman CLI on host -> VM

The Hypeman CLI can be installed on your host machine and used to interact with the VM.

1. Install the CLI on your host machine (or build from source):

```bash
brew install kernel/tap/hypeman
```

2. Set up the API key on your host machine:

```bash
# in the vm, generate a token:
make gen-jwt

# on the host machine
export HYPEMAN_API_KEY="<token-from-vm>"
export HYPEMAN_BASE_URL="http://hypemman.local:8080"

# then try out some commands
hypeman pull debian:13-slim
hypeman run --name test-debian debian:13-slim
hypeman exec test-debian -- uname -a
```
