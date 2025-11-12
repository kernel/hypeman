# Network Manager

Manages virtual networks for instances using Linux bridges, TAP devices, and dnsmasq for DNS.

## Design Decisions

### State Derivation (No Central Allocations File)

**What:** Network allocations are derived from Cloud Hypervisor and snapshots, not stored in a central file.

**Why:**
- Single source of truth (CH and snapshots are authoritative)
- Self-contained guest directories (delete directory = automatic cleanup)
- No state drift between allocation file and reality
- Follows instance manager's pattern

**Sources of truth:**
- **Running VMs**: Query `GetVmInfo()` from Cloud Hypervisor - returns IP/MAC/TAP
- **Standby VMs**: Read `guests/{id}/snapshots/snapshot-latest/vm.json` from snapshot
- **Stopped VMs**: No network allocation

**Metadata storage:**
```
/var/lib/hypeman/guests/{instance-id}/
  metadata.json        # Contains: network field ("default", "internal", or "")
  snapshots/
    snapshot-latest/
      vm.json          # Cloud Hypervisor's config with IP/MAC/TAP
```

### Hybrid Network Model (Option 3)

**Standby → Restore: Network Fixed**
- TAP device deleted on standby (VMM shutdown)
- Snapshot `vm.json` preserves IP/MAC/TAP names
- Restore recreates TAP with same name
- DNS entries unchanged
- Fast resume path

**Shutdown → Boot: Network Changeable**
- TAP device deleted, DNS unregistered
- Can boot with different network
- Allows upgrades, migrations, reconfiguration
- Full recreate path

### Default Network

- Auto-created on first `Initialize()` call
- Configured from environment variables (BRIDGE_NAME, SUBNET_CIDR, SUBNET_GATEWAY)
- Cannot be deleted (returns error)
- Named "default"
- Always uses bridge_slave isolated mode

### Name Uniqueness Per Network

Instance names must be unique within each network:
- Prevents DNS collisions
- Scoped per network (can have "my-app" in both "default" and "internal")
- Enforced at allocation time by checking all running/standby instances

### DNS Resolution

**Naming convention:**
```
{instance-name}.{network}.hypeman  → IP
{instance-id}.{network}.hypeman    → IP
```

**Examples:**
```
my-app.default.hypeman          → 192.168.100.10
instance-xyz.default.hypeman    → 192.168.100.10
worker.internal.hypeman         → 192.168.101.10
```

**Single dnsmasq instance:**
- Listens on all bridge gateway IPs
- Serves all networks (no DNS isolation)
- Forwards unknown queries to 1.1.1.1
- Reloads with SIGHUP signal when allocations change

**Why no DNS isolation:**
- Instance proxy needs cross-network resolution
- Network isolation (bridge_slave) prevents actual VM-VM traffic
- Simpler implementation
- Can add DNS filtering later if needed

### Dependencies

**Go libraries:**
- `github.com/vishvananda/netlink` - Bridge/TAP operations (standard, used by Docker/K8s)

**Shell commands (justified):**
- `dnsmasq` - No Go library exists for DNS forwarder
- `iptables` - Complex rule manipulation not well-supported in netlink
- `ip link set X type bridge_slave isolated on` - Netlink library doesn't expose this flag

### Permissions

Network operations require `CAP_NET_ADMIN` and `CAP_NET_BIND_SERVICE` capabilities.

**Installation requirement:**
```bash
sudo setcap 'cap_net_admin,cap_net_bind_service=+ep' /path/to/hypeman
```

**Why:** Simplest approach, narrowly scoped permissions (not full root), standard practice for network services.

## Filesystem Layout

```
/var/lib/hypeman/
  network/
    dnsmasq.conf      # Generated config (listen addresses, upstreams)
    dnsmasq.hosts     # Generated from scanning guest dirs
    dnsmasq.pid       # Process PID
  guests/
    {instance-id}/
      metadata.json   # Contains: network field
      snapshots/
        snapshot-latest/
          vm.json     # Contains: IP/MAC/TAP (source of truth)
```

## Network Operations

### Initialize
- Create default network bridge (vmbr0)
- Assign gateway IP
- Setup iptables NAT and forwarding
- Start dnsmasq

### AllocateNetwork
1. Validate network exists
2. Check name uniqueness in network
3. Allocate next available IP (starting from .10)
4. Generate MAC (02:00:00:... format)
5. Generate TAP name (tap-{first8chars})
6. Create TAP device and attach to bridge
7. Reload DNS

### RecreateNetwork (for restore)
1. Derive allocation from snapshot vm.json
2. Recreate TAP device with same name
3. Attach to bridge with isolation mode

### ReleaseNetwork (for shutdown/delete)
1. Derive current allocation
2. Delete TAP device
3. Reload DNS (removes entries)

## IP Allocation Strategy

- Start from .10 (reserve .1-.9 for infrastructure)
- Sequential allocation through subnet
- Scan existing allocations to find next free IP
- Skip broadcast address (.255)

## Bridge Naming

- Default: vmbr0
- Custom networks: vmbr1, vmbr2, etc. (auto-assigned sequentially)
- Within Linux interface name limits

## Security

**Bridge_slave isolated mode:**
- Prevents layer-2 VM-to-VM communication
- VMs can only communicate with gateway (for internet)
- Instance proxy can route traffic between VMs if needed

**iptables rules:**
- NAT for outbound connections
- Stateful firewall (only allow ESTABLISHED,RELATED inbound)
- Default DENY for forwarding

## Testing

Network manager tests create real network devices (bridges, TAPs, dnsmasq) and require elevated permissions.

### Running Tests

```bash
make test
```

The Makefile compiles test binaries and grants capabilities via `sudo setcap`, then runs tests as your user (not root).

### Test Isolation

Network integration tests use per-test unique configuration for safe parallel execution:

- Each test gets a unique bridge and /29 subnet in 172.16.0.0/12 range
- Bridge names: `t{3hex}` (e.g., `t5a3`, `tff2`)
- 131,072 possible test networks (supports massive parallelism)
- Tests run safely in parallel with `t.Parallel()`
- Hash includes test name + PID + timestamp + random = cross-run safe

**Subnet allocation:**
- /29 subnets = 6 usable IPs per test (sufficient for test cases)
- Each test creates independent bridge, dnsmasq instance on unique IP
- No port conflicts (dnsmasq binds to unique gateway IP on standard port 53)

### Cleanup

Cleanup happens automatically via `t.Cleanup()`, which runs even on test failure or panic.

**If tests are killed (Ctrl+C)**, stale resources may remain. Manual cleanup:

```bash
./scripts/cleanup-test-networks.sh
```

This removes all test bridges (matching `t[0-9a-f]{3}`) and TAP devices (matching `tap-*`).

### Unit Tests vs Integration Tests

- **Unit tests** (TestGenerateMAC, TestValidateNetworkName, etc.): Run without permissions, test logic only
- **Integration tests** (TestInitializeIntegration, TestAllocateNetworkIntegration, etc.): Require permissions, create real devices

All tests run via `make test` - no separate commands needed.

