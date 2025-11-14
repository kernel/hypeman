# Network Manager

Manages the default virtual network for instances using a Linux bridge and TAP devices.

## Overview

Hypeman provides a single default network that all instances can optionally connect to. There is no support for multiple custom networks - instances either have networking enabled (connected to the default network) or disabled (no network connectivity).

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
  metadata.json        # Contains: network_enabled field (bool)
  snapshots/
    snapshot-latest/
      vm.json          # Cloud Hypervisor's config with IP/MAC/TAP
```

### Hybrid Network Model

**Standby → Restore: Network Fixed**
- TAP device deleted on standby (VMM shutdown)
- Snapshot `vm.json` preserves IP/MAC/TAP names
- Restore recreates TAP with same name
- DNS entries unchanged
- Fast resume path

**Shutdown → Boot: Network Reconfigurable**
- TAP device deleted, DNS unregistered
- Can boot with different network settings (enabled/disabled)
- Allows upgrades, migrations, reconfiguration
- Full recreate path

### Default Network

- Auto-created on first `Initialize()` call
- Configured from environment variables (BRIDGE_NAME, SUBNET_CIDR, SUBNET_GATEWAY)
- Named "default" (only network in the system)
- Always uses bridge_slave isolated mode for VM-to-VM isolation

### Name Uniqueness

Instance names must be globally unique:
- Enforced at allocation time by checking all running/standby instances
- Simpler than per-network scoping

### DNS Configuration

Guests are configured to use external DNS servers directly (no internal DNS server needed):
- Configurable via `DNS_SERVER` environment variable (default: 1.1.1.1)
- Set in guest's `/etc/resolv.conf` during boot

### Dependencies

**Go libraries:**
- `github.com/vishvananda/netlink` - Bridge/TAP operations (standard, used by Docker/K8s)

**Shell commands:**
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
  network/            # Network state directory (reserved for future use)
  guests/
    {instance-id}/
      metadata.json   # Contains: network_enabled field (bool)
      snapshots/
        snapshot-latest/
          vm.json     # Contains: IP/MAC/TAP (source of truth)
```

## Network Operations

### Initialize
- Create default network bridge (vmbr0 or configured name)
- Assign gateway IP
- Setup iptables NAT and forwarding

### AllocateNetwork
1. Get default network details
2. Check name uniqueness globally
3. Allocate next available IP (starting from .2, after gateway at .1)
4. Generate MAC (02:00:00:... format - locally administered)
5. Generate TAP name (tap-{first8chars-of-instance-id})
6. Create TAP device and attach to bridge

### RecreateNetwork (for restore from standby)
1. Derive allocation from snapshot vm.json
2. Recreate TAP device with same name
3. Attach to bridge with isolation mode

### ReleaseNetwork (for shutdown/delete)
1. Derive current allocation
2. Delete TAP device

Note: In case of unexpected scenarios like power loss, straggler TAP devices may persist until manual cleanup or host reboot.

## IP Allocation Strategy

- Gateway at .1 (first IP in subnet)
- Instance IPs start from .2
- **Random allocation** with up to 5 retry attempts
  - Picks random IP in usable range
  - Checks for conflicts
  - Retries if conflict found
  - Falls back to sequential scan if all random attempts fail
- Helps distribute IPs across large subnets (especially /16)
- Reduces conflicts when moving standby VMs across hosts
- Skip network address, gateway, and broadcast address
- RNG seeded with timestamp for uniqueness across runs

## Concurrency & Locking

The network manager uses a single mutex to protect allocation operations:

### Locked Operations
- **AllocateNetwork**: Prevents concurrent IP allocation

### Unlocked Operations  
- **RecreateNetwork**: Safe without lock - protected by instance-level locking, doesn't allocate IPs
- **ReleaseNetwork**: Safe without lock - only deletes TAP device
- **Read operations** (GetAllocation, ListAllocations, NameExists): No lock needed - eventual consistency is acceptable

### Why This Works
- Write operations are serialized to prevent race conditions
- Read operations can run concurrently for better performance
- Internal calls (e.g., AllocateNetwork → ListAllocations) work because reads don't lock
- Instance manager already provides per-instance locking for state transitions

## Security

**Bridge_slave isolated mode:**
- Prevents layer-2 VM-to-VM communication
- VMs can only communicate with gateway (for internet access)
- Instance proxy could route traffic between VMs if needed in the future

**iptables rules:**
- NAT for outbound connections
- Stateful firewall (only allow ESTABLISHED,RELATED inbound)
- Default DENY for forwarding
- Rules added on Initialize, per-subnet basis

## Testing

Network manager tests create real network devices (bridges, TAPs) and require elevated permissions.

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
- Each test creates independent bridge on unique IP

### Cleanup

Cleanup happens automatically via `t.Cleanup()`, which runs even on test failure or panic.

### Unit Tests vs Integration Tests

- **Unit tests** (TestGenerateMAC, etc.): Run without permissions, test logic only
- **Integration tests** (TestInitializeIntegration, TestAllocateNetworkIntegration, etc.): Require permissions, create real devices

All tests run via `make test` - no separate commands needed.

