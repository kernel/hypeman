# Windows guest agent

Windows personas include `hypeman-guest-agent.exe` as the automatic `HypemanGuestAgent` LocalSystem service and the signed virtio-win VioSock driver. The agent listens on virtio-vsock port 2222 and implements the existing guest gRPC protocol.

The Windows build supports:

- command execution in the LocalSystem service session
- command execution in the active interactive desktop session
- ConPTY allocation and terminal resize events
- file copy, path stat, and graceful shutdown

Exec requests use `session: "system"` by default. `session: "desktop"` obtains the token for the active Windows session and returns an error when no interactive user is logged in.

Build the service with:

```sh
make build-windows-guest-agent
```

The generated executable, Windows driver packages, credentials, and prepared persona disks are release inputs. They must not be committed to this repository.
