import AVFoundation
import GoChatUI
import ReplayKit
import UIKit
import WebRTC

/// Voice channels on iOS: Google's WebRTC, as stasel/WebRTC builds it, behind
/// the protocols the shared Kotlin code talks to. One connection at a time
/// sends the microphone, the front camera and the screen the broadcast
/// extension captures, and plays the others, on the loudspeaker unless a
/// headset is connected.
final class VoiceRtc: NSObject, IosRtcEngine {
    let video = true

    lazy var factory: RTCPeerConnectionFactory = {
        RTCInitializeSSL()
        // WebRTC sets up the audio session when a call starts: as voice chat
        // apps do, on the loudspeaker, or a Bluetooth or wired headset.
        let audio = RTCAudioSessionConfiguration.webRTC()
        audio.categoryOptions = [.defaultToSpeaker, .allowBluetooth]
        RTCAudioSessionConfiguration.setWebRTC(audio)
        // The phone's own H.264 and VP8 in software, which the voice server takes.
        return RTCPeerConnectionFactory(encoderFactory: RTCDefaultVideoEncoderFactory(), decoderFactory: RTCDefaultVideoDecoderFactory())
    }()

    /// The connection open now, which the screen's broadcast goes to.
    private weak var current: VoicePeer?
    private var screenEvents: IosScreenEvents?

    private var camera: (capturer: RTCCameraVideoCapturer, track: RTCVideoTrack)?
    private var startingCamera: [(RTCVideoTrack?) -> Void] = []

    // The screen, as the broadcast extension sends it.
    private lazy var screenSource = factory.videoSource(forScreenCast: true)
    private lazy var screenTrack = factory.videoTrack(with: screenSource, trackId: "screen")
    private lazy var broadcast: ScreenBroadcast = {
        let b = ScreenBroadcast(source: screenSource)
        b.onStart = { [weak self] in self?.broadcastStarted() }
        b.onEnd = { [weak self] in self?.broadcastEnded() }
        return b
    }()
    private var broadcasting = false
    private var picker: RPSystemBroadcastPickerView?

    func connect(iceServers: [IosIceServer], events: IosRtcEvents) -> IosRtcPeer {
        let config = RTCConfiguration()
        config.iceServers = iceServers.map { RTCIceServer(urlStrings: $0.urls, username: $0.username, credential: $0.credential) }
        config.sdpSemantics = .unifiedPlan
        config.bundlePolicy = .maxBundle
        config.rtcpMuxPolicy = .require
        // A phone that changes networks offers its new addresses, and the
        // voice may carry on over them.
        config.continualGatheringPolicy = .gatherContinually
        broadcast.listen()
        let peer = VoicePeer(engine: self, config: config, events: events)
        current = peer
        return peer
    }

    // The camera

    /// Starts the front camera, or the first there is, at the format nearest
    /// 1280x720; nil when there is none, as on a simulator.
    func startCamera(_ done: @escaping (RTCVideoTrack?) -> Void) {
        if let camera = camera {
            done(camera.track)
            return
        }
        startingCamera.append(done)
        guard startingCamera.count == 1 else { return }
        let finish: (RTCVideoTrack?) -> Void = { [weak self] track in
            guard let self = self else { return }
            let waiting = self.startingCamera
            self.startingCamera = []
            waiting.forEach { $0(track) }
        }
        let devices = RTCCameraVideoCapturer.captureDevices()
        guard let device = devices.first(where: { $0.position == .front }) ?? devices.first else {
            finish(nil)
            return
        }
        let formats = RTCCameraVideoCapturer.supportedFormats(for: device)
        let wanted: Int32 = 1280 * 720
        guard let format = formats.min(by: { abs(Self.pixels($0) - wanted) < abs(Self.pixels($1) - wanted) }) else {
            finish(nil)
            return
        }
        let fps = min(24, Int(format.videoSupportedFrameRateRanges.map(\.maxFrameRate).max() ?? 24))
        let source = factory.videoSource()
        let capturer = RTCCameraVideoCapturer(delegate: source)
        capturer.startCapture(with: device, format: format, fps: fps) { [weak self] error in
            DispatchQueue.main.async {
                guard let self = self, error == nil else {
                    finish(nil)
                    return
                }
                let track = self.factory.videoTrack(with: source, trackId: "camera")
                self.camera = (capturer, track)
                finish(track)
            }
        }
    }

    func stopCamera() {
        camera?.capturer.stopCapture()
        camera = nil
    }

    private static func pixels(_ format: AVCaptureDevice.Format) -> Int32 {
        let d = CMVideoFormatDescriptionGetDimensions(format.formatDescription)
        return d.width * d.height
    }

    // The screen

    /// The screen's track while a broadcast sends it.
    var screen: RTCVideoTrack? { broadcasting ? screenTrack : nil }

    /// Ends the broadcast, from the app.
    func stopScreen() {
        if broadcasting { broadcast.stop() }
    }

    /// Forgets a connection that closed, so a broadcast started after it
    /// has no call to go to.
    fileprivate func closed(_ peer: VoicePeer) {
        if current === peer { current = nil }
    }

    private func broadcastStarted() {
        // A broadcast outside a call has nowhere to go.
        guard current != nil else {
            broadcast.stop()
            return
        }
        broadcasting = true
        screenEvents?.started()
    }

    private func broadcastEnded() {
        broadcasting = false
        current?.screenEnded()
    }

    func pickBroadcast() {
        // The system's picker, opened as if its own button were tapped,
        // with the app's extension chosen.
        picker?.removeFromSuperview()
        let picker = RPSystemBroadcastPickerView(frame: CGRect(x: 0, y: 0, width: 1, height: 1))
        picker.preferredExtension = Bundle.main.object(forInfoDictionaryKey: "GoChatBroadcastExtension") as? String
        picker.showsMicrophoneButton = false
        picker.alpha = 0.01
        let window = UIApplication.shared.connectedScenes.compactMap { ($0 as? UIWindowScene)?.windows.first { $0.isKeyWindow } }.first
        window?.addSubview(picker)
        self.picker = picker
        for case let button as UIButton in picker.subviews {
            button.sendActions(for: .allTouchEvents)
        }
    }

    func setScreenEvents(events: IosScreenEvents?) {
        screenEvents = events
    }

    func stopVideo() {
        stopCamera()
        stopScreen()
    }

    // Drawing

    func videoView(track: Any, mirror: Bool, fill: Bool) -> UIView {
        let view = RTCMTLVideoView(frame: .zero)
        view.videoContentMode = fill ? .scaleAspectFill : .scaleAspectFit
        view.clipsToBounds = true
        view.isUserInteractionEnabled = false
        // The person's own camera shows as a mirror does.
        if mirror { view.transform = CGAffineTransform(scaleX: -1, y: 1) }
        (track as? RTCVideoTrack)?.add(view)
        return view
    }

    func releaseVideoView(view: UIView, track: Any) {
        if let renderer = view as? RTCVideoRenderer {
            (track as? RTCVideoTrack)?.remove(renderer)
        }
    }
}

private final class VoicePeer: NSObject, IosRtcPeer, RTCPeerConnectionDelegate {
    private unowned let engine: VoiceRtc
    private let events: IosRtcEvents
    private var pc: RTCPeerConnection?
    private var mic: RTCAudioTrack?
    private var meter: Timer?
    private var deafened = false
    // The transceivers the camera and the screen go out on, and the
    // streams of the others' cameras and screens by the receiver they come
    // on: WebRTC calls back on its own thread.
    private let lock = NSLock()
    private var slots: [String: RTCRtpTransceiver] = [:]
    private var videos: [String: String] = [:]

    init(engine: VoiceRtc, config: RTCConfiguration, events: IosRtcEvents) {
        self.engine = engine
        self.events = events
        super.init()
        let factory = engine.factory
        let none = RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil)
        guard let pc = factory.peerConnection(with: config, constraints: none, delegate: self) else {
            events.state(state: "failed")
            return
        }
        // WebRTC's echo cancellation, noise suppression and gain control
        // are on, as getUserMedia's are on the web.
        let track = factory.audioTrack(with: factory.audioSource(with: none), trackId: "mic")
        pc.add(track, streamIds: ["mic"])
        self.pc = pc
        mic = track
        // How loud the microphone is, from WebRTC's statistics, as often as
        // the web measures it.
        meter = Timer.scheduledTimer(withTimeInterval: 0.06, repeats: true) { [weak self] _ in self?.measure() }
    }

    func answer(offer: String, cameraMid: String?, screenMid: String?, reply: IosRtcAnswer) {
        guard let pc = pc else {
            reply.failed(reason: "The connection is closed.")
            return
        }
        pc.setRemoteDescription(RTCSessionDescription(type: .offer, sdp: offer)) { [weak self] error in
            if let error = error {
                reply.failed(reason: error.localizedDescription)
                return
            }
            self?.useSlots(camera: cameraMid, screen: screenMid)
            // An offer may bring in another voice, which deafening silences too.
            self?.applyDeafened()
            pc.answer(for: RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil)) { sdp, error in
                guard let sdp = sdp else {
                    reply.failed(reason: error?.localizedDescription ?? "WebRTC made no answer.")
                    return
                }
                pc.setLocalDescription(sdp) { error in
                    if let error = error {
                        reply.failed(reason: error.localizedDescription)
                    } else {
                        self?.limit("camera")
                        self?.limit("screen")
                        reply.answered(sdp: sdp.sdp)
                    }
                }
            }
        }
    }

    /// Answers the camera's and the screen's transceivers sendonly from the
    /// first offer on, so either goes out without another.
    private func useSlots(camera: String?, screen: String?) {
        guard let pc = pc else { return }
        lock.lock()
        defer { lock.unlock() }
        guard slots.isEmpty else { return }
        for (source, mid) in [("camera", camera), ("screen", screen)] {
            guard let mid = mid, let t = pc.transceivers.first(where: { $0.mid == mid }) else { continue }
            var error: NSError?
            t.setDirection(.sendOnly, error: &error)
            slots[source] = t
        }
    }

    private func slot(_ source: String) -> RTCRtpTransceiver? {
        lock.lock()
        defer { lock.unlock() }
        return slots[source]
    }

    func addCandidate(sdp: String, sdpMid: String?, sdpMLineIndex: Int32) {
        pc?.add(RTCIceCandidate(sdp: sdp, sdpMLineIndex: max(sdpMLineIndex, 0), sdpMid: sdpMid)) { _ in }
    }

    func setMuted(muted: Bool) {
        mic?.isEnabled = !muted
    }

    func setDeafened(deafened: Bool) {
        self.deafened = deafened
        applyDeafened()
    }

    func setCamera(on: Bool, reply: IosRtcCamera) {
        guard on else {
            _ = send("camera", nil)
            engine.stopCamera()
            reply.started(track: nil)
            return
        }
        engine.startCamera { [weak self] track in
            guard let self = self, let track = track, self.send("camera", track) else {
                if track != nil { self?.engine.stopCamera() }
                reply.started(track: nil)
                return
            }
            reply.started(track: track)
        }
    }

    func setScreen(on: Bool) -> Any? {
        guard on else {
            _ = send("screen", nil)
            engine.stopScreen()
            return nil
        }
        guard let track = engine.screen, send("screen", track) else { return nil }
        return track
    }

    /// Puts a track on its slot, or takes it off; false when there is no slot.
    private func send(_ source: String, _ track: RTCVideoTrack?) -> Bool {
        guard pc != nil, let t = slot(source) else { return false }
        t.sender.track = track
        if track != nil { limit(source) }
        return true
    }

    /// Caps what a source sends, as the web does; a screen keeps its
    /// sharpness and gives up frames.
    private func limit(_ source: String) {
        guard let sender = slot(source)?.sender else { return }
        let params = sender.parameters
        guard let encoding = params.encodings.first else { return }
        encoding.maxBitrateBps = NSNumber(value: source == "camera" ? 1_000_000 : 2_500_000)
        if source == "screen" {
            params.degradationPreference = NSNumber(value: RTCDegradationPreference.maintainResolution.rawValue)
        }
        sender.parameters = params
    }

    /// The broadcast ended, wherever the person ended it.
    func screenEnded() {
        events.screenEnded()
    }

    func close() {
        meter?.invalidate()
        meter = nil
        // The engine keeps the camera and the screen for a call that reconnects.
        lock.lock()
        slots.values.forEach { $0.sender.track = nil }
        slots = [:]
        lock.unlock()
        pc?.close()
        pc = nil
        mic = nil
        engine.closed(self)
    }

    /// A voice that comes in plays while its track is enabled.
    private func applyDeafened() {
        let on = !deafened
        pc?.transceivers.compactMap { $0.receiver.track as? RTCAudioTrack }.forEach { $0.isEnabled = on }
    }

    private func measure() {
        pc?.statistics { [weak self] report in
            var level = 0.0
            for stats in report.statistics.values where stats.type == "media-source" {
                if let value = stats.values["audioLevel"] as? NSNumber {
                    level = max(level, value.doubleValue)
                }
            }
            self?.events.level(level: Float(level))
        }
    }

    private func report(_ state: String) {
        events.state(state: state)
    }

    // RTCPeerConnectionDelegate

    func peerConnection(_ peerConnection: RTCPeerConnection, didChange stateChanged: RTCSignalingState) {}

    // The voices that come in play by themselves; cameras and screens come as receivers.
    func peerConnection(_ peerConnection: RTCPeerConnection, didAdd stream: RTCMediaStream) {}

    func peerConnection(_ peerConnection: RTCPeerConnection, didRemove stream: RTCMediaStream) {}

    func peerConnection(_ peerConnection: RTCPeerConnection, didAdd rtpReceiver: RTCRtpReceiver, streams mediaStreams: [RTCMediaStream]) {
        guard let track = rtpReceiver.track as? RTCVideoTrack, let stream = mediaStreams.first?.streamId else { return }
        lock.lock()
        videos[rtpReceiver.receiverId] = stream
        lock.unlock()
        events.video(streamId: stream, track: track)
    }

    func peerConnection(_ peerConnection: RTCPeerConnection, didRemove rtpReceiver: RTCRtpReceiver) {
        lock.lock()
        let stream = videos.removeValue(forKey: rtpReceiver.receiverId)
        lock.unlock()
        if let stream = stream {
            events.video(streamId: stream, track: nil)
        }
    }

    func peerConnectionShouldNegotiate(_ peerConnection: RTCPeerConnection) {}

    func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCIceConnectionState) {
        // The connection's own state says more; this one stands in where
        // it is late.
        switch newState {
        case .connected, .completed: report("connected")
        case .failed: report("failed")
        default: break
        }
    }

    func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCIceGatheringState) {}

    func peerConnection(_ peerConnection: RTCPeerConnection, didGenerate candidate: RTCIceCandidate) {
        events.candidate(sdp: candidate.sdp, sdpMid: candidate.sdpMid, sdpMLineIndex: candidate.sdpMLineIndex)
    }

    func peerConnection(_ peerConnection: RTCPeerConnection, didRemove candidates: [RTCIceCandidate]) {}

    func peerConnection(_ peerConnection: RTCPeerConnection, didOpen dataChannel: RTCDataChannel) {}

    func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCPeerConnectionState) {
        switch newState {
        case .new, .connecting: report("connecting")
        case .connected: report("connected")
        case .disconnected: report("disconnected")
        case .failed: report("failed")
        case .closed: break
        @unknown default: break
        }
    }
}
