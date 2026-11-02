import Foundation

// UnixListener accepts stream connections on a Unix socket, each handled on its own
// thread. Closing it unlinks the socket path, as Go's listener does.
final class UnixListener {
  let path: String
  private let fd: Int32

  init(path: String) throws {
    unlink(path)
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let capacity = MemoryLayout.size(ofValue: addr.sun_path)
    guard path.utf8.count < capacity else { throw ShimError("socket path too long: \(path)") }
    withUnsafeMutableBytes(of: &addr.sun_path) { buffer in
      buffer.copyBytes(from: path.utf8)
      buffer[path.utf8.count] = 0
    }
    addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { throw ShimError("socket: \(errnoString())") }
    let bound = withUnsafePointer(to: &addr) {
      $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    guard bound == 0, listen(fd, 64) == 0 else {
      let message = errnoString()
      Darwin.close(fd)
      throw ShimError("listen on \(path): \(message)")
    }
    self.path = path
    self.fd = fd
  }

  func serve(_ handle: @escaping (Int32) -> Void) {
    Thread.detachNewThread { [fd] in
      while true {
        let conn = accept(fd, nil, nil)
        if conn < 0 {
          if errno == EINTR { continue }
          return
        }
        var on: Int32 = 1
        setsockopt(conn, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        Thread.detachNewThread { handle(conn) }
      }
    }
  }

  func close() {
    Darwin.close(fd)
    unlink(path)
  }
}

func errnoString() -> String {
  String(cString: strerror(errno))
}

func writeAll(_ fd: Int32, _ data: Data) -> Bool {
  data.withUnsafeBytes { buffer -> Bool in
    var offset = 0
    while offset < buffer.count {
      let n = write(fd, buffer.baseAddress! + offset, buffer.count - offset)
      if n < 0 {
        if errno == EINTR { continue }
        return false
      }
      offset += n
    }
    return true
  }
}

// readLine reads one newline-terminated line a byte at a time, so nothing after the
// newline is consumed.
func readLine(_ fd: Int32, limit: Int) -> String? {
  var bytes: [UInt8] = []
  var byte: UInt8 = 0
  while bytes.count < limit {
    let n = read(fd, &byte, 1)
    if n < 0, errno == EINTR { continue }
    if n <= 0 { return nil }
    bytes.append(byte)
    if byte == 0x0a { return String(decoding: bytes, as: UTF8.self) }
  }
  return nil
}

// pump copies src to dst until either side fails or reaches EOF.
func pump(from src: Int32, to dst: Int32) {
  var buffer = [UInt8](repeating: 0, count: 64 << 10)
  while true {
    let n = read(src, &buffer, buffer.count)
    if n < 0, errno == EINTR { continue }
    if n <= 0 { return }
    if !writeAll(dst, Data(buffer[0..<n])) { return }
  }
}

// HTTPRequest is one request on a connection that serves exactly one. Hypeman's
// client disables keep-alives, so every response closes its connection.
struct HTTPRequest {
  var method: String
  var path: String
  var body: Data
}

struct HTTPResponse {
  var status: Int
  var contentType = "text/plain; charset=utf-8"
  var body = Data()
  // after runs once the response has been written.
  var after: (() -> Void)?

  static func noContent() -> HTTPResponse { HTTPResponse(status: 204) }

  static func text(_ status: Int, _ message: String) -> HTTPResponse {
    HTTPResponse(status: status, body: (message + "\n").data(using: .utf8)!)
  }

  static func json(_ object: Any) -> HTTPResponse {
    let data = (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])) ?? Data("{}".utf8)
    return HTTPResponse(status: 200, contentType: "application/json", body: data + Data([0x0a]))
  }
}

private let reasons = [200: "OK", 204: "No Content", 400: "Bad Request", 404: "Not Found", 405: "Method Not Allowed", 413: "Payload Too Large", 500: "Internal Server Error"]

func serveHTTP(_ fd: Int32, _ handle: (HTTPRequest) -> HTTPResponse) {
  defer { close(fd) }
  guard let request = readHTTPRequest(fd) else {
    _ = writeHTTPResponse(fd, .text(400, "malformed request"))
    return
  }
  let response = handle(request)
  _ = writeHTTPResponse(fd, response)
  response.after?()
}

private func readHTTPRequest(_ fd: Int32) -> HTTPRequest? {
  var data = Data()
  var buffer = [UInt8](repeating: 0, count: 4096)
  let separator = Data("\r\n\r\n".utf8)
  var headerEnd: Range<Data.Index>?
  while headerEnd == nil {
    guard data.count < 64 << 10 else { return nil }
    let n = read(fd, &buffer, buffer.count)
    if n < 0, errno == EINTR { continue }
    if n <= 0 { return nil }
    data.append(contentsOf: buffer[0..<n])
    headerEnd = data.range(of: separator)
  }
  let head = String(decoding: data[..<headerEnd!.lowerBound], as: UTF8.self)
  var lines = head.components(separatedBy: "\r\n")
  let requestLine = lines.removeFirst().split(separator: " ")
  guard requestLine.count >= 2 else { return nil }
  var contentLength = 0
  for line in lines {
    let parts = line.split(separator: ":", maxSplits: 1)
    if parts.count == 2, parts[0].lowercased() == "content-length" {
      contentLength = Int(parts[1].trimmingCharacters(in: .whitespaces)) ?? -1
    }
  }
  guard (0...(1 << 20)).contains(contentLength) else { return nil }
  var body = Data(data[headerEnd!.upperBound...])
  while body.count < contentLength {
    let n = read(fd, &buffer, min(buffer.count, contentLength - body.count))
    if n < 0, errno == EINTR { continue }
    if n <= 0 { return nil }
    body.append(contentsOf: buffer[0..<n])
  }
  let target = String(requestLine[1])
  let path = target.split(separator: "?", maxSplits: 1).first.map(String.init) ?? target
  return HTTPRequest(method: String(requestLine[0]), path: path, body: body.prefix(contentLength))
}

private func writeHTTPResponse(_ fd: Int32, _ response: HTTPResponse) -> Bool {
  var head = "HTTP/1.1 \(response.status) \(reasons[response.status] ?? "Status")\r\n"
  if response.status != 204 {
    head += "Content-Type: \(response.contentType)\r\nContent-Length: \(response.body.count)\r\n"
  }
  head += "Connection: close\r\n\r\n"
  return writeAll(fd, Data(head.utf8) + response.body)
}
