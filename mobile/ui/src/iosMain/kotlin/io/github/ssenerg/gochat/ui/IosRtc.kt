package io.github.ssenerg.gochat.ui

import io.github.ssenerg.gochat.core.voice.IceCandidate
import io.github.ssenerg.gochat.core.voice.IceServer
import io.github.ssenerg.gochat.core.voice.RtcEngine
import io.github.ssenerg.gochat.core.voice.RtcListener
import io.github.ssenerg.gochat.core.voice.RtcPeer
import io.github.ssenerg.gochat.core.voice.RtcState
import io.github.ssenerg.gochat.core.voice.VideoSlots
import kotlinx.coroutines.suspendCancellableCoroutine
import platform.UIKit.UIView
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

// WebRTC as the iOS app supplies it. Its Swift side links Google's WebRTC,
// as stasel/WebRTC builds it, and implements these protocols; Kotlin adapts
// them to the core's RtcEngine. Kotlin calls in on the main thread, and
// Swift calls back on any. Video tracks cross as opaque objects, the
// RTCVideoTracks Swift hands out, which only Swift draws.

/** Starts voice connections, and owns the camera and the screen's broadcast. */
interface IosRtcEngine {
    /** Whether this build sends and shows video. */
    val video: Boolean

    fun connect(iceServers: List<IosIceServer>, events: IosRtcEvents): IosRtcPeer

    /** Stops the camera and the screen's broadcast, once a call is over. */
    fun stopVideo()

    /** Opens the system's picker for the broadcast that shares the screen. */
    fun pickBroadcast()

    /** Hears the screen's broadcast start, which turns the call's screen on. */
    fun setScreenEvents(events: IosScreenEvents?)

    /** A view that draws [track]; [fill] crops it to the view, or else fits it whole. */
    fun videoView(track: Any, mirror: Boolean, fill: Boolean): UIView

    /** Lets go of a view [videoView] made for [track]. */
    fun releaseVideoView(view: UIView, track: Any)
}

/** A TURN server the voice server lends. */
class IosIceServer(val urls: List<String>, val username: String, val credential: String)

/** What a connection tells the call. */
interface IosRtcEvents {
    /** A candidate of this device's; [sdpMLineIndex] is -1 when it has none. */
    fun candidate(sdp: String, sdpMid: String?, sdpMLineIndex: Int)

    /** "connecting", "connected", "disconnected" or "failed". */
    fun state(state: String)

    /** How loud the microphone is, from 0 to 1, several times a second. */
    fun level(level: Float)

    /** Someone else's camera or screen came on [streamId], or went when [track] is null. */
    fun video(streamId: String, track: Any?)

    /** The screen's broadcast ended. */
    fun screenEnded()
}

/** What the screen's broadcast tells the app, wherever the person started it. */
interface IosScreenEvents {
    fun started()
}

/** Where an answer goes, once WebRTC has made it. */
interface IosRtcAnswer {
    fun answered(sdp: String)

    fun failed(reason: String)
}

/** Where the camera's track goes once it started, or null when it could not. */
interface IosRtcCamera {
    fun started(track: Any?)
}

/** One voice connection, which sends the microphone, the camera and the screen, and plays the others. */
interface IosRtcPeer {
    /** Takes the voice server's offer, answers the slots' mids sendonly, and answers [reply]. */
    fun answer(offer: String, cameraMid: String?, screenMid: String?, reply: IosRtcAnswer)

    fun addCandidate(sdp: String, sdpMid: String?, sdpMLineIndex: Int)

    fun setMuted(muted: Boolean)

    fun setDeafened(deafened: Boolean)

    /** Starts the camera and sends it, or stops it when [on] is false and answers null. */
    fun setCamera(on: Boolean, reply: IosRtcCamera)

    /** Sends the broadcast's screen, and returns its track, or null when none is broadcast; or stops it. */
    fun setScreen(on: Boolean): Any?

    /** Ends the connection; again, it does nothing. */
    fun close()
}

/** The core's RtcEngine, over the Swift one. */
internal class SwiftRtc(val native: IosRtcEngine) : RtcEngine {
    override val video: Boolean get() = native.video

    override fun stopVideo() = native.stopVideo()

    override fun connect(iceServers: List<IceServer>, listener: RtcListener): RtcPeer {
        val events = object : IosRtcEvents {
            override fun candidate(sdp: String, sdpMid: String?, sdpMLineIndex: Int) =
                listener.onCandidate(IceCandidate(sdp, sdpMid, sdpMLineIndex.takeIf { it >= 0 }))

            override fun state(state: String) {
                when (state) {
                    "connecting" -> listener.onState(RtcState.Connecting)
                    "connected" -> listener.onState(RtcState.Connected)
                    "disconnected" -> listener.onState(RtcState.Disconnected)
                    "failed" -> listener.onState(RtcState.Failed)
                }
            }

            override fun level(level: Float) = listener.onLevel(level)

            override fun video(streamId: String, track: Any?) = listener.onVideo(streamId, track)

            override fun screenEnded() = listener.onScreenEnded()
        }
        val peer = native.connect(iceServers.map { IosIceServer(it.urls, it.username, it.credential) }, events)
        return object : RtcPeer {
            override suspend fun answer(offer: String, slots: VideoSlots?): String = suspendCancellableCoroutine { c ->
                peer.answer(
                    offer,
                    slots?.camera,
                    slots?.screen,
                    object : IosRtcAnswer {
                        override fun answered(sdp: String) = c.resume(sdp)

                        override fun failed(reason: String) = c.resumeWithException(IllegalStateException(reason))
                    },
                )
            }

            override suspend fun addCandidate(candidate: IceCandidate) =
                peer.addCandidate(candidate.candidate, candidate.sdpMid, candidate.sdpMLineIndex ?: -1)

            override fun setMuted(muted: Boolean) = peer.setMuted(muted)

            override fun setDeafened(deafened: Boolean) = peer.setDeafened(deafened)

            override suspend fun setCamera(on: Boolean): Any? = suspendCancellableCoroutine { c ->
                peer.setCamera(
                    on,
                    object : IosRtcCamera {
                        override fun started(track: Any?) = c.resume(track)
                    },
                )
            }

            override suspend fun setScreen(on: Boolean): Any? = peer.setScreen(on)

            override fun close() = peer.close()
        }
    }
}
