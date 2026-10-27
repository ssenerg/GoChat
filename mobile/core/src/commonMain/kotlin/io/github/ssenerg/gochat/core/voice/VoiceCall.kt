package io.github.ssenerg.gochat.core.voice

import gochat.v1.Channel
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.state.KeyValueStore
import io.ktor.client.plugins.websocket.webSocket
import io.ktor.http.Url
import io.ktor.websocket.Frame
import io.ktor.websocket.readText
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.Channel as Queue
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlin.time.Duration.Companion.milliseconds
import kotlin.time.Duration.Companion.seconds
import kotlin.time.TimeSource

/** The voice channel the person talks in. */
data class VoiceCallState(
    val channelId: String,
    val teamId: String,
    val name: String,
    /** The voice flows; until then, and while it reconnects, it does not. */
    val connected: Boolean = false,
    /** Whom the person hears talk now, themselves included. */
    val speaking: Set<String> = emptySet(),
    /** Whether the person wants their camera, and their screen, to go out. */
    val camera: Boolean = false,
    val screen: Boolean = false,
)

/**
 * Talking in voice channels, as the web's voice client does: the chat
 * server hands out a token for the channel's voice server, whose signaling
 * websocket takes it; the voice server offers, the platform's WebRTC
 * answers, and the voices flow. Where the platform's WebRTC has video, the
 * camera and the screen go out too, and the others' come in. One call at a
 * time; joining another channel leaves the first. Use it from the main
 * thread.
 */
class VoiceCall internal constructor(
    private val api: GoChatApi,
    private val engine: RtcEngine?,
    private val store: KeyValueStore,
    private val myId: String,
    private val scope: CoroutineScope,
) {
    /** Whether this platform can talk in voice channels. */
    val available: Boolean get() = engine != null

    /** Whether this platform sends and shows cameras and screens. */
    val video: Boolean get() = engine?.video == true

    private val _state = MutableStateFlow<VoiceCallState?>(null)
    val state: StateFlow<VoiceCallState?> = _state.asStateFlow()

    // Kept across calls and launches, as the web keeps them.
    private val _muted = MutableStateFlow(store.get(KEY_MUTED) == "1")
    val muted: StateFlow<Boolean> = _muted.asStateFlow()
    private val _deafened = MutableStateFlow(store.get(KEY_DEAFENED) == "1")
    val deafened: StateFlow<Boolean> = _deafened.asStateFlow()

    private val _videos = MutableStateFlow<List<CallVideo>>(emptyList())

    /** The cameras and screens in the call: the others' that come in, and the person's own. */
    val videos: StateFlow<List<CallVideo>> = _videos.asStateFlow()

    private val _errors = MutableSharedFlow<String>(extraBufferCapacity = 8)

    /** Why a call ended, or could not start. */
    val errors: SharedFlow<String> = _errors.asSharedFlow()

    private var job: Job? = null
    private var peer: RtcPeer? = null

    /** Set while the voice server counts the person in, whose state is then theirs to report. */
    private var identified = false
    private val reporting = Mutex()

    /** What goes out now, besides the voice, as the others are told. */
    private var sending = emptySet<VideoSource>()

    /** The connection's own events, which a camera or screen turned on or off joins. */
    private var signals: Queue<Signal>? = null

    fun join(channel: Channel) {
        val engine = engine ?: run {
            _errors.tryEmit("Talking in voice channels is not in this app yet.")
            return
        }
        if (_state.value?.channelId == channel.id) return
        hangUp()
        _state.value = VoiceCallState(channel.id, channel.team_id, channel.display_name)
        job = scope.launch { run(engine, channel.id) }
    }

    fun leave() {
        if (_state.value == null) return
        hangUp()
        scope.launch { runCatching { api.leaveVoice() } }
    }

    /** Mutes, or unmutes and undeafens, as the web's button does. */
    fun toggleMute() {
        if (_muted.value || _deafened.value) set(muted = false, deafened = false) else set(muted = true, deafened = false)
    }

    fun toggleDeafen() = set(_muted.value, !_deafened.value)

    fun toggleCamera() {
        val st = _state.value ?: return
        setVideo(VideoSource.Camera, !st.camera)
    }

    /**
     * Starts or stops sending the camera or the screen; it goes out once the
     * voice connects. The screen must be captured first: Android asks the
     * person, iOS starts the broadcast.
     */
    fun setVideo(source: VideoSource, on: Boolean) {
        if (!video) return
        _state.update {
            when (source) {
                VideoSource.Camera -> it?.copy(camera = on)
                VideoSource.Screen -> it?.copy(screen = on)
            }
        }
        signals?.trySend(Signal.Video)
    }

    /** Hangs up without a word to the chat server, for signing out. */
    internal fun close() = hangUp()

    private fun set(muted: Boolean, deafened: Boolean) {
        _muted.value = muted
        _deafened.value = deafened
        store.put(KEY_MUTED, if (muted) "1" else null)
        store.put(KEY_DEAFENED, if (deafened) "1" else null)
        peer?.apply {
            setMuted(muted || deafened)
            setDeafened(deafened)
        }
        report()
    }

    /**
     * Tells the others whether the person is muted or deafened; the last
     * call carries the latest. The voice server tells them of the camera
     * and the screen, which it forwards.
     */
    private fun report() {
        if (!identified) return
        scope.launch {
            reporting.withLock {
                if (identified) runCatching { api.updateVoiceState(_muted.value || _deafened.value, _deafened.value) }
            }
        }
    }

    private fun hangUp() {
        job?.cancel()
        job = null
        // The microphone and the camera go now, not when the cancelled call
        // next runs.
        peer?.close()
        peer = null
        engine?.stopVideo()
        identified = false
        sending = emptySet()
        _videos.value = emptyList()
        _state.value = null
    }

    private fun end(message: String?) {
        job = null
        peer = null
        engine?.stopVideo()
        identified = false
        sending = emptySet()
        _videos.value = emptyList()
        _state.value = null
        message?.let { _errors.tryEmit(it) }
    }

    private suspend fun run(engine: RtcEngine, channelId: String) {
        var everConnected = false
        var tries = 0
        while (true) {
            val outcome = try {
                talk(engine, channelId) { everConnected = true }
            } catch (e: CancellationException) {
                throw e
            } catch (e: ApiException) {
                Outcome.Failed(e.userMessage())
            } catch (_: Exception) {
                // The websocket did not open, or broke.
                if (everConnected) Outcome.Lost(false) else Outcome.Failed("Cannot reach the voice server.")
            }
            when (outcome) {
                is Outcome.Failed -> return end(outcome.message)
                is Outcome.Lost -> {
                    // A network that changed or dropped: join again, as
                    // long as the voice ever flowed, so it can.
                    if (outcome.connected) tries = 0
                    if (!everConnected || ++tries > MAX_RETRIES) return end("Disconnected from voice.")
                    _state.update { it?.copy(connected = false, speaking = emptySet()) }
                    delay(RETRY_DELAY * (1 shl (tries - 1)))
                }
            }
        }
    }

    private sealed interface Outcome {
        /** It ended for good, and says why. */
        data class Failed(val message: String) : Outcome

        /** It dropped; [connected] says whether the voice flowed before. */
        data class Lost(val connected: Boolean) : Outcome
    }

    private sealed interface Signal {
        data class Message(val text: String) : Signal

        data class Candidate(val candidate: IceCandidate) : Signal

        data class State(val state: RtcState) : Signal

        data class Level(val level: Float) : Signal

        /** Someone else's camera or screen came, or went when [track] is null. */
        data class RemoteVideo(val streamId: String, val track: Any?) : Signal

        /** The person turned the camera or screen on or off. */
        data object Video : Signal

        data object ScreenEnded : Signal

        data object CameraEnded : Signal

        data object TimedOut : Signal

        data object Closed : Signal
    }

    /** One connection to the voice server, from the token to the hang-up. */
    private suspend fun talk(engine: RtcEngine, channelId: String, onConnected: () -> Unit): Outcome {
        val ticket = api.joinVoice(channelId)
        ticket.channel?.let { ch -> _state.update { it?.copy(name = ch.display_name.ifBlank { it.name }) } }
        unreachable(ticket.endpoint)?.let { return Outcome.Failed(it) }

        // Everything that happens, handled in order on this coroutine: the
        // voice server's messages and the platform's WebRTC events.
        val signals = Queue<Signal>(Queue.UNLIMITED)
        this.signals = signals
        val listener = object : RtcListener {
            override fun onCandidate(candidate: IceCandidate) {
                signals.trySend(Signal.Candidate(candidate))
            }

            override fun onState(state: RtcState) {
                signals.trySend(Signal.State(state))
            }

            override fun onLevel(level: Float) {
                signals.trySend(Signal.Level(level))
            }

            override fun onVideo(streamId: String, track: Any?) {
                signals.trySend(Signal.RemoteVideo(streamId, track))
            }

            override fun onScreenEnded() {
                signals.trySend(Signal.ScreenEnded)
            }

            override fun onCameraEnded() {
                signals.trySend(Signal.CameraEnded)
            }
        }
        var p: RtcPeer? = null
        var connected = false
        var outcome: Outcome? = null
        try {
            api.client.http.webSocket(urlString = ticket.endpoint) {
                val pump = launch {
                    for (frame in incoming) if (frame is Frame.Text) signals.send(Signal.Message(frame.readText()))
                    signals.send(Signal.Closed)
                }
                send(Frame.Text(buildJsonObject { put("op", "identify"); put("token", ticket.token); put("video", engine.video) }.toString()))
                var timer: Job? = null
                val early = mutableListOf<IceCandidate>()
                var offered = false
                // Set once an answer took the camera's and the screen's slots.
                var slotted = false
                var speaking = false
                var loudAt: TimeSource.Monotonic.ValueTimeMark? = null

                suspend fun emit(msg: JsonObject) = send(Frame.Text(msg.toString()))

                loop@ while (true) {
                    when (val s = signals.receive()) {
                        is Signal.Message -> {
                            val msg = runCatching { Json.parseToJsonElement(s.text).jsonObject }.getOrNull() ?: continue@loop
                            when (msg.string("op")) {
                                "ready" -> {
                                    if (p != null) continue@loop
                                    val created = try {
                                        engine.connect(iceServers(msg), listener)
                                    } catch (e: CancellationException) {
                                        throw e
                                    } catch (_: Exception) {
                                        outcome = Outcome.Failed("Could not start the microphone.")
                                        break@loop
                                    }
                                    p = created
                                    peer = created
                                    identified = true
                                    created.setMuted(_muted.value || _deafened.value)
                                    created.setDeafened(_deafened.value)
                                    if (_muted.value || _deafened.value) report()
                                    timer = launch {
                                        delay(CONNECT_TIMEOUT)
                                        signals.send(Signal.TimedOut)
                                    }
                                }
                                "offer" -> {
                                    val current = p ?: continue@loop
                                    val sdp = msg.string("sdp") ?: continue@loop
                                    val slots = (msg["slots"] as? JsonObject)?.let(::slots)
                                    val answer = try {
                                        current.answer(sdp, slots)
                                    } catch (e: CancellationException) {
                                        throw e
                                    } catch (_: Exception) {
                                        outcome = Outcome.Failed("The voice connection failed.")
                                        break@loop
                                    }
                                    if (!offered) {
                                        offered = true
                                        early.forEach { runCatching { current.addCandidate(it) } }
                                        early.clear()
                                    }
                                    emit(buildJsonObject { put("op", "answer"); put("sdp", answer) })
                                    if (slots != null && !slotted) {
                                        slotted = true
                                        // A camera or screen turned on early, or before a reconnect, goes out now.
                                        signals.trySend(Signal.Video)
                                    }
                                }
                                "candidate" -> {
                                    val c = (msg["candidate"] as? JsonObject)?.let(::candidate) ?: continue@loop
                                    val current = p
                                    if (current == null || !offered) early += c else runCatching { current.addCandidate(c) }
                                }
                                "speaking" -> {
                                    val user = msg.string("user_id") ?: continue@loop
                                    setSpeaking(user, msg["speaking"]?.jsonPrimitive?.booleanOrNull == true)
                                }
                                // The room forwards no more cameras and screens: this one goes off.
                                "video" -> {
                                    val source = VideoSource.entries.firstOrNull { it.wire == msg.string("source") } ?: continue@loop
                                    if (msg["on"]?.jsonPrimitive?.booleanOrNull == false && source in sending) {
                                        setVideo(source, false)
                                        _errors.tryEmit("Your ${source.wire} is off: ${msg.string("reason") ?: "the voice channel refused it"}.")
                                    }
                                }
                            }
                        }
                        is Signal.Candidate -> emit(
                            buildJsonObject {
                                put("op", "candidate")
                                put(
                                    "candidate",
                                    buildJsonObject {
                                        put("candidate", s.candidate.candidate)
                                        s.candidate.sdpMid?.let { put("sdpMid", it) }
                                        s.candidate.sdpMLineIndex?.let { put("sdpMLineIndex", it) }
                                    },
                                )
                            },
                        )
                        is Signal.State -> when (s.state) {
                            RtcState.Connected -> {
                                connected = true
                                timer?.cancel()
                                onConnected()
                                _state.update { it?.copy(connected = true) }
                            }
                            // It may come back on its own.
                            RtcState.Disconnected -> _state.update { it?.copy(connected = false) }
                            RtcState.Failed -> {
                                outcome = if (connected) Outcome.Lost(true) else Outcome.Failed(NETWORK)
                                break@loop
                            }
                            RtcState.Connecting -> {}
                        }
                        is Signal.Level -> {
                            // Speaking is a voice heard in the last moment,
                            // as the web judges it.
                            val now = TimeSource.Monotonic.markNow()
                            if (s.level > SPEAKING_LEVEL) loudAt = now
                            val silent = _muted.value || _deafened.value
                            val on = connected && !silent && loudAt?.let { now - it < SPEAKING_HOLD } == true
                            if (on != speaking) {
                                speaking = on
                                emit(buildJsonObject { put("op", "speaking"); put("speaking", on) })
                                setSpeaking(myId, on)
                            }
                        }
                        is Signal.RemoteVideo -> {
                            val at = s.streamId.lastIndexOf(':')
                            val source = VideoSource.entries.firstOrNull { it.wire == s.streamId.substring(at + 1) }
                            if (at <= 0 || source == null) continue@loop
                            val user = s.streamId.substring(0, at)
                            _videos.update { list ->
                                list.filterNot { !it.local && it.userId == user && it.source == source } +
                                    listOfNotNull(s.track?.let { CallVideo(user, source, it) })
                            }
                        }
                        Signal.Video -> {
                            val current = p
                            if (current == null || !slotted) continue@loop
                            for (source in VideoSource.entries) {
                                val want = _state.value?.let { if (source == VideoSource.Camera) it.camera else it.screen } == true
                                if (want == source in sending) continue
                                val track = try {
                                    if (source == VideoSource.Camera) current.setCamera(want) else current.setScreen(want)
                                } catch (e: CancellationException) {
                                    throw e
                                } catch (_: Exception) {
                                    null
                                }
                                if (want && track == null) {
                                    // Nothing could go out: the switch goes back off.
                                    setVideo(source, false)
                                    _errors.tryEmit(if (source == VideoSource.Camera) "Could not turn the camera on." else "Could not share the screen.")
                                    continue
                                }
                                sending = if (want) sending + source else sending - source
                                emit(buildJsonObject { put("op", "video"); put("source", source.wire); put("on", want) })
                                _videos.update { list ->
                                    list.filterNot { it.local && it.source == source } +
                                        listOfNotNull(track?.let { CallVideo(myId, source, it, local = true) })
                                }
                            }
                        }
                        Signal.ScreenEnded -> setVideo(VideoSource.Screen, false)
                        Signal.CameraEnded -> if (_state.value?.camera == true) {
                            setVideo(VideoSource.Camera, false)
                            _errors.tryEmit("The camera stopped.")
                        }
                        Signal.TimedOut -> if (!connected) {
                            outcome = Outcome.Failed(NETWORK)
                            break@loop
                        }
                        Signal.Closed -> break@loop
                    }
                }
                timer?.cancel()
                pump.cancel()
                if (outcome == null) {
                    val reason = closeReason.await()
                    outcome = closed(reason?.code?.toInt(), reason?.message.orEmpty(), connected)
                }
            }
        } finally {
            p?.close()
            if (this.signals === signals) this.signals = null
            // A call joined since is not this one's to reset.
            if (peer === p) {
                peer = null
                identified = false
                // A connection that comes back sends again what the person wants out.
                sending = emptySet()
                _videos.value = emptyList()
            }
        }
        return outcome ?: Outcome.Lost(connected)
    }

    private fun setSpeaking(userId: String, on: Boolean) {
        _state.update { st -> st?.copy(speaking = if (on) st.speaking + userId else st.speaking - userId) }
    }

    /** What the voice server's close code means; client.proto and the voice server list them. */
    private fun closed(code: Int?, reason: String, connected: Boolean): Outcome = when (code) {
        4001 -> Outcome.Failed("Could not join the voice channel: try again.")
        4002 -> Outcome.Failed("Disconnected from voice: ${reason.ifBlank { "the server ended it" }}.")
        4003 -> Outcome.Failed("That voice channel is full.")
        4004 -> Outcome.Failed("Voice is unavailable right now.")
        4005 -> if (connected) Outcome.Lost(true) else Outcome.Failed("The voice connection failed.")
        else -> Outcome.Lost(connected)
    }

    /**
     * Says why a voice server announced at this machine's own address is out
     * of reach, as a development stack's default is, unless the chat server
     * is on this machine too.
     */
    private fun unreachable(endpoint: String): String? {
        val host = runCatching { Url(endpoint).host }.getOrNull() ?: return "The server gave a voice server address this app cannot use."
        val chat = runCatching { Url(api.client.baseUrl).host }.getOrNull()
        if (host.isLoopback() && chat?.isLoopback() != true) {
            return "The server announces its voice server at $host, which this device cannot reach: set VOICE_PUBLIC_URL on the server."
        }
        return null
    }

    private fun String.isLoopback() = this == "localhost" || startsWith("127.") || this == "::1" || this == "[::1]"

    companion object {
        private const val KEY_MUTED = "voice.muted"
        private const val KEY_DEAFENED = "voice.deafened"
        private const val SPEAKING_LEVEL = 0.02f
        private val SPEAKING_HOLD = 300.milliseconds
        private val CONNECT_TIMEOUT = 20.seconds
        private val RETRY_DELAY = 1.seconds
        private const val MAX_RETRIES = 3
        private const val NETWORK = "The voice connection failed: your network may block it."

        private fun JsonObject.string(key: String): String? = (this[key] as? kotlinx.serialization.json.JsonPrimitive)?.contentOrNull

        private fun iceServers(ready: JsonObject): List<IceServer> =
            (ready["ice_servers"] as? kotlinx.serialization.json.JsonArray)?.mapNotNull { el ->
                val o = el as? JsonObject ?: return@mapNotNull null
                val urls = runCatching { o["urls"]!!.jsonArray.mapNotNull { it.jsonPrimitive.contentOrNull } }.getOrNull() ?: return@mapNotNull null
                IceServer(urls, o.string("username").orEmpty(), o.string("credential").orEmpty())
            }.orEmpty()

        private fun slots(o: JsonObject): VideoSlots? {
            val camera = o.string("camera") ?: return null
            val screen = o.string("screen") ?: return null
            return VideoSlots(camera, screen)
        }

        private fun candidate(o: JsonObject): IceCandidate? {
            val c = o.string("candidate") ?: return null
            return IceCandidate(c, o.string("sdpMid"), (o["sdpMLineIndex"] as? kotlinx.serialization.json.JsonPrimitive)?.intOrNull)
        }
    }
}
