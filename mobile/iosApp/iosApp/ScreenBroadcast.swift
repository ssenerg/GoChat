import CoreImage
import CoreVideo
import Darwin
import Foundation
import QuartzCore
import WebRTC

/// The screen, as the broadcast extension sends it: the app listens on the
/// Unix socket Broadcast names, takes one broadcast at a time, and hands
/// each frame it gets, a JPEG, to WebRTC as the screen's track.
final class ScreenBroadcast {
    /// Called on the main thread as a broadcast starts and ends.
    var onStart: (() -> Void)?
    var onEnd: (() -> Void)?

    private let source: RTCVideoSource
    private let capturer: RTCVideoCapturer
    private let queue = DispatchQueue(label: "io.github.ssenerg.gochat.screen")
    private let context = CIContext()
    private var server: Int32 = -1
    // The extension's connection while a broadcast goes on.
    private let lock = NSLock()
    private var client: Int32 = -1
    private var pool: CVPixelBufferPool?
    private var poolSize = CGSize.zero

    init(source: RTCVideoSource) {
        self.source = source
        capturer = RTCVideoCapturer(delegate: source)
    }

    /// Starts listening, once: a socket path too long for the system, as a
    /// simulator's is, leaves the screen unshared.
    func listen() {
        guard server < 0, let path = Broadcast.socketPath, var addr = Broadcast.address(path) else { return }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return }
        unlink(path)
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard bound == 0, Darwin.listen(fd, 1) == 0 else {
            close(fd)
            return
        }
        server = fd
        queue.async { [weak self] in self?.serve() }
    }

    /// Ends the broadcast by closing the extension's connection, which it
    /// hears; onEnd follows.
    func stop() {
        lock.lock()
        defer { lock.unlock() }
        if client >= 0 { shutdown(client, SHUT_RDWR) }
    }

    /// Takes one broadcast after the other, for as long as the app runs.
    private func serve() {
        while true {
            let fd = accept(server, nil, nil)
            if fd < 0 {
                if errno == EINTR { continue }
                return
            }
            var on: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
            lock.lock()
            client = fd
            lock.unlock()
            DispatchQueue.main.async { self.onStart?() }
            while let header = Broadcast.read(fd, 4) {
                let length = Broadcast.length(header)
                guard length > 0, length <= Broadcast.maxFrame, let jpeg = Broadcast.read(fd, length) else { break }
                deliver(jpeg)
            }
            lock.lock()
            client = -1
            lock.unlock()
            close(fd)
            DispatchQueue.main.async { self.onEnd?() }
        }
    }

    private func deliver(_ jpeg: Data) {
        guard let image = CIImage(data: jpeg) else { return }
        let size = image.extent.size
        if pool == nil || poolSize != size {
            let attributes: [String: Any] = [
                kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA,
                kCVPixelBufferWidthKey as String: Int(size.width),
                kCVPixelBufferHeightKey as String: Int(size.height),
                kCVPixelBufferIOSurfacePropertiesKey as String: [String: Any](),
            ]
            pool = nil
            CVPixelBufferPoolCreate(nil, nil, attributes as CFDictionary, &pool)
            poolSize = size
        }
        guard let pool = pool else { return }
        var buffer: CVPixelBuffer?
        CVPixelBufferPoolCreatePixelBuffer(nil, pool, &buffer)
        guard let pixels = buffer else { return }
        context.render(image.transformed(by: CGAffineTransform(translationX: -image.extent.minX, y: -image.extent.minY)), to: pixels)
        let frame = RTCVideoFrame(
            buffer: RTCCVPixelBuffer(pixelBuffer: pixels),
            rotation: ._0,
            timeStampNs: Int64(CACurrentMediaTime() * 1_000_000_000)
        )
        source.capturer(capturer, didCapture: frame)
    }
}
