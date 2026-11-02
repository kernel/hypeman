import Foundation
import Virtualization
import vmnet
#if canImport(DiskImageKit)
  import DiskImageKit
#endif

// makeConfiguration builds a validated VM configuration. A Linux guest without a
// machine identifier gets a new one, recorded in config so snapshots keep it.
func makeConfiguration(_ config: inout ShimConfig) throws -> VZVirtualMachineConfiguration {
  if config.isMacGuest {
    return try makeMacConfiguration(config)
  }
  return try makeLinuxConfiguration(&config)
}

// makeMacConfiguration boots an imported macOS disk with its platform identity.
// It has no serial console, entropy device, Rosetta share or memory balloon.
func makeMacConfiguration(_ c: ShimConfig) throws -> VZVirtualMachineConfiguration {
  #if arch(arm64)
    if c.enableRosetta == true || c.enableMemoryBalloon == true || c.requireMemoryBalloon == true {
      throw ShimError("macOS guests do not support Linux Rosetta or memory ballooning")
    }
    guard let modelText = c.macHardwareModelData, !modelText.isEmpty,
      let idText = c.macMachineIdentifierData, !idText.isEmpty,
      let auxPath = c.macAuxStoragePath, !auxPath.isEmpty
    else {
      throw ShimError("create boot loader: macOS boot requires hardware model, machine identifier, and auxiliary storage")
    }
    guard let modelData = decodeBase64(modelText) else {
      throw ShimError("configure Mac platform: decode Mac hardware model")
    }
    guard let model = VZMacHardwareModel(dataRepresentation: modelData) else {
      throw ShimError("configure Mac platform: invalid Mac hardware model")
    }
    guard model.isSupported else {
      throw ShimError("configure Mac platform: Mac hardware model is unsupported on this host")
    }
    guard let idData = decodeBase64(idText) else {
      throw ShimError("configure Mac platform: decode Mac machine identifier")
    }
    guard let identifier = VZMacMachineIdentifier(dataRepresentation: idData) else {
      throw ShimError("configure Mac platform: invalid Mac machine identifier")
    }
    let platform = VZMacPlatformConfiguration()
    platform.hardwareModel = model
    platform.machineIdentifier = identifier
    platform.auxiliaryStorage = VZMacAuxiliaryStorage(url: URL(fileURLWithPath: auxPath))

    let vc = VZVirtualMachineConfiguration()
    vc.bootLoader = VZMacOSBootLoader()
    vc.cpuCount = c.vcpus ?? 0
    vc.memorySize = UInt64(max(0, c.memoryBytes ?? 0))
    vc.platform = platform
    Log.debug("macOS VM config", ["vcpus": vc.cpuCount, "memory_bytes": vc.memorySize])
    vc.networkDevices = try makeNetworkDevices(c.networks ?? [])
    vc.storageDevices = try makeStorageDevices(c.disks ?? [])

    let graphics = VZMacGraphicsDeviceConfiguration()
    // 3024x1964 pixels at 2x: 1512x982 points, the logical size of a 14-inch MacBook Pro.
    graphics.displays = [VZMacGraphicsDisplayConfiguration(widthInPixels: 3024, heightInPixels: 1964, pixelsPerInch: 160)]
    vc.graphicsDevices = [graphics]
    vc.keyboards = [VZMacKeyboardConfiguration()]
    vc.pointingDevices = [VZMacTrackpadConfiguration()]
    vc.audioDevices = [makeMacAudio()]
    vc.socketDevices = [VZVirtioSocketDeviceConfiguration()]
    do {
      try vc.validate()
    } catch {
      throw ShimError("invalid vm configuration: \(error.localizedDescription)")
    }
    return vc
  #else
    throw ShimError("macOS guests require Apple silicon")
  #endif
}

// makeMacAudio gives the guest an audio device. Virtualization attaches audio only
// through a sound device, so without one the guest boots with no CoreAudio devices:
// `system_profiler SPAudioDataType` lists none and `afplay` fails with
// `AudioQueueStart failed (-66680)`.
//
// The output stream has no host sink. The samples are discarded on the host, which
// is what a headless browser VM wants -- nothing should play on the machine running
// the guests -- while the guest still has a real output device, so applications can
// render audio and ScreenCaptureKit has a mix to capture for the live view.
func makeMacAudio() -> VZAudioDeviceConfiguration {
  let sound = VZVirtioSoundDeviceConfiguration()
  sound.streams = [VZVirtioSoundDeviceOutputStreamConfiguration()]
  return sound
}

func makeLinuxConfiguration(_ c: inout ShimConfig) throws -> VZVirtualMachineConfiguration {
  // vz uses hvc0 for the serial console.
  var kernelArgs = c.kernelArgs ?? ""
  if kernelArgs.isEmpty {
    kernelArgs = "console=hvc0 root=/dev/vda"
  } else {
    kernelArgs = kernelArgs.replacingOccurrences(of: "console=ttyS0", with: "console=hvc0")
  }
  let bootLoader = VZLinuxBootLoader(kernelURL: URL(fileURLWithPath: c.kernelPath ?? ""))
  bootLoader.commandLine = kernelArgs
  if let initrd = c.initrdPath, !initrd.isEmpty {
    bootLoader.initialRamdiskURL = URL(fileURLWithPath: initrd)
  }

  let vc = VZVirtualMachineConfiguration()
  vc.bootLoader = bootLoader
  vc.cpuCount = cpuCount(c.vcpus ?? 0)
  vc.memorySize = memorySize(UInt64(max(0, c.memoryBytes ?? 0)))
  Log.debug("VM config", ["vcpus": vc.cpuCount, "memory_bytes": vc.memorySize, "kernel": c.kernelPath ?? "", "initrd": c.initrdPath ?? ""])

  vc.serialPorts = [try makeSerialPort(c.serialLogPath)]
  vc.networkDevices = try makeNetworkDevices(c.networks ?? [])
  vc.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
  vc.storageDevices = try makeStorageDevices(c.disks ?? [])
  vc.directorySharingDevices = try makeRosettaShare(c)
  vc.platform = try makeGenericPlatform(&c)
  vc.socketDevices = [VZVirtioSocketDeviceConfiguration()]
  if c.enableMemoryBalloon == true {
    vc.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
  }
  do {
    try vc.validate()
  } catch {
    throw ShimError("invalid vm configuration: \(error.localizedDescription)")
  }
  return vc
}

func makeGenericPlatform(_ c: inout ShimConfig) throws -> VZGenericPlatformConfiguration {
  let platform = VZGenericPlatformConfiguration()
  if let text = c.machineIdentifierData, !text.isEmpty {
    guard let data = decodeBase64(text), let identifier = VZGenericMachineIdentifier(dataRepresentation: data) else {
      throw ShimError("configure platform: recreate machine identifier")
    }
    platform.machineIdentifier = identifier
  } else {
    let identifier = VZGenericMachineIdentifier()
    platform.machineIdentifier = identifier
    c.machineIdentifierData = identifier.dataRepresentation.base64EncodedString()
  }
  return platform
}

func makeSerialPort(_ logPath: String?) throws -> VZSerialPortConfiguration {
  guard let reader = FileHandle(forReadingAtPath: "/dev/null") else {
    throw ShimError("configure serial: open /dev/null for reading")
  }
  let writer: FileHandle
  if let logPath, !logPath.isEmpty {
    let fd = open(logPath, O_CREAT | O_WRONLY | O_APPEND, 0o644)
    guard fd >= 0 else { throw ShimError("configure serial: open serial log file: \(String(cString: strerror(errno)))") }
    writer = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
  } else {
    guard let null = FileHandle(forWritingAtPath: "/dev/null") else {
      throw ShimError("configure serial: open /dev/null for writing")
    }
    writer = null
  }
  let port = VZVirtioConsoleDeviceSerialPortConfiguration()
  port.attachment = VZFileHandleSerialPortAttachment(fileHandleForReading: reader, fileHandleForWriting: writer)
  return port
}

// makeRosettaShare attaches the Linux Rosetta virtio-fs share when requested. The
// host must have Rosetta installed; anything else is a startup error.
func makeRosettaShare(_ c: ShimConfig) throws -> [VZDirectorySharingDeviceConfiguration] {
  guard c.enableRosetta == true else { return [] }
  #if arch(arm64)
    switch VZLinuxRosettaDirectoryShare.availability {
    case .installed:
      break
    case .notInstalled:
      throw ShimError("configure directory sharing: rosetta requested but not installed (run: softwareupdate --install-rosetta)")
    default:
      throw ShimError("configure directory sharing: rosetta requested but not supported on this host (requires Apple silicon + macOS 13+)")
    }
    let device = VZVirtioFileSystemDeviceConfiguration(tag: rosettaMountTag)
    do {
      device.share = try VZLinuxRosettaDirectoryShare()
    } catch {
      throw ShimError("configure directory sharing: create rosetta directory share: \(error.localizedDescription)")
    }
    Log.info("attached rosetta directory share", ["tag": rosettaMountTag])
    return [device]
  #else
    throw ShimError("configure directory sharing: rosetta requested but not supported on this host (requires Apple silicon + macOS 13+)")
  #endif
}

func makeStorageDevices(_ disks: [DiskConfig]) throws -> [VZStorageDeviceConfiguration] {
  try disks.map { disk in
    guard FileManager.default.fileExists(atPath: disk.path) else {
      throw ShimError("configure storage: disk image not found: \(disk.path)")
    }
    if disk.path.hasSuffix(".qcow2") {
      throw ShimError("configure storage: qcow2 not supported by vz, use raw format: \(disk.path)")
    }
    let readOnly = disk.readonly ?? false
    let attachment: VZStorageDeviceAttachment
    do {
      if let parents = disk.parents, !parents.isEmpty {
        attachment = try stackedAttachment(parents: parents, overlay: disk.path, readOnly: readOnly)
      } else {
        attachment = try VZDiskImageStorageDeviceAttachment(url: URL(fileURLWithPath: disk.path), readOnly: readOnly)
      }
    } catch let error as ShimError {
      throw error
    } catch {
      throw ShimError("configure storage: create disk attachment for \(disk.path): \(error.localizedDescription)")
    }
    return VZVirtioBlockDeviceConfiguration(attachment: attachment)
  }
}

// stackedAttachment attaches an ASIF overlay above its read-only parents, base
// first. Each image must name the one below it as its parent; hypeman creates them.
func stackedAttachment(parents: [String], overlay: String, readOnly: Bool) throws -> VZStorageDeviceAttachment {
  #if canImport(DiskImageKit)
    if #available(macOS 27.0, *) {
      for path in parents where !FileManager.default.fileExists(atPath: path) {
        throw ShimError("configure storage: disk parent not found: \(path)")
      }
      var stacked = try DiskImage(opening: .open(url: URL(fileURLWithPath: parents[0]), mode: .readOnly))
      for path in parents.dropFirst() {
        stacked = try stacked.appending(try openOverlay(path, mode: .readOnly))
      }
      stacked = try stacked.appending(try openOverlay(overlay, mode: readOnly ? .readOnly : .readWrite))
      Log.info("attached stacked disk", ["base": parents[0], "overlay": overlay, "layers": (stacked as? any StackedImage)?.layers.count ?? 1])
      return try VZDiskImageStorageDeviceAttachment(diskImage: stacked, cachingMode: .automatic, synchronizationMode: .full)
    }
  #endif
  throw ShimError("configure storage: stacked disks require DiskImageKit on macOS 27 or newer")
}

#if canImport(DiskImageKit)
  @available(macOS 27.0, *)
  func openOverlay(_ path: String, mode: OpenConfiguration.Mode) throws -> DiskImage {
    let image = try DiskImage(opening: .open(url: URL(fileURLWithPath: path), mode: mode))
    guard image.format == .asif else {
      throw ShimError("configure storage: overlay must use ASIF format: \(path)")
    }
    return image
  }
#endif

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

func cpuCount(_ requested: Int) -> Int {
  var count = requested
  if count == 0 {
    count = max(1, ProcessInfo.processInfo.processorCount - 1)
  }
  return min(max(count, VZVirtualMachineConfiguration.minimumAllowedCPUCount), VZVirtualMachineConfiguration.maximumAllowedCPUCount)
}

func memorySize(_ requested: UInt64) -> UInt64 {
  // 2 GiB safety default; the caller normally provides this.
  let size = requested == 0 ? 2 << 30 : requested
  return min(max(size, VZVirtualMachineConfiguration.minimumAllowedMemorySize), VZVirtualMachineConfiguration.maximumAllowedMemorySize)
}
