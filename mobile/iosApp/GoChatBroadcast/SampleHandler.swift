import CoreImage
import Darwin
import ReplayKit

/// Shares the screen in a GoChat call: ReplayKit hands this extension the
/// screen, and it writes each frame, upright, scaled and compressed, to the
/// app over the socket the app listens on in their shared container. A
/// broadcast extension may use 50 MB at most, so it keeps one frame in
/// hand and drops the others.
final class SampleHandler: RPBroadcastSampleHandler {
    private var fd: Int32 = -1
    private let context = CIContext()
    private let queue = DispatchQueue(label: "io.github.ssenerg.gochat.broadcast")
    private let lock = NSLock()
    private var busy = false
    private var last: CFTimeInterval = 0

    override func broadcastStarted(withSetupInfo setupInfo: [String: NSObject]?) {
        guard let path = Broadcast.socketPath, connect(path) else {
            finish("Join a voice channel in GoChat to share your screen.")
            return
        }
    }

    override func broadcastFinished() {
        disconnect()
    }

    override func processSampleBuffer(_ sampleBuffer: CMSampleBuffer, with sampleBufferType: RPSampleBufferType) {
        guard sampleBufferType == .video, let pixels = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }
        let now = CACurrentMediaTime()
        lock.lock()
        // At most 15 frames a second, one at a time: a busy app gets fewer.
        guard fd >= 0, !busy, now - last >= 1.0 / 15 else {
            lock.unlock()
            return
        }
        busy = true
        last = now
        lock.unlock()
        let raw = (CMGetAttachment(sampleBuffer, key: RPVideoSampleOrientationKey as CFString, attachmentModeOut: nil) as? NSNumber)?.uint32Value
        var image = CIImage(cvPixelBuffer: pixels).oriented(raw.flatMap(CGImagePropertyOrientation.init(rawValue:)) ?? .up)
        let scale = min(1, Broadcast.longSide / max(image.extent.width, image.extent.height))
        if scale < 1 {
            image = image.transformed(by: CGAffineTransform(scaleX: scale, y: scale))
        }
        queue.async { [weak self] in
            guard let self = self else { return }
            defer {
                self.lock.lock()
                self.busy = false
                self.lock.unlock()
            }
            let options = [kCGImageDestinationLossyCompressionQuality as CIImageRepresentationOption: 0.6]
            guard let jpeg = self.context.jpegRepresentation(of: image, colorSpace: CGColorSpaceCreateDeviceRGB(), options: options) else { return }
            self.lock.lock()
            let fd = self.fd
            self.lock.unlock()
            if fd >= 0, !(Broadcast.write(fd, Broadcast.header(jpeg.count)) && Broadcast.write(fd, jpeg)) {
                self.finish("GoChat stopped taking your screen.")
            }
        }
    }

    private func connect(_ path: String) -> Bool {
        guard var addr = Broadcast.address(path) else { return false }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        var on: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        let connected = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard connected == 0 else {
            close(fd)
            return false
        }
        lock.lock()
        self.fd = fd
        lock.unlock()
        watch(fd)
        return true
    }

    /// Waits for the app to close its side, as it does when the person
    /// stops sharing there, or when it goes.
    private func watch(_ fd: Int32) {
        Thread.detachNewThread { [weak self] in
            var byte: UInt8 = 0
            while true {
                let n = Darwin.read(fd, &byte, 1)
                if n > 0 || (n < 0 && errno == EINTR) { continue }
                break
            }
            guard let self = self else { return }
            self.lock.lock()
            let open = self.fd == fd
            self.lock.unlock()
            // The extension closing it itself says nothing more.
            if open { self.finish("GoChat stopped sharing your screen.") }
        }
    }

    private func disconnect() {
        lock.lock()
        let fd = self.fd
        self.fd = -1
        lock.unlock()
        if fd >= 0 {
            // Wakes the watch, which then sees the extension closed it.
            shutdown(fd, SHUT_RDWR)
            close(fd)
        }
    }

    /// Ends the broadcast, and says why on the system's notice.
    private func finish(_ reason: String) {
        disconnect()
        finishBroadcastWithError(NSError(domain: "io.github.ssenerg.gochat.broadcast", code: 1, userInfo: [NSLocalizedFailureReasonErrorKey: reason]))
    }
}
