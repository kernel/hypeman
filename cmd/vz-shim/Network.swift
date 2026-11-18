import Foundation
import Virtualization
import vmnet

func makeNetworkDevices(_ networks: [NetworkConfig]) throws -> [VZNetworkDeviceConfiguration] {
  // No networks configured (NetworkEnabled=false): no NIC is attached.
  try networks.map { n in
    let device = VZVirtioNetworkDeviceConfiguration()
    if let address = n.ipv4Address, !address.isEmpty {
      device.attachment = try vmnetAttachment(n)
    } else {
      device.attachment = VZNATNetworkDeviceAttachment()
    }
    device.macAddress = macAddress(n.mac)
    return device
  }
}

func macAddress(_ text: String?) -> VZMACAddress {
  if let text, !text.isEmpty {
    if let mac = VZMACAddress(string: text) {
      Log.info("using specified MAC address", ["mac": text])
      return mac
    }
    Log.warn("failed to parse MAC address, generating random", ["mac": text])
    return VZMACAddress.randomLocallyAdministered()
  }
  let mac = VZMACAddress.randomLocallyAdministered()
  Log.info("generated random MAC address", ["mac": mac.string])
  return mac
}

// vmnetAttachment attaches the guest to its own shared-mode vmnet network with the
// given subnet and a DHCP reservation for the guest's MAC, so the guest's address is
// the one hypeman assigned.
func vmnetAttachment(_ n: NetworkConfig) throws -> VZNetworkDeviceAttachment {
  guard #available(macOS 26.0, *) else {
    throw ShimError("configure network: vmnet networks require macOS 26 or newer")
  }
  guard let macText = n.mac, let mac = VZMACAddress(string: macText) else {
    throw ShimError("configure network: vmnet network needs the guest MAC")
  }
  guard let (network, bits) = parseIPv4Prefix(n.ipv4Subnet ?? ""), bits <= 30 else {
    throw ShimError("configure network: parse guest subnet \(n.ipv4Subnet ?? "")")
  }
  guard let address = parseIPv4(n.ipv4Address ?? "") else {
    throw ShimError("configure network: parse guest address \(n.ipv4Address ?? "")")
  }
  let mask: UInt32 = bits == 0 ? 0 : ~UInt32(0) << UInt32(32 - bits)
  let subnetText = "\(formatIPv4(network & mask))/\(bits)"

  var status = vmnet_return_t.VMNET_SUCCESS
  guard let config = vmnet_network_configuration_create(.VMNET_SHARED_MODE, &status), status == .VMNET_SUCCESS else {
    throw ShimError("configure network: configure vmnet network \(subnetText): status \(status.rawValue)")
  }
  // vmnet needs the host address, the first in the subnet, to start DHCP-backed
  // attachments; the network address alone leaves the guest without a lease.
  var subnetAddr = in_addr(s_addr: ((network & mask) + 1).bigEndian)
  var maskAddr = in_addr(s_addr: mask.bigEndian)
  status = vmnet_network_configuration_set_ipv4_subnet(config, &subnetAddr, &maskAddr)
  guard status == .VMNET_SUCCESS else {
    throw ShimError("configure network: set vmnet subnet \(subnetText): status \(status.rawValue)")
  }
  var client = ether_addr_t()
  withUnsafeMutableBytes(of: &client.octet) { $0.copyBytes(from: mac.ethernetAddress) }
  var reserved = in_addr(s_addr: address.bigEndian)
  status = vmnet_network_configuration_add_dhcp_reservation(config, &client, &reserved)
  guard status == .VMNET_SUCCESS else {
    throw ShimError("configure network: reserve \(formatIPv4(address)) on vmnet network \(subnetText): status \(status.rawValue)")
  }
  guard let network = vmnet_network_create(config, &status), status == .VMNET_SUCCESS else {
    throw ShimError("configure network: create vmnet network \(subnetText): status \(status.rawValue)")
  }
  Log.info("attached guest vmnet network", ["subnet": subnetText, "address": formatIPv4(address), "mac": macText])
  return VZVmnetNetworkDeviceAttachment(network: network)
}

extension VZMACAddress {
  var ethernetAddress: [UInt8] {
    string.split(separator: ":").compactMap { UInt8($0, radix: 16) }
  }
}

// parseIPv4 returns an address in host byte order.
func parseIPv4(_ text: String) -> UInt32? {
  var addr = in_addr()
  guard inet_pton(AF_INET, text, &addr) == 1 else { return nil }
  return UInt32(bigEndian: addr.s_addr)
}

func parseIPv4Prefix(_ text: String) -> (UInt32, Int)? {
  let parts = text.split(separator: "/")
  guard parts.count == 2, let address = parseIPv4(String(parts[0])), let bits = Int(parts[1]), (0...32).contains(bits) else { return nil }
  return (address, bits)
}

func formatIPv4(_ address: UInt32) -> String {
  "\(address >> 24 & 0xff).\(address >> 16 & 0xff).\(address >> 8 & 0xff).\(address & 0xff)"
}
