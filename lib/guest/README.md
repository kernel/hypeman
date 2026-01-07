# Guest Agent

Remote guest VM operations via vsock - command execution and file copying.

## Architecture

```
Client (WebSocket)
    ↓
API Server (/instances/{id}/exec, /instances/{id}/cp)
    ↓
lib/guest/client.go (ExecIntoInstance, CopyToInstance, CopyFromInstance)
    ↓
Cloud Hypervisor vsock socket
    ↓
Guest: guest-agent (lib/system/guest_agent)
    ↓
Container (chroot /overlay/newroot)
```

## Features

### Command Execution (Exec)

- **ExecIntoInstance()**: Execute commands with bidirectional stdin/stdout streaming
- **TTY support**: Interactive shells with terminal control
- **Concurrent exec**: Multiple simultaneous commands per VM (separate streams)
- **Exit codes**: Proper process exit status reporting

### File Copy (CP)

- **CopyToInstance()**: Copy files/directories from host to guest
- **CopyFromInstance()**: Copy files/directories from guest to host
- **Streaming**: Efficient chunked transfer for large files
- **Permissions**: Preserve file mode and ownership where possible

## How It Works

### 1. API Layer

- WebSocket endpoint: `GET /instances/{id}/exec` - command execution
- WebSocket endpoint: `GET /instances/{id}/cp` - file copy operations
- **Note**: Uses GET method because WebSocket connections MUST be initiated with GET per RFC 6455.
- Upgrades HTTP to WebSocket for bidirectional streaming
- Calls `guest.ExecIntoInstance()` or `guest.CopyTo/FromInstance()` with the instance's vsock socket path
- Logs audit trail: JWT subject, instance ID, operation, start/end time

### 2. Client (`lib/guest/client.go`)

- Connects to Cloud Hypervisor's vsock Unix socket
- Performs vsock handshake: `CONNECT 2222\n` → `OK <cid>`
- Creates gRPC client over the vsock connection (pooled per VM for efficiency)
- Streams data bidirectionally

**Concurrency**: Multiple calls to the same VM share the underlying gRPC connection but use separate streams.

### 3. Protocol (`guest.proto`)

gRPC streaming RPC with protobuf messages:

**Exec Request (client → server):**
- `ExecStart`: Command, TTY flag, environment variables, working directory, timeout
- `stdin`: Input data bytes

**Exec Response (server → client):**
- `stdout`: Output data bytes
- `stderr`: Error output bytes (non-TTY only)
- `exit_code`: Final message with command's exit status

**Copy Request (client → server):**
- `CopyStart`: Destination path, file mode
- `data`: File content chunks
- `done`: Indicates transfer complete

**Copy Response (server → client):**
- `data`: File content chunks (for CopyFromInstance)
- `error`: Error message if operation failed
- `done`: Indicates transfer complete

### 4. Guest Agent (`lib/system/guest_agent/main.go`)

- Embedded binary injected into microVM via initrd
- **Runs inside container namespace** (chrooted to `/overlay/newroot`) for proper file access
- Listens on vsock port 2222 inside guest
- Implements gRPC `GuestService` server
- Executes commands and handles file operations directly

### 5. Embedding

- `guest-agent` binary built by Makefile
- Embedded into host binary via `lib/system/guest_agent_binary.go`
- Injected into initrd at VM creation time
- Auto-started by init script in guest

## Why vsock?

- **Low latency**: Direct host-guest communication without networking
- **No network setup**: Works even if container has no network
- **Secure**: No exposed ports, isolated to host-guest boundary
- **Simple**: No SSH keys, passwords, or network configuration

## Security & Authorization

- All authentication and authorization is handled at the API layer via JWT
- The guest agent trusts that the host has properly authorized the request
- Commands and file operations run in the container context, not the VM context

