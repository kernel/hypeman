# Windows networking

Hypeman can attach a Windows 11 QEMU guest to its normal TAP/bridge network. The public instance model remains unchanged: `NetworkEnabled` allocates the address, MAC, gateway, netmask, DNS servers, and TAP device used for Linux guests.

After the Windows guest agent becomes reachable over virtio-vsock, Hypeman sends the allocation through the typed `ReconfigureNetwork` RPC. The Windows agent:

1. finds the virtio-net adapter by its allocated MAC address;
2. removes stale IPv4 addresses and default routes;
3. creates the allocated IPv4 address and default route with Windows IP Helper APIs; and
4. applies the allocated DNS servers with `SetInterfaceDnsSettings`.

Windows never uses the Linux shell-command fallback. Create fails and stops the VM if the typed reconfiguration fails. Start applies the current allocation again, allowing an instance to receive a different address or MAC after it was stopped.

RDP is not a Hypeman API. A prepared persona may enable RDP, and callers can reach TCP port 3389 through the instance's generic allocated IP after applying their normal ingress policy.

## Integration fixture

`TestWindowsNetworkingIntegration` uses the private `HYPEMAN_WINDOWS_TEST_AGENT_PERSONA` fixture (default `/ci/windows/persona-agent.qcow2`). It verifies the address from inside Windows, performs a DNS lookup, checks ICMP, and opens the RDP TCP port over the allocated TAP network. The fixture and its Windows license are not stored in this repository.
