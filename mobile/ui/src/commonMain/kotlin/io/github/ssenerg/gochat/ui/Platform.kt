package io.github.ssenerg.gochat.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Modifier
import io.github.ssenerg.gochat.core.api.AppInfo
import io.github.ssenerg.gochat.core.state.KeyValueStore
import io.github.ssenerg.gochat.core.voice.CallVideo
import io.github.ssenerg.gochat.core.voice.RtcEngine
import kotlinx.coroutines.flow.StateFlow

/** What each operating system provides the shared screens. */
interface Platform {
    /** The app's version and the device it runs on, for the server to know it by. */
    val app: AppInfo

    /** Durable storage for the server, the session token and preferences. */
    val store: KeyValueStore

    /** Plays voice messages, one at a time. */
    val player: AudioPlayer

    /** WebRTC, for talking in voice channels, where this platform has it. */
    val rtc: RtcEngine?

    /** Whether the app is in the foreground, where what it shows is seen. */
    val foreground: StateFlow<Boolean>

    /** Hands a downloaded file to the system, to open it in another app or save it. */
    fun openFile(name: String, mimeType: String, bytes: ByteArray)

    /** Offers text to share, such as an invite link. */
    fun share(text: String)

    fun copy(text: String)
}

val LocalPlatform = staticCompositionLocalOf<Platform> { error("no platform") }

/** A file the person picked to attach. */
class PickedFile(val name: String, val mimeType: String, val bytes: ByteArray)

/** A finished voice recording. */
class Recording(val bytes: ByteArray, val fileName: String, val durationMs: Long)

interface VoiceRecorder {
    /** Starts recording, asking for the microphone first; false when it is not allowed or available. */
    suspend fun start(): Boolean

    /** Stops and returns the recording, or null when it failed or was too short. */
    suspend fun stop(): Recording?

    fun cancel()

    /** How loud the microphone hears now, from 0 to 1. */
    fun level(): Float
}

/** What the player plays: one voice message, by its attachment id. */
data class PlayerState(
    val id: String? = null,
    val playing: Boolean = false,
    val positionMs: Long = 0,
    val durationMs: Long = 0,
    val error: String? = null,
)

interface AudioPlayer {
    val state: StateFlow<PlayerState>

    /** Plays [bytes] as the audio of [id], stopping anything else. */
    fun play(id: String, bytes: ByteArray, mimeType: String)

    fun pause()

    fun resume()

    fun seek(positionMs: Long)

    fun stop()
}

/** The largest file the apps read into memory to attach. */
const val MAX_PICKED_BYTES = 100L * 1024 * 1024

/** The microphone, asking for it when first used. */
@Composable
expect fun rememberVoiceRecorder(): VoiceRecorder

/** Returns what asks for the microphone when it is not allowed yet; it answers whether it is. */
@Composable
expect fun rememberMicrophoneAccess(): suspend () -> Boolean

/**
 * Returns what asks for what a call needs: the microphone and, where the
 * system asks for it, the notice of the call; it answers whether the
 * microphone is allowed.
 */
@Composable
expect fun rememberCallAccess(): suspend () -> Boolean

/** Returns what asks for the camera when it is not allowed yet; it answers whether it is. */
@Composable
expect fun rememberCameraAccess(): suspend () -> Boolean

/**
 * Returns what starts capturing the screen for a call: Android asks the
 * person whether to share it, iOS opens the system's broadcast picker. It
 * answers whether the screen is captured now, for the call to send it; on
 * iOS the broadcast starts later, and turns the call's screen on itself.
 */
@Composable
expect fun rememberScreenShare(): suspend () -> Boolean

/**
 * Draws a camera or screen of a call: [fill] crops it to the space, as a
 * tile shows a camera, or else fits it whole, as a shared screen shows.
 */
@Composable
expect fun VideoView(video: CallVideo, modifier: Modifier = Modifier, fill: Boolean = true)

/** Returns what opens the system's file picker; [onPicked] gets the file. */
@Composable
expect fun rememberFilePicker(onPicked: (PickedFile) -> Unit, onError: (String) -> Unit): () -> Unit

/** Takes the system's back gesture while [enabled]. */
@Composable
expect fun PlatformBackHandler(enabled: Boolean, onBack: () -> Unit)
