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

- WebSocket endpoint: `GET /instances/{id}/exec`
- **Note**: Uses GET method because WebSocket connections MUST be initiated with GET per RFC 6455 (the WebSocket specification). Even though this is semantically a command execution (which would normally be POST), the WebSocket upgrade handshake requires GET.
- Upgrades HTTP to WebSocket for bidirectional streaming
- First WebSocket message must be JSON with exec parameters:
  ```json
  {
    "command": ["bash", "-c", "whoami"],
    "tty": true,
    "env": {                // optional: environment variables
      "FOO": "bar"
    },
    "cwd": "/app",          // optional: working directory
    "timeout": 30           // optional: timeout in seconds
  }
  ```
- Calls `exec.ExecIntoInstance()` with the instance's vsock socket path
- Logs audit trail: JWT subject, instance ID, command, start/end time, exit code

### 2. Client (`lib/exec/client.go`)

- **ExecIntoInstance()**: Main client function
- Connects to Cloud Hypervisor's vsock Unix socket
- Performs vsock handshake: `CONNECT 2222\n` → `OK <cid>`
- Creates gRPC client over the vsock connection (pooled per VM for efficiency)
- Streams stdin/stdout/stderr bidirectionally
- Returns exit status when command completes

**Concurrency**: Multiple exec calls to the same VM share the underlying gRPC connection but use separate streams, enabling concurrent command execution.

### 3. Protocol (`exec.proto`)

gRPC streaming RPC with protobuf messages:

**Request (client → server):**
- `ExecStart`: Command, TTY flag, environment variables, working directory, timeout
- `stdin`: Input data bytes

**Response (server → client):**
- `stdout`: Output data bytes
- `stderr`: Error output bytes (non-TTY only)
- `exit_code`: Final message with command's exit status

### 4. Guest Agent (`lib/system/exec_agent/main.go`)

- Embedded binary injected into microVM via initrd
- **Runs inside container namespace** (chrooted to `/overlay/newroot`) for proper PTY signal handling
- Listens on vsock port 2222 inside guest
- Implements gRPC `ExecService` server
- Executes commands directly (no chroot wrapper needed since agent is already in container)
- Two modes:
  - **Non-TTY**: Separate stdout/stderr pipes
  - **TTY**: Single PTY for interactive shells with proper Ctrl+C handling

### 5. Embedding

- `exec-agent` binary built by Makefile
- Embedded into host binary via `lib/system/exec_agent_binary.go`
- Injected into initrd at VM creation time
- Auto-started by init script in guest

## Key Features

- **Bidirectional streaming**: Real-time stdin/stdout/stderr
- **TTY support**: Interactive shells with terminal control
- **Concurrent exec**: Multiple simultaneous commands per VM (separate streams)
- **Exit codes**: Proper process exit status reporting
- **No SSH required**: Direct vsock communication (faster, simpler)
- **Container isolation**: Commands run in container context, not VM context

## Why vsock?

- **Low latency**: Direct host-guest communication without networking
- **No network setup**: Works even if container has no network
- **Secure**: No exposed ports, isolated to host-guest boundary
- **Simple**: No SSH keys, passwords, or network configuration

## Security & Authorization

- All authentication and authorization is handled at the API layer via JWT
- The guest agent trusts that the host has properly authorized the request
- User/UID switching is performed in the guest to enforce privilege boundaries
- Commands run in the container context (`chroot /overlay/newroot`), not the VM context

## Observability

### API Layer Logging

The API logs comprehensive audit trails for all exec sessions:

```
# Session start
{"level":"info","msg":"exec session started","instance_id":"abc123","subject":"user@example.com",
 "command":["bash","-c","whoami"],"tty":true,"user":"www-data","uid":0,"cwd":"/app","timeout":30}

# Session end
{"level":"info","msg":"exec session ended","instance_id":"abc123","subject":"user@example.com",
 "exit_code":0,"duration_ms":1234}

# Errors
{"level":"error","msg":"exec failed","instance_id":"abc123","subject":"user@example.com",
 "error":"connection refused","duration_ms":500}
```

### Guest Agent Logging

The guest agent logs are written to the VM console log (accessible via `/var/lib/hypeman/guests/{id}/console.log`):

```
[exec-agent] listening on vsock port 2222
[exec-agent] new exec stream
[exec-agent] exec: command=[bash -c whoami] tty=true cwd=/app timeout=30
[exec-agent] command finished with exit code: 0
```

## Timeout Behavior

When a timeout is specified:
- The guest agent creates a context with the specified deadline
- If the command doesn't complete in time, it receives SIGKILL
- The exit code will be `124` (GNU timeout convention)
- Timeout is enforced in the guest, so network issues won't cause false timeouts

## Architecture

**exec-agent runs inside the container namespace**:
- Init script copies agent binary into `/overlay/newroot/usr/local/bin/`
- Bind-mounts `/dev/pts` so PTY devices are accessible
- Runs agent with `chroot /overlay/newroot`
- Commands execute directly (no chroot wrapper needed)

