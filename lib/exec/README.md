# Exec Feature

Remote command execution in microVM instances via vsock.

## Architecture

```
Client (WebSocket)
    ↓
API Server (/instances/{id}/exec)
    ↓
lib/exec/client.go (ExecIntoInstance)
    ↓
Cloud Hypervisor vsock socket
    ↓
Guest: exec-agent (lib/system/exec_agent)
    ↓
Container (chroot /overlay/newroot)
```

## How It Works

### 1. API Layer (`cmd/api/api/exec.go`)

- WebSocket endpoint: `GET /instances/{id}/exec?command=...&tty=true/false`
- Upgrades HTTP to WebSocket for bidirectional streaming
- Calls `exec.ExecIntoInstance()` with the instance's vsock socket path

### 2. Client (`lib/exec/client.go`)

- **ExecIntoInstance()**: Main client function
- Connects to Cloud Hypervisor's vsock Unix socket
- Performs vsock handshake: `CONNECT 2222\n` → `OK <cid>`
- Creates gRPC client over the vsock connection
- Streams stdin/stdout/stderr bidirectionally
- Returns exit status when command completes

### 3. Protocol (`exec.proto`)

gRPC streaming RPC with protobuf messages:

**Request (client → server):**
- `ExecStart`: Command to run, TTY flag
- `stdin`: Input data bytes
- `WindowSize`: Terminal resize events (TTY mode)

**Response (server → client):**
- `stdout`: Output data bytes
- `stderr`: Error output bytes (non-TTY only)
- `exit_code`: Final message with command's exit status

### 4. Guest Agent (`lib/system/exec_agent/main.go`)

- Embedded binary injected into microVM via initrd
- Listens on vsock port 2222 inside guest
- Implements gRPC `ExecService` server
- Executes commands via `chroot /overlay/newroot` (container rootfs)
- Two modes:
  - **Non-TTY**: Separate stdout/stderr pipes
  - **TTY**: Single PTY for interactive shells

### 5. Embedding

- `exec-agent` binary built by Makefile
- Embedded into host binary via `lib/system/exec_agent_binary.go`
- Injected into initrd at VM creation time
- Auto-started by init script in guest

## Key Features

- **Bidirectional streaming**: Real-time stdin/stdout/stderr
- **TTY support**: Interactive shells with terminal control
- **Exit codes**: Proper process exit status reporting
- **No SSH required**: Direct vsock communication (faster, simpler)
- **Container isolation**: Commands run in container context, not VM context

## Why vsock?

- **Low latency**: Direct host-guest communication without networking
- **No network setup**: Works even if container has no network
- **Secure**: No exposed ports, isolated to host-guest boundary
- **Simple**: No SSH keys, passwords, or network configuration

