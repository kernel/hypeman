import Foundation

// Log writes JSON lines in the shape of Go's slog JSON handler, so the shim log
// that hypeman attaches to startup errors reads the same as before.
enum Log {
  private static var handle = FileHandle.standardError
  private static let lock = NSLock()
  private static let formatter: ISO8601DateFormatter = {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return f
  }()

  static func setup(path: String?) throws {
    guard let path, !path.isEmpty else { return }
    try FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
    let fd = open(path, O_CREAT | O_WRONLY | O_APPEND, 0o644)
    guard fd >= 0 else { throw ShimError("open log file: \(String(cString: strerror(errno)))") }
    handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
  }

  static func debug(_ msg: String, _ fields: [String: Any] = [:]) { write("DEBUG", msg, fields) }
  static func info(_ msg: String, _ fields: [String: Any] = [:]) { write("INFO", msg, fields) }
  static func warn(_ msg: String, _ fields: [String: Any] = [:]) { write("WARN", msg, fields) }
  static func error(_ msg: String, _ fields: [String: Any] = [:]) { write("ERROR", msg, fields) }

  private static func write(_ level: String, _ msg: String, _ fields: [String: Any]) {
    var record = fields.mapValues { value -> Any in
      if value is String || value is Int || value is Int64 || value is UInt32 || value is UInt64 || value is Bool { return value }
      return String(describing: value)
    }
    record["time"] = formatter.string(from: Date())
    record["level"] = level
    record["msg"] = msg
    guard var line = try? JSONSerialization.data(withJSONObject: record, options: [.sortedKeys]) else { return }
    line.append(0x0a)
    lock.lock()
    handle.write(line)
    lock.unlock()
  }
}

func printError(_ message: String) {
  FileHandle.standardError.write((message + "\n").data(using: .utf8)!)
}
