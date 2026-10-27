import CoreGraphics
import Darwin
import Foundation

/// How the screen's broadcast reaches the app, in both the app and its
/// broadcast extension. The app listens on a Unix socket in the container
/// of the app group they share; the extension connects to it once the
/// person starts the broadcast and writes each frame as a JPEG, after its
/// length as four big-endian bytes. Either side closing the socket ends the
/// broadcast: the app closes it to stop sharing. Only the app's group can
/// reach the socket, unlike a Darwin notification, which any app may post.
enum Broadcast {
    /// The app group both belong to, as their Info.plist names it.
    static var group: String? {
        Bundle.main.object(forInfoDictionaryKey: "GoChatAppGroup") as? String
    }

    /// Where the app listens.
    static var socketPath: String? {
        guard let group = group else { return nil }
        return FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: group)?.appendingPathComponent("screen.sock").path
    }

    /// The longest side of a frame the extension sends, as the apps cap a screen.
    static let longSide: CGFloat = 1280

    /// The largest frame either side takes.
    static let maxFrame = 8 << 20

    /// The socket's address, or nil when the path is too long for one.
    static func address(_ path: String) -> sockaddr_un? {
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let capacity = MemoryLayout.size(ofValue: addr.sun_path)
        guard path.utf8.count < capacity else { return nil }
        withUnsafeMutableBytes(of: &addr.sun_path) { buf in
            path.withCString { _ = strncpy(buf.baseAddress!.assumingMemoryBound(to: CChar.self), $0, capacity - 1) }
        }
        return addr
    }

    /// Writes all of [data], or says it could not.
    static func write(_ fd: Int32, _ data: Data) -> Bool {
        data.withUnsafeBytes { raw -> Bool in
            var sent = 0
            while sent < raw.count {
                let n = Darwin.write(fd, raw.baseAddress!.advanced(by: sent), raw.count - sent)
                if n < 0 && errno == EINTR { continue }
                if n <= 0 { return false }
                sent += n
            }
            return true
        }
    }

    /// Reads exactly [count] bytes, or nil once the other side is gone.
    static func read(_ fd: Int32, _ count: Int) -> Data? {
        var data = Data(count: count)
        let ok = data.withUnsafeMutableBytes { raw -> Bool in
            var got = 0
            while got < count {
                let n = Darwin.read(fd, raw.baseAddress!.advanced(by: got), count - got)
                if n < 0 && errno == EINTR { continue }
                if n <= 0 { return false }
                got += n
            }
            return true
        }
        return ok ? data : nil
    }

    /// A frame's length, before it.
    static func header(_ length: Int) -> Data {
        var be = UInt32(length).bigEndian
        return Data(bytes: &be, count: 4)
    }

    static func length(_ header: Data) -> Int {
        Int(header.reduce(UInt32(0)) { $0 << 8 | UInt32($1) })
    }
}
