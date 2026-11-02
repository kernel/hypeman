// hypeman-vz-shim is a subprocess that hosts one vz VM, so VMs survive hypeman
// restarts. Hypeman starts it with -config <ShimConfig JSON> and talks to it over
// the control and vsock sockets named in that config.
import CryptoKit
import Foundation
import Virtualization

func configArgument() -> String? {
  let args = CommandLine.arguments.dropFirst()
  var iterator = args.makeIterator()
  while let arg = iterator.next() {
    if arg == "-config" || arg == "--config" {
      return iterator.next()
    }
    for prefix in ["-config=", "--config="] where arg.hasPrefix(prefix) {
      return String(arg.dropFirst(prefix.count))
    }
  }
  return nil
}

// lockMacOSIdentity holds an exclusive lock on the Mac machine identity until the
// process exits, including while paused. The lock file is never unlinked: another
// waiter may already hold its inode open.
func lockMacOSIdentity(_ config: ShimConfig) throws -> Int32? {
  guard let text = config.macMachineIdentifierData, !text.isEmpty else { return nil }
  guard let data = decodeBase64(text) else { throw ShimError("decode Mac identity for admission") }
  let hash = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
  // Fixed directory, not TMPDIR: servers with different environments must contend
  // on the same lock. The name is predictable, so refuse symlinks and files owned
  // by anyone else.
  let path = "/tmp/hypeman-mac-identity-\(getuid())-\(hash).lock"
  let fd = open(path, O_CREAT | O_RDWR | O_NOFOLLOW, 0o600)
  guard fd >= 0 else { throw ShimError("open \(path): \(errnoString())") }
  var info = stat()
  guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG, info.st_uid == getuid() else {
    close(fd)
    throw ShimError("Mac identity lock path is not an owned regular file")
  }
  guard flock(fd, LOCK_EX | LOCK_NB) == 0 else {
    let message = errnoString()
    close(fd)
    throw ShimError("Mac machine identity already in use: \(message)")
  }
  return fd
}

signal(SIGPIPE, SIG_IGN)

guard let configJSON = configArgument(), !configJSON.isEmpty else {
  printError("error: -config is required")
  exit(1)
}
var config: ShimConfig
do {
  config = try JSONDecoder().decode(ShimConfig.self, from: Data(configJSON.utf8))
} catch {
  printError("error: invalid config JSON: \(error)")
  exit(1)
}
do {
  try Log.setup(path: config.logPath)
} catch {
  printError("error: setup logging: \(error)")
  exit(1)
}
Log.info("vz-shim starting", ["control_socket": config.controlSocket ?? "", "vsock_socket": config.vsockSocket ?? ""])

let identityLock: Int32?
do {
  identityLock = try lockMacOSIdentity(config)
} catch {
  printError("macOS identity admission: \(error)")
  exit(1)
}

let vmConfig: VZVirtualMachineConfiguration
do {
  vmConfig = try makeConfiguration(&config)
} catch {
  Log.error("failed to create VM", ["error": "\(error)"])
  printError("failed to create VM: \(error)")
  exit(1)
}
let shim = Shim(vm: VZVirtualMachine(configuration: vmConfig, queue: .main), vmConfig: vmConfig, config: config)

func serveOrExit() {
  do {
    try shim.listen()
  } catch {
    Log.error("failed to listen", ["error": "\(error)"])
    exit(1)
  }
}

if let restorePath = config.restoreMachineStatePath, !restorePath.isEmpty {
  if let reason = shim.saveRestoreUnsupportedReason() {
    Log.error("save/restore not supported for VM config", ["error": reason])
    printError("save/restore not supported for VM config: \(reason)")
    exit(1)
  }
  shim.vm.restoreMachineStateFrom(url: URL(fileURLWithPath: restorePath)) { error in
    if let error {
      Log.error("failed to restore VM machine state", ["error": error.localizedDescription, "path": restorePath])
      printError("failed to restore VM machine state: \(error.localizedDescription)")
      exit(1)
    }
    Log.info("VM restored from machine state", ["path": restorePath, "state": stateName(shim.vm.state)])
    serveOrExit()
  }
} else {
  shim.vm.start { result in
    if let error = result.error {
      Log.error("failed to start VM", ["error": error.localizedDescription])
      printError("failed to start VM: \(error.localizedDescription)")
      exit(1)
    }
    Log.info("VM started", ["vcpus": config.vcpus ?? 0, "memory_mb": (config.memoryBytes ?? 0) / 1024 / 1024])
    serveOrExit()
  }
}

var signalSources: [DispatchSourceSignal] = []
for number in [SIGTERM, SIGINT] {
  signal(number, SIG_IGN)
  let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
  source.setEventHandler {
    Log.info("received signal, shutting down", ["signal": number])
    shim.stopForSignal()
  }
  source.resume()
  signalSources.append(source)
}

withExtendedLifetime(identityLock) {
  dispatchMain()
}
