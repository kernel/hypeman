import DiskImageKit
import Foundation
import Virtualization

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
  guard #available(macOS 27.0, *) else {
    throw ShimError("configure storage: stacked disks require macOS 27 or newer")
  }
  for path in parents where !FileManager.default.fileExists(atPath: path) {
    throw ShimError("configure storage: disk parent not found: \(path)")
  }
  var stacked = try openASIF(parents[0], mode: .readOnly)
  for path in parents.dropFirst() {
    stacked = try stacked.appending(try openASIF(path, mode: .readOnly))
  }
  stacked = try stacked.appending(try openASIF(overlay, mode: readOnly ? .readOnly : .readWrite))
  Log.info("attached stacked disk", ["base": parents[0], "overlay": overlay, "layers": parents.count + 1])
  return try VZDiskImageStorageDeviceAttachment(diskImage: stacked, cachingMode: .automatic, synchronizationMode: .full)
}

// openASIF opens one image of a stack, which must be ASIF: the base and every overlay.
@available(macOS 27.0, *)
func openASIF(_ path: String, mode: OpenConfiguration.Mode) throws -> DiskImage {
  let image = try DiskImage(opening: .open(url: URL(fileURLWithPath: path), mode: mode))
  guard image.format == .asif else {
    throw ShimError("configure storage: stacked disk images must use ASIF format: \(path)")
  }
  return image
}
