import CryptoKit
import Foundation

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
