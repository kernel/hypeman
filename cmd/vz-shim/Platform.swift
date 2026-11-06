import Foundation
import Virtualization

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
