package io.github.ssenerg.gochat.core

import gochat.v1.Channel
import gochat.v1.ChannelType
import gochat.v1.CreateChannelRequest
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.socket.Connection
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.voice.IceCandidate
import io.github.ssenerg.gochat.core.voice.IceServer
import io.github.ssenerg.gochat.core.voice.RtcEngine
import io.github.ssenerg.gochat.core.voice.RtcListener
import io.github.ssenerg.gochat.core.voice.RtcPeer
import io.github.ssenerg.gochat.core.voice.RtcState
import io.github.ssenerg.gochat.core.voice.VideoSlots
import io.github.ssenerg.gochat.core.voice.VideoSource
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlin.random.Random
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/**
 * Talks in a voice channel of a running stack, as the apps do, with a
 * stand-in for the platform's WebRTC: the signaling, the voice states and
 * the hang-ups are the real ones.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class LiveVoiceTest {
    private val url = System.getenv("GOCHAT_E2E_URL")
    private val admin = System.getenv("GOCHAT_E2E_ADMIN")
    private val password = System.getenv("GOCHAT_E2E_ADMIN_PASSWORD")

    @Test
    fun aCallTalksInAVoiceChannel() = live { scope -> talk(scope) }

    @Test
    fun aCallSendsItsCameraAndScreen() = live { scope -> video(scope) }

    private fun live(test: suspend (CoroutineScope) -> Unit) {
        if (url == null || admin == null || password == null) {
            println("GOCHAT_E2E_URL is not set: skipping")
            return
        }
        runBlocking {
            val main = Dispatchers.Default.limitedParallelism(1)
            val scope = CoroutineScope(SupervisorJob() + main)
            try {
                withTimeout(60_000) { test(scope) }
            } finally {
                scope.cancel()
            }
        }
    }

    /** Signs in with [rtc] for WebRTC and makes a voice channel. */
    private suspend fun signIn(scope: CoroutineScope, rtc: FakeRtc): Triple<GoChatApp, ChatSession, Channel> {
        val app = GoChatApp(MemoryStore(), scope, rtc = rtc)
        assertEquals(null, withContext(scope.coroutineContext) { app.signIn(url!!, admin!!, password!!) })
        val session = (app.state.value as AppState.SignedIn).session
        val st = session.state.first { !it.loading && it.channels.isNotEmpty() }
        session.socket.connection.first { it is Connection.Online }
        val room = withContext(scope.coroutineContext) {
            session.createChannel(
                CreateChannelRequest(team_id = st.teamId!!, name = "app-voice-" + Random.nextInt(100_000), display_name = "App voice", type = ChannelType.CHANNEL_TYPE_VOICE),
            )!!
        }
        return Triple(app, session, room)
    }

    private suspend fun talk(scope: CoroutineScope) {
        val rtc = FakeRtc()
        val (app, session, room) = signIn(scope, rtc)
        val me = session.me.id
        val call = session.voice
        val errors = scope.async { call.errors.first() }

        // The voice server offers, the call answers, and the others see the
        // person in the channel.
        withContext(scope.coroutineContext) { call.join(room) }
        assertEquals(room.id, call.state.value?.channelId)
        rtc.offers.first { it > 0 }
        session.state.first { s -> s.voice[room.id]?.states?.any { it.user?.id == me } == true }

        // Candidates go both ways; once the voice flows, the call says so,
        // and a loud microphone is the person speaking.
        rtc.listener!!.onCandidate(IceCandidate("candidate:1 1 udp 2130706431 127.0.0.1 50000 typ host", "0", 0))
        rtc.listener!!.onState(RtcState.Connected)
        call.state.first { it?.connected == true }
        rtc.listener!!.onLevel(0.5f)
        call.state.first { it?.speaking?.contains(me) == true }

        // Muting silences the microphone and tells the others.
        withContext(scope.coroutineContext) { call.toggleMute() }
        assertEquals(true, rtc.muted.value)
        session.state.first { s -> s.voice[room.id]?.states?.any { it.user?.id == me && it.self_mute } == true }
        rtc.listener!!.onLevel(0.5f)
        call.state.first { it?.speaking?.contains(me) == false }
        withContext(scope.coroutineContext) { call.toggleMute() }
        session.state.first { s -> s.voice[room.id]?.states?.any { it.user?.id == me && !it.self_mute } == true }

        // A moderator takes the person out: the call ends and says why.
        val other = GoChatApi(ApiClient(url!!))
        other.client.token = other.login(admin!!, password!!).session!!.token
        other.disconnectVoiceMember(room.id, me)
        assertEquals("Disconnected from voice: disconnected by a moderator.", errors.await())
        call.state.first { it == null }
        assertTrue(rtc.closed > 0, "the microphone is given back")

        // Joining again and leaving.
        withContext(scope.coroutineContext) { call.join(room) }
        rtc.offers.first { it > 1 }
        session.state.first { s -> s.voice[room.id]?.states?.any { it.user?.id == me } == true }
        withContext(scope.coroutineContext) { call.leave() }
        assertEquals(null, call.state.value)
        session.state.first { s -> s.voice[room.id]?.states?.none { it.user?.id == me } != false }

        withContext(scope.coroutineContext) { app.signOut() }
        other.logout()
    }

    private suspend fun video(scope: CoroutineScope) {
        val rtc = FakeRtc(video = true)
        val (app, session, room) = signIn(scope, rtc)
        val me = session.me.id
        val call = session.voice
        assertTrue(call.video)

        // A camera turned on before the voice connects goes out once the
        // slots are answered. The others see its mark once the voice server
        // forwards it, which a fake WebRTC never sends it anything to.
        withContext(scope.coroutineContext) {
            call.join(room)
            call.toggleCamera()
        }
        rtc.offers.first { it > 0 }
        assertNotNull(rtc.slots, "the first offer names the slots")
        rtc.listener!!.onState(RtcState.Connected)
        rtc.camera.first { it }
        call.videos.first { v -> v.any { it.local && it.source == VideoSource.Camera && it.userId == me } }

        // The screen goes out beside it.
        withContext(scope.coroutineContext) { call.setVideo(VideoSource.Screen, true) }
        call.videos.first { v -> v.any { it.local && it.source == VideoSource.Screen } }

        // Someone else's camera comes and goes as their stream does.
        rtc.listener!!.onVideo("u2:camera", "their camera")
        call.videos.first { v -> v.any { !it.local && it.userId == "u2" && it.source == VideoSource.Camera && it.track == "their camera" } }
        rtc.listener!!.onVideo("u2:camera", null)
        call.videos.first { v -> v.none { it.userId == "u2" } }

        // The system stopping the capture stops the screen.
        rtc.listener!!.onScreenEnded()
        call.state.first { it?.screen == false }
        call.videos.first { v -> v.none { it.local && it.source == VideoSource.Screen } }

        // A camera that stops by itself goes off and says so.
        var errors = scope.async { call.errors.first() }
        rtc.listener!!.onCameraEnded()
        assertEquals("The camera stopped.", errors.await())
        call.state.first { it?.camera == false }
        call.videos.first { v -> v.none { it.local && it.source == VideoSource.Camera } }

        // A camera that cannot start goes back off and says so.
        rtc.broken = true
        errors = scope.async { call.errors.first() }
        withContext(scope.coroutineContext) { call.toggleCamera() }
        assertEquals("Could not turn the camera on.", errors.await())
        call.state.first { it?.camera == false }

        withContext(scope.coroutineContext) { call.leave() }
        assertTrue(rtc.stoppedVideo > 0, "leaving stops the camera and the screen")
        withContext(scope.coroutineContext) { app.signOut() }
    }

    /** Answers like WebRTC, with an SDP the voice server takes, and records what the call asks. */
    private class FakeRtc(override val video: Boolean = false) : RtcEngine {
        var listener: RtcListener? = null
        val offers = MutableStateFlow(0)
        val muted = MutableStateFlow<Boolean?>(null)
        val camera = MutableStateFlow(false)

        @Volatile
        var slots: VideoSlots? = null

        /** Set for a camera that cannot start. */
        @Volatile
        var broken = false

        @Volatile
        var closed = 0

        @Volatile
        var stoppedVideo = 0

        override fun connect(iceServers: List<IceServer>, listener: RtcListener): RtcPeer {
            this.listener = listener
            return object : RtcPeer {
                override suspend fun answer(offer: String, slots: VideoSlots?): String {
                    if (slots != null) this@FakeRtc.slots = slots
                    return fakeAnswer(offer).also { offers.value++ }
                }

                override suspend fun addCandidate(candidate: IceCandidate) {}

                override fun setMuted(muted: Boolean) {
                    this@FakeRtc.muted.value = muted
                }

                override fun setDeafened(deafened: Boolean) {}

                override suspend fun setCamera(on: Boolean): Any? {
                    if (on && broken) return null
                    camera.value = on
                    return if (on) "my camera" else null
                }

                override suspend fun setScreen(on: Boolean): Any? = if (on) "my screen" else null

                override fun close() {
                    closed++
                }
            }
        }

        override fun stopVideo() {
            stoppedVideo++
        }

        private fun fakeAnswer(offer: String): String {
            val media = offer.split(Regex("\r?\n(?=m=)")).drop(1)
            val mids = media.map { Regex("a=mid:(\\S+)").find(it)!!.groupValues[1] }
            return buildString {
                append("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n")
                append("a=group:BUNDLE ${mids.joinToString(" ")}\r\n")
                append("a=fingerprint:sha-256 ${List(32) { "AB" }.joinToString(":")}\r\n")
                append("a=ice-ufrag:fakeufrag\r\na=ice-pwd:fakepasswordfakepassword\r\n")
                media.forEachIndexed { i, m ->
                    val kind = if (m.startsWith("m=video")) "video" else "audio"
                    val codec = if (kind == "video") "VP8/90000" else "opus/48000/2"
                    val pt = Regex("a=rtpmap:(\\d+) ${Regex.escape(codec)}").find(m)?.groupValues?.get(1) ?: if (kind == "video") "96" else "111"
                    val dir = when {
                        "a=recvonly" in m -> "sendonly"
                        "a=sendonly" in m -> "recvonly"
                        "a=inactive" in m -> "inactive"
                        else -> "sendrecv"
                    }
                    append("m=$kind 9 UDP/TLS/RTP/SAVPF $pt\r\nc=IN IP4 0.0.0.0\r\na=mid:${mids[i]}\r\n")
                    append("a=setup:active\r\na=rtcp-mux\r\na=rtpmap:$pt $codec\r\na=$dir\r\n")
                }
            }
        }
    }
}
