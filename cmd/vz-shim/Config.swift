import Foundation

// These types mirror lib/hypervisor/vz/shimconfig. Every field is optional so a
// config written by any hypeman version decodes, and an absent field encodes as
// absent, which Go reads back as its zero value.

struct ShimConfig: Codable {
  var vcpus: Int?
  var memoryBytes: Int64?
  var disks: [DiskConfig]?
  var networks: [NetworkConfig]?
  var serialLogPath: String?
  var kernelPath: String?
  var initrdPath: String?
  var kernelArgs: String?
  var macHardwareModelData: String?
  var macMachineIdentifierData: String?
  var macAuxStoragePath: String?
  var enableMemoryBalloon: Bool?
  var requireMemoryBalloon: Bool?
  var enableRosetta: Bool?
  var controlSocket: String?
  var vsockSocket: String?
  var logPath: String?
  var stopReceiptPath: String?
  var machineIdentifierData: String?
  var restoreMachineStatePath: String?

  enum CodingKeys: String, CodingKey {
    case vcpus
    case memoryBytes = "memory_bytes"
    case disks, networks
    case serialLogPath = "serial_log_path"
    case kernelPath = "kernel_path"
    case initrdPath = "initrd_path"
    case kernelArgs = "kernel_args"
    case macHardwareModelData = "mac_hardware_model_data"
    case macMachineIdentifierData = "mac_machine_identifier_data"
    case macAuxStoragePath = "mac_aux_storage_path"
    case enableMemoryBalloon = "enable_memory_balloon"
    case requireMemoryBalloon = "require_memory_balloon"
    case enableRosetta = "enable_rosetta"
    case controlSocket = "control_socket"
    case vsockSocket = "vsock_socket"
    case logPath = "log_path"
    case stopReceiptPath = "stop_receipt_path"
    case machineIdentifierData = "machine_identifier_data"
    case restoreMachineStatePath = "restore_machine_state_path"
  }

  var isMacGuest: Bool {
    !(macHardwareModelData ?? "").isEmpty || !(macMachineIdentifierData ?? "").isEmpty || !(macAuxStoragePath ?? "").isEmpty
  }
}

struct DiskConfig: Codable {
  var path: String
  var readonly: Bool?
  // parents, when set, are the read-only images below path, base first. path is
  // then the ASIF overlay on top of them.
  var parents: [String]?
}

struct NetworkConfig: Codable {
  var mac: String?
  var ipv4Subnet: String?
  var ipv4Address: String?

  enum CodingKeys: String, CodingKey {
    case mac
    case ipv4Subnet = "ipv4_subnet"
    case ipv4Address = "ipv4_address"
  }
}

struct SnapshotManifest: Codable {
  var hypervisor: String
  var machineStateFile: String
  var shimConfig: ShimConfig

  enum CodingKeys: String, CodingKey {
    case hypervisor
    case machineStateFile = "machine_state_file"
    case shimConfig = "shim_config"
  }
}

let snapshotManifestFile = "config.json"
let snapshotMachineStateFile = "machine-state.vzm"
let rosettaMountTag = "rosetta"

struct ShimError: Error, CustomStringConvertible {
  let description: String
  init(_ description: String) { self.description = description }
}

// decodeBase64 accepts what Go's base64 decoder accepts, including line breaks.
func decodeBase64(_ s: String) -> Data? {
  Data(base64Encoded: s, options: .ignoreUnknownCharacters)
}
