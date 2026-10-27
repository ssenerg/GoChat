package io.github.ssenerg.gochat.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import com.russhwolf.settings.KeychainSettings
import io.github.ssenerg.gochat.core.api.AppInfo
import io.github.ssenerg.gochat.core.state.KeyValueStore
import io.github.ssenerg.gochat.core.voice.RtcEngine
import kotlinx.cinterop.addressOf
import kotlinx.cinterop.convert
import kotlinx.cinterop.usePinned
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.IO
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import platform.AVFAudio.AVAudioPlayer
import platform.AVFAudio.AVAudioRecorder
import platform.AVFAudio.AVAudioSession
import platform.AVFAudio.AVAudioSessionCategoryOptionDefaultToSpeaker
import platform.AVFAudio.AVAudioSessionCategoryPlayAndRecord
import platform.AVFAudio.AVAudioSessionCategoryPlayback
import platform.AVFAudio.AVEncoderBitRateKey
import platform.AVFAudio.AVFormatIDKey
import platform.AVFAudio.AVNumberOfChannelsKey
import platform.AVFAudio.AVSampleRateKey
import platform.AVFAudio.setActive
import platform.AVFoundation.AVCaptureDevice
import platform.AVFoundation.AVMediaTypeVideo
import platform.AVFoundation.requestAccessForMediaType
import platform.CoreAudioTypes.kAudioFormatMPEG4AAC
import platform.Foundation.CFBridgingRetain
import platform.Foundation.NSBundle
import platform.Foundation.NSData
import platform.Foundation.NSFileManager
import platform.Foundation.NSOperationQueue
import platform.Foundation.NSNotificationCenter
import platform.Foundation.NSNumber
import platform.Foundation.NSTemporaryDirectory
import platform.Foundation.NSURL
import platform.Foundation.create
import platform.Foundation.dataWithContentsOfFile
import platform.Foundation.dataWithContentsOfURL
import platform.Foundation.writeToFile
import platform.Security.kSecAttrAccessible
import platform.Security.kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
import platform.Security.kSecAttrService
import platform.UIKit.UIActivityViewController
import platform.UIKit.UIApplicationState
import platform.UIKit.UIApplicationDidEnterBackgroundNotification
import platform.UIKit.UIApplicationDidBecomeActiveNotification
import platform.UIKit.UIApplication
import platform.UIKit.UIDevice
import platform.UIKit.UIDocumentPickerDelegateProtocol
import platform.UIKit.UIDocumentPickerViewController
import platform.UIKit.UIPasteboard
import platform.UIKit.UIViewController
import platform.UIKit.popoverPresentationController
import platform.UniformTypeIdentifiers.UTTypeItem
import platform.darwin.NSObject
import platform.posix.memcpy
import kotlin.coroutines.resume
import kotlin.math.pow

/** iOS's side of the app. */
class IosPlatform(private val scope: CoroutineScope, override val rtc: RtcEngine?) : Platform {
    override val app = AppInfo(
        version = NSBundle.mainBundle.objectForInfoDictionaryKey("CFBundleShortVersionString") as? String ?: "0",
        device = "iOS ${UIDevice.currentDevice.systemVersion}; ${UIDevice.currentDevice.model}",
    )
    override val store: KeyValueStore = KeychainStore()
    override val player: AudioPlayer = IosAudioPlayer(scope)

    private val shown = MutableStateFlow(UIApplication.sharedApplication.applicationState != UIApplicationState.UIApplicationStateBackground)
    override val foreground: StateFlow<Boolean> = shown.asStateFlow()

    init {
        val center = NSNotificationCenter.defaultCenter
        center.addObserverForName(UIApplicationDidBecomeActiveNotification, null, NSOperationQueue.mainQueue) { _ -> shown.value = true }
        center.addObserverForName(UIApplicationDidEnterBackgroundNotification, null, NSOperationQueue.mainQueue) { _ -> shown.value = false }
    }

    override fun openFile(name: String, mimeType: String, bytes: ByteArray) {
        val safe = name.substringAfterLast('/').ifBlank { "file" }
        val path = NSTemporaryDirectory() + safe
        scope.launch {
            val written = withContext(Dispatchers.IO) { bytes.toNSData().writeToFile(path, atomically = true) }
            if (written) present(listOf(NSURL.fileURLWithPath(path)))
        }
    }

    override fun share(text: String) = present(listOf(text))

    override fun copy(text: String) {
        UIPasteboard.generalPasteboard.string = text
    }

    private fun present(items: List<Any>) {
        val top = topController() ?: return
        val sheet = UIActivityViewController(activityItems = items, applicationActivities = null)
        // On iPad the sheet is a popover and needs an anchor.
        sheet.popoverPresentationController?.sourceView = top.view
        top.presentViewController(sheet, animated = true, completion = null)
    }
}

internal fun topController(): UIViewController? {
    var c = UIApplication.sharedApplication.keyWindow?.rootViewController
    while (c?.presentedViewController != null) c = c.presentedViewController
    return c
}

/**
 * The Keychain: the session token stays encrypted on the device, readable
 * once the phone was first unlocked, and never restored to another device.
 */
private class KeychainStore : KeyValueStore {
    private val keychain = KeychainSettings(
        kSecAttrService to CFBridgingRetain(SERVICE),
        kSecAttrAccessible to kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
    )

    // Its queries name no accessibility: they find the items stored before,
    // with the default one, and the others too.
    private val any = KeychainSettings(service = SERVICE)

    override fun get(key: String): String? = keychain.getStringOrNull(key) ?: any.getStringOrNull(key)?.also {
        put(key, it)
    }

    override fun put(key: String, value: String?) {
        any.remove(key)
        if (value != null) keychain.putString(key, value)
    }

    private companion object {
        const val SERVICE = "io.github.ssenerg.gochat"
    }
}

internal fun ByteArray.toNSData(): NSData =
    if (isEmpty()) NSData() else usePinned { NSData.create(bytes = it.addressOf(0), length = size.convert()) }

internal fun NSData.toByteArray(): ByteArray {
    val out = ByteArray(length.toInt())
    if (out.isNotEmpty()) out.usePinned { memcpy(it.addressOf(0), bytes, length) }
    return out
}

private class IosAudioPlayer(private val scope: CoroutineScope) : AudioPlayer {
    private val _state = MutableStateFlow(PlayerState())
    override val state: StateFlow<PlayerState> = _state.asStateFlow()
    private var player: AVAudioPlayer? = null
    private var ticker: Job? = null

    override fun play(id: String, bytes: ByteArray, mimeType: String) {
        stop()
        val session = AVAudioSession.sharedInstance()
        // In a voice channel's call the session already plays, and records.
        if (session.category != AVAudioSessionCategoryPlayAndRecord) {
            session.setCategory(AVAudioSessionCategoryPlayback, null)
            session.setActive(true, null)
        }
        // AVAudioPlayer plays the MP4 recordings of the apps and Safari; the
        // WebM of other browsers it does not.
        val p = runCatching { AVAudioPlayer(data = bytes.toNSData(), error = null) }.getOrNull()
        if (p == null || !p.prepareToPlay() || !p.play()) {
            _state.value = PlayerState(
                id = id,
                error = if ("webm" in mimeType) "Voice messages recorded in this browser format cannot play on iOS yet." else "This voice message cannot play.",
            )
            return
        }
        player = p
        _state.value = PlayerState(id = id, playing = true, durationMs = (p.duration * 1000).toLong())
        tick()
    }

    private fun tick() {
        ticker?.cancel()
        ticker = scope.launch {
            while (isActive) {
                val p = player ?: break
                if (p.playing) {
                    _state.value = _state.value.copy(positionMs = (p.currentTime * 1000).toLong())
                } else if (_state.value.playing) {
                    // It reached the end.
                    _state.value = _state.value.copy(playing = false, positionMs = 0)
                    break
                }
                delay(200)
            }
        }
    }

    override fun pause() {
        player?.pause()
        ticker?.cancel()
        _state.value = _state.value.copy(playing = false)
    }

    override fun resume() {
        val p = player ?: return
        p.play()
        _state.value = _state.value.copy(playing = true)
        tick()
    }

    override fun seek(positionMs: Long) {
        player?.currentTime = positionMs / 1000.0
        _state.value = _state.value.copy(positionMs = positionMs)
    }

    override fun stop() {
        ticker?.cancel()
        player?.stop()
        player = null
        _state.value = PlayerState()
    }
}

private class IosVoiceRecorder : VoiceRecorder {
    private var recorder: AVAudioRecorder? = null
    private val path = NSTemporaryDirectory() + "voice-message.m4a"

    // Whether recording changed the session's category, which a voice
    // channel's call otherwise sets for itself.
    private var tookSession = false

    override suspend fun start(): Boolean {
        val session = AVAudioSession.sharedInstance()
        val granted = suspendCancellableCoroutine { c -> session.requestRecordPermission { ok -> c.resume(ok) } }
        if (!granted) return false
        cancel()
        if (session.category != AVAudioSessionCategoryPlayAndRecord) {
            // The speaker, not the earpiece, for anything that plays meanwhile.
            session.setCategory(AVAudioSessionCategoryPlayAndRecord, withOptions = AVAudioSessionCategoryOptionDefaultToSpeaker, error = null)
            session.setActive(true, null)
            tookSession = true
        }
        // AAC in MP4, as Android records: the server takes it, everyone plays it.
        val settings = mapOf<Any?, Any?>(
            AVFormatIDKey to NSNumber(unsignedInt = kAudioFormatMPEG4AAC),
            AVSampleRateKey to NSNumber(double = 44_100.0),
            AVNumberOfChannelsKey to NSNumber(int = 1),
            AVEncoderBitRateKey to NSNumber(int = 64_000),
        )
        val r = runCatching { AVAudioRecorder(uRL = NSURL.fileURLWithPath(path), settings = settings, error = null) }.getOrNull()
        r?.meteringEnabled = true
        if (r == null || !r.record()) {
            release()
            return false
        }
        recorder = r
        return true
    }

    override suspend fun stop(): Recording? {
        val r = recorder ?: return null
        recorder = null
        val durationMs = (r.currentTime * 1000).toLong()
        r.stop()
        release()
        val data = withContext(Dispatchers.IO) {
            NSData.dataWithContentsOfFile(path).also { NSFileManager.defaultManager.removeItemAtPath(path, null) }
        }
        if (data == null || durationMs < 500) return null
        return Recording(data.toByteArray(), "voice-message.m4a", durationMs)
    }

    override fun cancel() {
        recorder?.let {
            it.stop()
            it.deleteRecording()
        }
        recorder = null
        release()
    }

    /** Gives the session back to playback, so voice messages play on the speaker again. */
    private fun release() {
        if (!tookSession) return
        tookSession = false
        AVAudioSession.sharedInstance().setCategory(AVAudioSessionCategoryPlayback, null)
    }

    override fun level(): Float {
        val r = recorder ?: return 0f
        r.updateMeters()
        val db = r.averagePowerForChannel(0uL)
        return 10.0.pow(db / 20.0).toFloat().coerceIn(0f, 1f)
    }
}

@Composable
actual fun rememberVoiceRecorder(): VoiceRecorder = remember { IosVoiceRecorder() }

@Composable
actual fun rememberCallAccess(): suspend () -> Boolean = rememberMicrophoneAccess()

@Composable
actual fun rememberMicrophoneAccess(): suspend () -> Boolean = remember {
    suspend { suspendCancellableCoroutine<Boolean> { c -> AVAudioSession.sharedInstance().requestRecordPermission { ok -> c.resume(ok) } } }
}

@Composable
actual fun rememberCameraAccess(): suspend () -> Boolean = remember {
    suspend { suspendCancellableCoroutine<Boolean> { c -> AVCaptureDevice.requestAccessForMediaType(AVMediaTypeVideo) { ok -> c.resume(ok) } } }
}

@Composable
actual fun rememberScreenShare(): suspend () -> Boolean {
    val engine = (LocalPlatform.current.rtc as? SwiftRtc)?.native
    return remember(engine) {
        suspend {
            // The broadcast starts in the system's picker, and turns the
            // call's screen on itself once it does.
            engine?.pickBroadcast()
            false
        }
    }
}

private class PickerDelegate(private val onPicked: (NSURL) -> Unit) : NSObject(), UIDocumentPickerDelegateProtocol {
    override fun documentPicker(controller: UIDocumentPickerViewController, didPickDocumentsAtURLs: List<*>) {
        (didPickDocumentsAtURLs.firstOrNull() as? NSURL)?.let(onPicked)
    }
}

@Composable
actual fun rememberFilePicker(onPicked: (PickedFile) -> Unit, onError: (String) -> Unit): () -> Unit {
    val picked by rememberUpdatedState(onPicked)
    val failed by rememberUpdatedState(onError)
    val scope = rememberCoroutineScope()
    // The picker holds its delegate weakly: this keeps it.
    val delegate = remember {
        PickerDelegate { url ->
            scope.launch {
                val data = withContext(Dispatchers.IO) { NSData.dataWithContentsOfURL(url) }
                when {
                    data == null -> failed("Could not read the file.")
                    data.length.toLong() > MAX_PICKED_BYTES -> failed("The file is too large to send.")
                    else -> picked(PickedFile(url.lastPathComponent ?: "file", "application/octet-stream", withContext(Dispatchers.IO) { data.toByteArray() }))
                }
            }
        }
    }
    return {
        val picker = UIDocumentPickerViewController(forOpeningContentTypes = listOf(UTTypeItem), asCopy = true)
        picker.delegate = delegate
        topController()?.presentViewController(picker, animated = true, completion = null)
    }
}

@Composable
actual fun PlatformBackHandler(enabled: Boolean, onBack: () -> Unit) {
    // iOS has no system back button: every screen shows its own.
}
