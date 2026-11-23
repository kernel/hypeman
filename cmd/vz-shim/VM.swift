import Foundation
import Virtualization

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
