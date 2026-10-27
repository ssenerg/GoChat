package io.github.ssenerg.gochat.ui

import android.content.Context
import android.content.Intent
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.media.projection.MediaProjection
import android.os.Build
import android.os.Handler
import android.os.Looper
import io.github.ssenerg.gochat.core.voice.IceCandidate
import io.github.ssenerg.gochat.core.voice.IceServer
import io.github.ssenerg.gochat.core.voice.RtcEngine
import io.github.ssenerg.gochat.core.voice.RtcListener
import io.github.ssenerg.gochat.core.voice.RtcPeer
import io.github.ssenerg.gochat.core.voice.RtcState
import io.github.ssenerg.gochat.core.voice.VideoSlots
import io.github.ssenerg.gochat.core.voice.VideoSource
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import org.webrtc.AudioSource
import org.webrtc.AudioTrack
import org.webrtc.Camera2Enumerator
import org.webrtc.CameraVideoCapturer
import org.webrtc.DataChannel
import org.webrtc.DefaultVideoDecoderFactory
import org.webrtc.DefaultVideoEncoderFactory
import org.webrtc.EglBase
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RtpParameters
import org.webrtc.RtpReceiver
import org.webrtc.RtpTransceiver
import org.webrtc.ScreenCapturerAndroid
import org.webrtc.SdpObserver
import org.webrtc.SessionDescription
import org.webrtc.SurfaceTextureHelper
import org.webrtc.VideoCapturer
import org.webrtc.VideoTrack
import org.webrtc.audio.JavaAudioDeviceModule
import java.util.concurrent.CompletableFuture
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException
import kotlin.math.sqrt

/**
 * Voice channels on Android, over the WebRTC library Stream builds from
 * Google's: one connection at a time, which sends the microphone, the
 * front camera and the screen, plays the others on the loudspeaker or a
 * headset, and hears how loud the person is.
 */
class AndroidRtcEngine(context: Context) : RtcEngine {
    private val context = context.applicationContext
    private var factory: PeerConnectionFactory? = null
    private var audio: JavaAudioDeviceModule? = null

    override val video = true

    /** The GL context the cameras, the screen, the codecs and the views that draw them share. */
    val egl: EglBase by lazy { EglBase.create() }

    /** Whom the microphone's loudness and the screen's end go to: the connection open now. */
    @Volatile
    private var events: RtcListener? = null

    private var camera: Capture? = null
    private var screen: Capture? = null

    /** Counts the camera's stops: a start one overtook lets go of what it opened. */
    private var cameraStops = 0

    /** Set once the system stopped the screen's capture, with or without a connection to hear it. */
    @Volatile
    private var screenEnded = false

    /** The person's consent to capture the screen, which serves one capture. */
    @Volatile
    private var screenGrant: Intent? = null

    @Synchronized
    private fun factory(): PeerConnectionFactory {
        factory?.let { return it }
        PeerConnectionFactory.initialize(PeerConnectionFactory.InitializationOptions.builder(context).createInitializationOptions())
        val meter = LevelMeter { level -> events?.onLevel(level) }
        val module = JavaAudioDeviceModule.builder(context)
            .setUseHardwareAcousticEchoCanceler(JavaAudioDeviceModule.isBuiltInAcousticEchoCancelerSupported())
            .setUseHardwareNoiseSuppressor(JavaAudioDeviceModule.isBuiltInNoiseSuppressorSupported())
            .setSamplesReadyCallback { samples -> meter.add(samples.data) }
            .createAudioDeviceModule()
        val f = PeerConnectionFactory.builder()
            .setAudioDeviceModule(module)
            // The phone's own encoders and decoders where it has them, libvpx where not.
            .setVideoEncoderFactory(DefaultVideoEncoderFactory(egl.eglBaseContext, true, false))
            .setVideoDecoderFactory(DefaultVideoDecoderFactory(egl.eglBaseContext))
            .createPeerConnectionFactory()
        audio = module
        factory = f
        return f
    }

    /** Takes the person's consent to capture the screen, from MediaProjection's dialog. */
    fun grantScreen(data: Intent) {
        screenGrant = data
    }

    /**
     * Starts the front camera, or the first there is, and waits for its
     * first frame; null when the phone has none or it would not open, as
     * when another app has it.
     */
    internal fun startCamera(): VideoTrack? {
        val stops = synchronized(this) {
            camera?.let { return it.track }
            cameraStops
        }
        val enumerator = Camera2Enumerator(context)
        val names = enumerator.deviceNames
        val name = names.firstOrNull(enumerator::isFrontFacing) ?: names.firstOrNull() ?: return null
        val opened = CompletableFuture<Boolean>()
        val capturer = enumerator.createCapturer(name, CameraEvents(opened)) ?: return null
        val started = capture("camera", capturer, screencast = false, CAMERA_WIDTH, CAMERA_HEIGHT, CAMERA_FPS) ?: return null
        // The camera opens on its own thread, and says only there whether it
        // could; the engine is not held meanwhile, so a call may end.
        val ok = runCatching { opened.get(CAMERA_OPEN_SECONDS, TimeUnit.SECONDS) }.getOrDefault(false) == true
        val kept = synchronized(this) {
            when {
                !ok || cameraStops != stops -> null
                camera != null -> camera
                else -> started.also { camera = it }
            }
        }
        if (kept !== started) started.dispose()
        return kept?.track
    }

    /** Hears the camera open, or fail then or later. */
    private inner class CameraEvents(private val opened: CompletableFuture<Boolean>) : CameraVideoCapturer.CameraEventsHandler {
        override fun onFirstFrameAvailable() {
            opened.complete(true)
        }

        override fun onCameraError(error: String) = failed()

        override fun onCameraDisconnected() = failed()

        // A camera that worked and stopped ends the call's camera.
        private fun failed() {
            if (!opened.complete(false)) events?.onCameraEnded()
        }

        override fun onCameraFreezed(error: String) {}

        override fun onCameraOpening(cameraName: String) {}

        override fun onCameraClosed() {}
    }

    internal fun stopCamera() {
        val stopped = synchronized(this) {
            cameraStops++
            camera.also { camera = null }
        }
        stopped?.dispose()
    }

    /**
     * Starts capturing the screen, with the consent taken last; null without
     * one, or when Android refuses, as it does unless the call's service
     * runs for the screen first.
     */
    @Synchronized
    internal fun startScreen(): VideoTrack? {
        if (!screenEnded) screen?.let { return it.track }
        stopScreen()
        val grant = screenGrant ?: return null
        screenGrant = null
        screenEnded = false
        val capturer = ScreenCapturerAndroid(
            grant,
            object : MediaProjection.Callback() {
                // The person stopped it from the system, or the system took
                // it back: the call stops sending it, and so stops it here.
                // Between connections, the next one hears of it.
                override fun onStop() {
                    screenEnded = true
                    events?.onScreenEnded()
                }
            },
        )
        // The screen as it is, at most 1280 pixels on its long side.
        val metrics = context.resources.displayMetrics
        val scale = minOf(1f, SCREEN_LONG_SIDE.toFloat() / maxOf(metrics.widthPixels, metrics.heightPixels))
        val width = (metrics.widthPixels * scale).toInt() and 1.inv()
        val height = (metrics.heightPixels * scale).toInt() and 1.inv()
        screen = capture("screen", capturer, screencast = true, width, height, SCREEN_FPS)
        return screen?.track
    }

    @Synchronized
    internal fun stopScreen() {
        screen?.dispose()
        screen = null
    }

    /** Lets go of a screen the system stopped while no connection heard it; says whether there was one. */
    @Synchronized
    private fun dropEndedScreen(): Boolean {
        if (screen == null || !screenEnded) return false
        stopScreen()
        return true
    }

    override fun stopVideo() {
        stopCamera()
        stopScreen()
        screenGrant = null
    }

    private fun capture(name: String, capturer: VideoCapturer, screencast: Boolean, width: Int, height: Int, fps: Int): Capture? {
        val f = factory()
        val helper = SurfaceTextureHelper.create(name, egl.eglBaseContext) ?: return null
        val source = f.createVideoSource(screencast)
        return try {
            capturer.initialize(helper, context, source.capturerObserver)
            capturer.startCapture(width, height, fps)
            Capture(capturer, source, f.createVideoTrack(name, source), helper)
        } catch (_: Exception) {
            capturer.dispose()
            source.dispose()
            helper.dispose()
            null
        }
    }

    override fun connect(iceServers: List<IceServer>, listener: RtcListener): RtcPeer {
        val f = factory()
        val config = PeerConnection.RTCConfiguration(
            iceServers.map { s -> PeerConnection.IceServer.builder(s.urls).setUsername(s.username).setPassword(s.credential).createIceServer() },
        ).apply {
            sdpSemantics = PeerConnection.SdpSemantics.UNIFIED_PLAN
            bundlePolicy = PeerConnection.BundlePolicy.MAXBUNDLE
            rtcpMuxPolicy = PeerConnection.RtcpMuxPolicy.REQUIRE
            // A phone that changes networks offers its new addresses, and
            // the voice may carry on over them.
            continualGatheringPolicy = PeerConnection.ContinualGatheringPolicy.GATHER_CONTINUALLY
        }
        val pc = f.createPeerConnection(config, Observer(listener)) ?: throw IllegalStateException("WebRTC could not start")
        // WebRTC's own echo cancellation, noise suppression and gain
        // control are on by default, as getUserMedia's are on the web.
        val source = f.createAudioSource(MediaConstraints())
        val track = f.createAudioTrack("mic", source)
        pc.addTrack(track, listOf("mic"))
        val routing = AudioRouting(context).also { it.start() }
        events = listener
        if (dropEndedScreen()) listener.onScreenEnded()
        return AndroidRtcPeer(this, pc, source, track, routing, audio) {
            if (events === listener) events = null
        }
    }

    private companion object {
        const val CAMERA_WIDTH = 1280
        const val CAMERA_HEIGHT = 720
        const val CAMERA_FPS = 24
        const val CAMERA_OPEN_SECONDS = 12L
        const val SCREEN_LONG_SIDE = 1280
        const val SCREEN_FPS = 15
    }
}

/** A camera or the screen while it is captured, which connections send. */
private class Capture(val capturer: VideoCapturer, val source: org.webrtc.VideoSource, val track: VideoTrack, val helper: SurfaceTextureHelper) {
    fun dispose() {
        runCatching { capturer.stopCapture() }
        capturer.dispose()
        track.dispose()
        source.dispose()
        helper.dispose()
    }
}

private class AndroidRtcPeer(
    private val engine: AndroidRtcEngine,
    private val pc: PeerConnection,
    private val source: AudioSource,
    private val track: AudioTrack,
    private val routing: AudioRouting,
    private val audio: JavaAudioDeviceModule?,
    private val onClose: () -> Unit,
) : RtcPeer {
    private val closed = AtomicBoolean(false)

    /** The transceivers the camera and the screen go out on. */
    @Volatile
    private var slots: Map<VideoSource, RtpTransceiver> = emptyMap()

    override suspend fun answer(offer: String, slots: VideoSlots?): String {
        pc.awaitSet { pc.setRemoteDescription(it, SessionDescription(SessionDescription.Type.OFFER, offer)) }
        if (slots != null && this.slots.isEmpty()) {
            // Answered sendonly from the first offer on, so the camera and
            // the screen go out without another.
            val byMid = pc.transceivers.associateBy { it.mid }
            this.slots = buildMap {
                byMid[slots.camera]?.let { put(VideoSource.Camera, it) }
                byMid[slots.screen]?.let { put(VideoSource.Screen, it) }
            }
            this.slots.values.forEach { it.setDirection(RtpTransceiver.RtpTransceiverDirection.SEND_ONLY) }
        }
        val answer = suspendCancellableCoroutine { c ->
            pc.createAnswer(
                object : Sdp() {
                    override fun onCreateSuccess(sdp: SessionDescription) = c.resume(sdp)

                    override fun onCreateFailure(error: String?) = c.resumeWithException(IllegalStateException(error ?: "no answer"))
                },
                MediaConstraints(),
            )
        }
        pc.awaitSet { pc.setLocalDescription(it, answer) }
        this.slots.keys.forEach(::limit)
        return answer.description
    }

    override suspend fun setCamera(on: Boolean): Any? {
        if (!on) {
            send(VideoSource.Camera, null)
            engine.stopCamera()
            return null
        }
        val camera = withContext(Dispatchers.Default) { engine.startCamera() } ?: return null
        if (send(VideoSource.Camera, camera)) return camera
        engine.stopCamera()
        return null
    }

    override suspend fun setScreen(on: Boolean): Any? {
        if (!on) {
            send(VideoSource.Screen, null)
            engine.stopScreen()
            return null
        }
        val screen = withContext(Dispatchers.Default) { engine.startScreen() } ?: return null
        if (send(VideoSource.Screen, screen)) return screen
        engine.stopScreen()
        return null
    }

    /** Puts a track on its slot, or takes it off; false when there is no slot. */
    private fun send(source: VideoSource, track: VideoTrack?): Boolean {
        if (closed.get()) return false
        val slot = slots[source] ?: return false
        // The engine owns the track: a connection that closes leaves it.
        if (!slot.sender.setTrack(track, false)) return false
        if (track != null) limit(source)
        return true
    }

    /** Caps what a source sends, as the web does; a screen keeps its sharpness and gives up frames. */
    private fun limit(source: VideoSource) {
        val sender = slots[source]?.sender ?: return
        val params = sender.parameters
        if (params.encodings.isEmpty()) return
        params.encodings[0].maxBitrateBps = if (source == VideoSource.Camera) CAMERA_BITRATE else SCREEN_BITRATE
        if (source == VideoSource.Screen) params.degradationPreference = RtpParameters.DegradationPreference.MAINTAIN_RESOLUTION
        sender.parameters = params
    }

    override suspend fun addCandidate(candidate: IceCandidate) {
        if (closed.get()) return
        pc.addIceCandidate(org.webrtc.IceCandidate(candidate.sdpMid ?: "", candidate.sdpMLineIndex ?: 0, candidate.candidate))
    }

    override fun setMuted(muted: Boolean) {
        if (!closed.get()) track.setEnabled(!muted)
    }

    override fun setDeafened(deafened: Boolean) {
        // Silences the speaker, and so every voice that comes in.
        if (!closed.get()) audio?.setSpeakerMute(deafened)
    }

    override fun close() {
        if (!closed.compareAndSet(false, true)) return
        onClose()
        audio?.setSpeakerMute(false)
        slots.values.forEach { runCatching { it.sender.setTrack(null, false) } }
        pc.dispose()
        source.dispose()
        routing.stop()
    }

    private companion object {
        const val CAMERA_BITRATE = 1_000_000
        const val SCREEN_BITRATE = 2_500_000
    }
}

/** Runs a set call of the peer connection and waits for its outcome. */
private suspend fun PeerConnection.awaitSet(call: (SdpObserver) -> Unit) = suspendCancellableCoroutine { c ->
    call(
        object : Sdp() {
            override fun onSetSuccess() = c.resume(Unit)

            override fun onSetFailure(error: String?) = c.resumeWithException(IllegalStateException(error ?: "refused"))
        },
    )
}

private open class Sdp : SdpObserver {
    override fun onCreateSuccess(sdp: SessionDescription) {}

    override fun onSetSuccess() {}

    override fun onCreateFailure(error: String?) {}

    override fun onSetFailure(error: String?) {}
}

/** Hands WebRTC's events on to the call. */
private class Observer(private val listener: RtcListener) : PeerConnection.Observer {
    /** The streams of the others' cameras and screens, by the receiver they come on. */
    private val videos = ConcurrentHashMap<String, String>()

    override fun onAddTrack(receiver: RtpReceiver, streams: Array<out MediaStream>) {
        val track = receiver.track() as? VideoTrack ?: return
        val stream = streams.firstOrNull()?.id ?: return
        videos[receiver.id()] = stream
        listener.onVideo(stream, track)
    }

    override fun onRemoveTrack(receiver: RtpReceiver) {
        videos.remove(receiver.id())?.let { listener.onVideo(it, null) }
    }

    override fun onIceCandidate(candidate: org.webrtc.IceCandidate) =
        listener.onCandidate(IceCandidate(candidate.sdp, candidate.sdpMid, candidate.sdpMLineIndex))

    override fun onConnectionChange(newState: PeerConnection.PeerConnectionState) {
        when (newState) {
            PeerConnection.PeerConnectionState.NEW, PeerConnection.PeerConnectionState.CONNECTING -> listener.onState(RtcState.Connecting)
            PeerConnection.PeerConnectionState.CONNECTED -> listener.onState(RtcState.Connected)
            PeerConnection.PeerConnectionState.DISCONNECTED -> listener.onState(RtcState.Disconnected)
            PeerConnection.PeerConnectionState.FAILED -> listener.onState(RtcState.Failed)
            PeerConnection.PeerConnectionState.CLOSED -> {}
        }
    }

    override fun onSignalingChange(state: PeerConnection.SignalingState) {}

    override fun onIceConnectionChange(state: PeerConnection.IceConnectionState) {}

    override fun onIceConnectionReceivingChange(receiving: Boolean) {}

    override fun onIceGatheringChange(state: PeerConnection.IceGatheringState) {}

    override fun onIceCandidatesRemoved(candidates: Array<out org.webrtc.IceCandidate>) {}

    // The voices that come in play by themselves; cameras and screens come as tracks.
    override fun onAddStream(stream: MediaStream) {}

    override fun onRemoveStream(stream: MediaStream) {}

    override fun onDataChannel(channel: DataChannel) {}

    override fun onRenegotiationNeeded() {}
}

/**
 * The microphone's loudness: the root mean square of its 16-bit samples,
 * the loudest of each 60 ms, as often as the web measures it.
 */
private class LevelMeter(private val emit: (Float) -> Unit) {
    private var loudest = 0f
    private var chunks = 0

    fun add(pcm: ByteArray) {
        var sum = 0.0
        val n = pcm.size / 2
        if (n == 0) return
        for (i in 0 until n) {
            val s = ((pcm[2 * i + 1].toInt() shl 8) or (pcm[2 * i].toInt() and 0xff)).toShort() / 32768.0
            sum += s * s
        }
        loudest = maxOf(loudest, sqrt(sum / n).toFloat())
        // WebRTC hands over 10 ms at a time.
        if (++chunks >= 6) {
            emit(loudest)
            loudest = 0f
            chunks = 0
        }
    }
}

/**
 * Puts the phone in a call: voice communication mode and audio focus, on a
 * headset when one is connected and on the loudspeaker otherwise, as voice
 * chat apps do; a headset plugged in or out moves the voice.
 */
private class AudioRouting(context: Context) {
    private val am = context.getSystemService(AudioManager::class.java)
    private var focus: AudioFocusRequest? = null
    private var mode = AudioManager.MODE_NORMAL
    private var sco = false
    private val handler = Handler(Looper.getMainLooper())
    private val devices = object : AudioDeviceCallback() {
        override fun onAudioDevicesAdded(added: Array<out AudioDeviceInfo>) = route()

        override fun onAudioDevicesRemoved(removed: Array<out AudioDeviceInfo>) = route()
    }

    fun start() {
        mode = am.mode
        val attributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
            .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
            .build()
        focus = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN).setAudioAttributes(attributes).build().also { am.requestAudioFocus(it) }
        am.mode = AudioManager.MODE_IN_COMMUNICATION
        route()
        am.registerAudioDeviceCallback(devices, handler)
    }

    private fun route() {
        if (Build.VERSION.SDK_INT >= 31) {
            val available = am.availableCommunicationDevices
            val pick = available.firstOrNull { it.type in HEADSETS } ?: available.firstOrNull { it.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER }
            if (pick != null) am.setCommunicationDevice(pick)
        } else {
            val outputs = am.getDevices(AudioManager.GET_DEVICES_OUTPUTS)
            val bluetooth = outputs.any { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }
            val wired = outputs.any { it.type in HEADSETS && it.type != AudioDeviceInfo.TYPE_BLUETOOTH_SCO }
            @Suppress("DEPRECATION")
            run {
                if (bluetooth && !wired && !sco) {
                    am.startBluetoothSco()
                    am.isBluetoothScoOn = true
                    sco = true
                } else if (!bluetooth && sco) {
                    am.stopBluetoothSco()
                    am.isBluetoothScoOn = false
                    sco = false
                }
                am.isSpeakerphoneOn = !bluetooth && !wired
            }
        }
    }

    fun stop() {
        am.unregisterAudioDeviceCallback(devices)
        if (Build.VERSION.SDK_INT >= 31) {
            am.clearCommunicationDevice()
        } else {
            @Suppress("DEPRECATION")
            run {
                if (sco) {
                    am.stopBluetoothSco()
                    am.isBluetoothScoOn = false
                    sco = false
                }
                am.isSpeakerphoneOn = false
            }
        }
        am.mode = mode
        focus?.let { am.abandonAudioFocusRequest(it) }
        focus = null
    }

    private companion object {
        val HEADSETS = setOf(
            AudioDeviceInfo.TYPE_WIRED_HEADSET,
            AudioDeviceInfo.TYPE_WIRED_HEADPHONES,
            AudioDeviceInfo.TYPE_USB_HEADSET,
            AudioDeviceInfo.TYPE_BLUETOOTH_SCO,
            AudioDeviceInfo.TYPE_HEARING_AID,
            AudioDeviceInfo.TYPE_BLE_HEADSET,
        )
    }
}
