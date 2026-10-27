package io.github.ssenerg.gochat.core.voice

/** A TURN server the voice server lends, as WebRTC's RTCIceServer has it. */
data class IceServer(val urls: List<String>, val username: String = "", val credential: String = "")

/** An ICE candidate, as WebRTC's RTCIceCandidateInit has it. */
data class IceCandidate(val candidate: String, val sdpMid: String?, val sdpMLineIndex: Int?)

/** How a voice connection stands. */
enum class RtcState { Connecting, Connected, Disconnected, Failed }

/** What a person sends besides their voice, as the voice server names it. */
enum class VideoSource(val wire: String) {
    Camera("camera"),
    Screen("screen"),
}

/**
 * The mids of the transceivers the voice server takes this device's camera
 * and screen on: they are answered sendonly, whether anything goes out on
 * them yet or not, so turning either on needs no new offer.
 */
data class VideoSlots(val camera: String, val screen: String)

/**
 * A camera or screen in a call: someone else's as it comes in, or this
 * device's own while it goes out. [track] is the platform's video track,
 * for the screen that draws it: an org.webrtc.VideoTrack on Android, an
 * RTCVideoTrack on iOS.
 */
class CallVideo(val userId: String, val source: VideoSource, val track: Any, val local: Boolean = false)

/** What a platform's WebRTC tells a call, from any thread. */
interface RtcListener {
    /** A candidate of this device's, for the voice server. */
    fun onCandidate(candidate: IceCandidate)

    fun onState(state: RtcState)

    /** How loud the microphone is, from 0 to 1, several times a second. */
    fun onLevel(level: Float)

    /**
     * Someone else's camera or screen started coming, on the stream the
     * voice server names after them and it, as "u1:camera"; [track] is null
     * once it stopped.
     */
    fun onVideo(streamId: String, track: Any?) {}

    /** The screen is no longer captured: the person or the system stopped it. */
    fun onScreenEnded() {}

    /** The camera stopped capturing: another app took it, or it failed. */
    fun onCameraEnded() {}
}

/**
 * A platform's WebRTC, which carries the voice of a call, and the camera
 * and screen where it can: Android's comes from the stream-webrtc-android
 * library, iOS's from the WebRTC package the Swift side links. The engine
 * owns the camera and the screen while they are captured, so a call that
 * reconnects keeps them.
 */
interface RtcEngine {
    /** Whether this platform sends and shows cameras and screens. */
    val video: Boolean get() = false

    /**
     * Starts a connection that sends the microphone and plays the voices
     * that come back. The microphone is allowed by then.
     */
    fun connect(iceServers: List<IceServer>, listener: RtcListener): RtcPeer

    /** Stops capturing the camera and the screen, once a call is over. */
    fun stopVideo() {}
}

/** One voice connection to the voice server, which always offers. */
interface RtcPeer {
    /**
     * Takes an offer of the voice server's and returns the answer to send;
     * [slots] are the camera's and the screen's, for an engine with video.
     */
    suspend fun answer(offer: String, slots: VideoSlots?): String

    /** Adds a candidate of the voice server's, once its first offer is in. */
    suspend fun addCandidate(candidate: IceCandidate)

    /** Stops sending the microphone, or sends it again. */
    fun setMuted(muted: Boolean)

    /** Stops playing the others, or plays them again. */
    fun setDeafened(deafened: Boolean)

    /**
     * Starts or stops sending the camera, once the slots are answered:
     * returns its track, for the person to see themselves, or null when it
     * is off or could not start.
     */
    suspend fun setCamera(on: Boolean): Any? = null

    /**
     * Starts or stops sending the screen the platform captures, once the
     * slots are answered: returns its track, or null when it is off or
     * nothing captures it.
     */
    suspend fun setScreen(on: Boolean): Any? = null

    /** Ends the connection and gives back the microphone and the speaker. */
    fun close()
}
