# Windows networking

Hypeman can attach a Windows 11 QEMU guest to its normal TAP/bridge network. The public instance model remains unchanged: `NetworkEnabled` allocates the address, MAC, gateway, netmask, DNS servers, and TAP device used for Linux guests.

Create and start apply the current allocation before the instance becomes ready. A configuration failure fails the lifecycle operation rather than exposing a guest with partial networking.

RDP is not a Hypeman API. A prepared image may enable RDP, and callers can reach TCP port 3389 through the instance's generic allocated IP after applying their normal ingress policy.
