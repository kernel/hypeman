import Foundation
import Virtualization

// Shim owns the VM. The VM runs on the main queue, so every VM call below happens
// there; request handlers run on their own threads and wait for it.
final class Shim: NSObject, VZVirtualMachineDelegate {
  let vm: VZVirtualMachine
  let vmConfig: VZVirtualMachineConfiguration
  let config: ShimConfig
  // Serializes control operations, as the Go shim's mutex did.
  private let ops = NSLock()
  // forcedStop is set, on the main queue, before the shim stops the VM itself, so
  // that stop is not mistaken for the guest powering off.
  private var forcedStop = false
  private var listeners: [UnixListener] = []
  private var stateObservation: NSKeyValueObservation?

  init(vm: VZVirtualMachine, vmConfig: VZVirtualMachineConfiguration, config: ShimConfig) {
    self.vm = vm
    self.vmConfig = vmConfig
    self.config = config
    super.init()
    vm.delegate = self
    stateObservation = vm.observe(\.state) { vm, _ in
      Log.info("VM state changed", ["state": stateName(vm.state)])
    }
  }

  // MARK: Lifecycle

  func listen() throws {
    let control = try UnixListener(path: config.controlSocket ?? "")
    let vsock = try UnixListener(path: config.vsockSocket ?? "")
    listeners = [control, vsock]
    control.serve { [self] fd in serveHTTP(fd, route) }
    vsock.serve { [self] fd in proxyVsock(fd) }
    Log.info("control API listening", ["socket": control.path])
    Log.info("vsock proxy listening", ["socket": vsock.path])
  }

  func guestDidStop(_ virtualMachine: VZVirtualMachine) {
    if !forcedStop {
      writeStopReceipt()
    }
    Log.info("VM stopped, shutting down shim")
    finish()
  }

  func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
    Log.error("VM stopped with error", ["error": error.localizedDescription])
    finish()
  }

  // stopForSignal runs on the main queue.
  func stopForSignal() {
    if vm.state == .running && vm.canStop {
      Log.info("stopping VM")
      forcedStop = true
      vm.stop { _ in self.finish() }
      return
    }
    finish()
  }

  private func finish() {
    for listener in listeners {
      listener.close()
    }
    Log.info("vz-shim shutdown complete")
    exit(0)
  }

  // writeStopReceipt records that the guest stopped the VM itself. The host counts
  // a macOS stop as graceful only with this receipt.
  private func writeStopReceipt() {
    guard let path = config.stopReceiptPath, !path.isEmpty else { return }
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let data = Data((formatter.string(from: Date()) + "\n").utf8)
    let fd = open(path, O_CREAT | O_WRONLY | O_TRUNC, 0o600)
    guard fd >= 0, writeAll(fd, data) else {
      Log.error("failed to write stop receipt", ["error": errnoString(), "path": path])
      if fd >= 0 { close(fd) }
      return
    }
    close(fd)
  }

  // MARK: Main-queue helpers

  private func onMain<T>(_ body: () throws -> T) rethrows -> T {
    try DispatchQueue.main.sync(execute: body)
  }

  // awaitMain starts an asynchronous VM operation on the main queue and waits for
  // its completion handler.
  private func awaitMain(_ start: @escaping (@escaping (Error?) -> Void) -> Void) -> Error? {
    let done = DispatchSemaphore(value: 0)
    var result: Error?
    DispatchQueue.main.async {
      start { error in
        result = error
        done.signal()
      }
    }
    done.wait()
    return result
  }

  // MARK: Control API

  private static let routes: [String: Set<String>] = [
    "/api/v1/vm.info": ["GET"], "/api/v1/vm.pause": ["PUT"], "/api/v1/vm.resume": ["PUT"],
    "/api/v1/vm.shutdown": ["PUT"], "/api/v1/vm.snapshot": ["PUT"], "/api/v1/vm.snapshot-support": ["GET"],
    "/api/v1/vm.balloon": ["GET", "PUT"],
    "/api/v1/vmm.ping": ["GET"], "/api/v1/vmm.shutdown": ["PUT"],
  ]

  private func route(_ request: HTTPRequest) -> HTTPResponse {
    guard let methods = Shim.routes[request.path] else { return .text(404, "404 page not found") }
    guard methods.contains(request.method) else { return .text(405, "Method Not Allowed") }
    switch (request.method, request.path) {
    case ("GET", "/api/v1/vmm.ping"):
      return HTTPResponse(status: 200, body: Data("OK".utf8))
    case ("PUT", "/api/v1/vmm.shutdown"):
      return vmmShutdown()
    default:
      break
    }
    ops.lock()
    defer { ops.unlock() }
    switch (request.method, request.path) {
    case ("GET", "/api/v1/vm.info"): return info()
    case ("PUT", "/api/v1/vm.pause"): return pause()
    case ("PUT", "/api/v1/vm.resume"): return resume()
    case ("PUT", "/api/v1/vm.shutdown"): return requestStop()
    case ("GET", "/api/v1/vm.snapshot-support"): return snapshotSupport()
    case ("PUT", "/api/v1/vm.snapshot"): return snapshot(request.body)
    case ("GET", "/api/v1/vm.balloon"): return getBalloon()
    default: return setBalloon(request.body)
    }
  }

  private func info() -> HTTPResponse {
    let (state, balloons) = onMain { (stateName(vm.state), vm.memoryBalloonDevices.count) }
    var response: [String: Any] = ["state": state]
    if balloons > 0 {
      response["memory_balloon_devices"] = balloons
    }
    return .json(response)
  }

  private func pause() -> HTTPResponse {
    guard onMain({ vm.canPause }) else { return .text(400, "cannot pause VM") }
    if let error = awaitMain({ done in self.vm.pause { done($0.error) } }) {
      Log.error("failed to pause VM", ["error": error.localizedDescription])
      return .text(500, "pause failed: \(error.localizedDescription)")
    }
    Log.info("VM paused")
    return .noContent()
  }

  private func resume() -> HTTPResponse {
    guard onMain({ vm.canResume }) else { return .text(400, "cannot resume VM") }
    if let error = awaitMain({ done in self.vm.resume { done($0.error) } }) {
      Log.error("failed to resume VM", ["error": error.localizedDescription])
      return .text(500, "resume failed: \(error.localizedDescription)")
    }
    Log.info("VM resumed")
    return .noContent()
  }

  // requestStop sends the guest a power-button event. The instance manager handles
  // the timeout and forced stop if the guest does not shut down.
  private func requestStop() -> HTTPResponse {
    do {
      try onMain { try vm.requestStop() }
    } catch {
      Log.error("RequestStop failed", ["error": error.localizedDescription])
      return .text(500, "shutdown failed: \(error.localizedDescription)")
    }
    Log.info("VM graceful shutdown requested (ACPI power button)")
    return .noContent()
  }

  private func snapshotSupport() -> HTTPResponse {
    var response: [String: Any] = ["supported": true]
    if let reason = saveRestoreUnsupportedReason() {
      response["supported"] = false
      response["reason"] = reason
    }
    return .json(response)
  }

  func saveRestoreUnsupportedReason() -> String? {
    do {
      try vmConfig.validateSaveRestoreSupport()
      return nil
    } catch {
      return error.localizedDescription
    }
  }

  private func snapshot(_ body: Data) -> HTTPResponse {
    let destination: String
    do {
      let object = try JSONSerialization.jsonObject(with: body) as? [String: Any]
      destination = object?["destination_path"] as? String ?? ""
    } catch {
      return .text(400, "invalid snapshot request: \(error.localizedDescription)")
    }
    guard !destination.isEmpty else { return .text(400, "destination_path is required") }
    guard onMain({ vm.state == .paused }) else { return .text(400, "vm must be paused before snapshot") }
    if let reason = saveRestoreUnsupportedReason() {
      return .text(400, "save/restore not supported: \(reason)")
    }
    let fm = FileManager.default
    do {
      try fm.createDirectory(atPath: destination, withIntermediateDirectories: true)
    } catch {
      return .text(500, "create snapshot dir failed: \(error.localizedDescription)")
    }
    var complete = false
    defer {
      if !complete { try? fm.removeItem(atPath: destination) }
    }
    let statePath = (destination as NSString).appendingPathComponent(snapshotMachineStateFile)
    try? fm.removeItem(atPath: statePath)
    if let error = awaitMain({ done in self.vm.saveMachineStateTo(url: URL(fileURLWithPath: statePath), completionHandler: done) }) {
      return .text(500, "save machine state failed: \(error.localizedDescription)")
    }
    var shimConfig = config
    // Runtime-only: the restore path is filled in by the caller on restore.
    shimConfig.restoreMachineStatePath = nil
    let manifestPath = (destination as NSString).appendingPathComponent(snapshotManifestFile)
    do {
      let manifest = SnapshotManifest(hypervisor: "vz", machineStateFile: snapshotMachineStateFile, shimConfig: shimConfig)
      try JSONEncoder().encode(manifest).write(to: URL(fileURLWithPath: manifestPath + ".tmp"))
      try? fm.removeItem(atPath: manifestPath)
      try fm.moveItem(atPath: manifestPath + ".tmp", toPath: manifestPath)
    } catch {
      return .text(500, "write manifest failed: \(error.localizedDescription)")
    }
    complete = true
    Log.info("VM snapshot saved", ["destination": destination, "machine_state": statePath])
    return .noContent()
  }

  private func balloon() -> VZVirtioTraditionalMemoryBalloonDevice? {
    vm.memoryBalloonDevices.compactMap { $0 as? VZVirtioTraditionalMemoryBalloonDevice }.first
  }

  private func getBalloon() -> HTTPResponse {
    guard let target = onMain({ balloon()?.targetVirtualMachineMemorySize }) else {
      return .text(404, "no memory balloon device configured")
    }
    return .json(["target_guest_memory_bytes": Int64(target)])
  }

  private func setBalloon(_ body: Data) -> HTTPResponse {
    guard onMain({ balloon() != nil }) else { return .text(404, "no memory balloon device configured") }
    let object = try? JSONSerialization.jsonObject(with: body) as? [String: Any]
    guard let target = (object?["target_guest_memory_bytes"] as? NSNumber)?.int64Value else {
      return .text(400, "invalid balloon request: target_guest_memory_bytes is required")
    }
    guard target >= 0 else { return .text(400, "target_guest_memory_bytes must be non-negative") }
    onMain { balloon()?.targetVirtualMachineMemorySize = UInt64(target) }
    return .noContent()
  }

  private func vmmShutdown() -> HTTPResponse {
    Log.info("VMM shutdown requested")
    var response = HTTPResponse.noContent()
    response.after = { [self] in
      // The process exits once the VM stops.
      DispatchQueue.main.async {
        guard self.vm.canStop else { return }
        self.forcedStop = true
        self.vm.stop { _ in self.finish() }
      }
    }
    return response
  }

  // MARK: Vsock proxy

  // proxyVsock speaks Cloud Hypervisor's vsock protocol: the client sends
  // "CONNECT {port}\n", the shim answers "OK {port}\n", then proxies bytes.
  private func proxyVsock(_ client: Int32) {
    defer { close(client) }
    guard let line = readLine(client, limit: 64) else {
      Log.error("failed to read vsock handshake")
      return
    }
    let fields = line.trimmingCharacters(in: .newlines).split(separator: " ")
    guard fields.count == 2, fields[0] == "CONNECT", let port = UInt32(fields[1]) else {
      Log.error("invalid vsock handshake", ["cmd": line])
      _ = writeAll(client, Data("ERR invalid command: \(line)".utf8))
      return
    }
    guard let device = onMain({ vm.socketDevices.first as? VZVirtioSocketDevice }) else {
      Log.error("no vsock device configured")
      _ = writeAll(client, Data("ERR no vsock device\n".utf8))
      return
    }
    var connection: VZVirtioSocketConnection?
    let connectError = awaitMain { done in
      device.connect(toPort: port) { result in
        switch result {
        case .success(let c):
          connection = c
          done(nil)
        case .failure(let error):
          done(error)
        }
      }
    }
    guard let connection else {
      let message = connectError?.localizedDescription ?? "unknown error"
      Log.error("failed to connect to guest vsock", ["port": port, "error": message])
      _ = writeAll(client, Data("ERR connect failed: \(message)\n".utf8))
      return
    }
    defer { connection.close() }
    guard writeAll(client, Data("OK \(port)\n".utf8)) else {
      Log.error("failed to send OK response")
      return
    }
    Log.debug("vsock connection established", ["port": port])
    let guest = connection.fileDescriptor
    // When either direction ends, shut both sockets down to wake the other copy,
    // then wait for it before the descriptors are closed.
    let finished = DispatchSemaphore(value: 0)
    for (src, dst) in [(client, guest), (guest, client)] {
      Thread.detachNewThread {
        pump(from: src, to: dst)
        shutdown(client, SHUT_RDWR)
        shutdown(guest, SHUT_RDWR)
        finished.signal()
      }
    }
    finished.wait()
    finished.wait()
  }
}

extension Result {
  var error: Failure? {
    if case .failure(let error) = self { return error }
    return nil
  }
}

// stateName matches the Cloud Hypervisor state names the client expects.
func stateName(_ state: VZVirtualMachine.State) -> String {
  switch state {
  case .stopped: return "Shutdown"
  case .running: return "Running"
  case .paused: return "Paused"
  case .error: return "Error"
  case .starting: return "Starting"
  case .pausing: return "Pausing"
  case .resuming: return "Resuming"
  case .stopping: return "Stopping"
  case .saving: return "Saving"
  case .restoring: return "Restoring"
  @unknown default: return "Unknown"
  }
}
