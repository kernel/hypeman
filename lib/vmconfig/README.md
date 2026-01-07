# VM Config

Shared configuration schema for host-to-guest communication.

## Purpose

This package defines the `Config` struct that is:
- Serialized as JSON by the host when creating a VM's config disk
- Deserialized by the guest init binary during boot

By defining this in a shared package, the host and guest code stay in sync without duplication.

## Usage

The host writes this config to `/config.json` on the config disk (attached as `/dev/vdc`).
The guest init binary mounts this disk and reads the JSON configuration.

## Fields

- **Entrypoint/Cmd/Workdir**: Container execution parameters from the OCI image
- **Env**: Environment variables (merged from image + instance overrides)
- **Network**: Guest IP, gateway, DNS configuration
- **GPU**: Whether GPU passthrough is enabled
- **VolumeMounts**: Block devices to mount inside the guest
- **InitMode**: Either "exec" (container-like) or "systemd" (full VM)
