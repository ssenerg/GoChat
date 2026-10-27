package io.github.ssenerg.gochat.ui

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.media.AudioAttributes
import android.media.MediaPlayer
import android.media.MediaRecorder
import android.media.projection.MediaProjectionManager
import android.net.Uri
import android.os.Build
import android.os.SystemClock
import android.provider.OpenableColumns
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.FileProvider
import io.github.ssenerg.gochat.core.api.AppInfo
import io.github.ssenerg.gochat.core.state.KeyValueStore
import io.github.ssenerg.gochat.core.voice.RtcEngine
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File

/** Android's side of the app; the Application keeps one. */
class AndroidPlatform(private val context: Context, private val scope: CoroutineScope) : Platform {
    override val app = AppInfo(
        version = runCatching { context.packageManager.getPackageInfo(context.packageName, 0).versionName }.getOrNull() ?: "0",
        device = "Android ${Build.VERSION.RELEASE}; ${Build.MODEL}",
    )
    override val store: KeyValueStore = SealedPreferences(context)
    override val player: AudioPlayer = AndroidAudioPlayer(context, scope)
    override val rtc: RtcEngine = AndroidRtcEngine(context)
    override val foreground: StateFlow<Boolean> = Foreground(context.applicationContext as android.app.Application).shown

    override fun openFile(name: String, mimeType: String, bytes: ByteArray) {
        // Keep only the file's own name: it is chosen by whoever sent it.
        val safe = name.substringAfterLast('/').substringAfterLast('\\').ifBlank { "file" }
        scope.launch {
            val file = try {
                withContext(Dispatchers.IO) {
                    File(File(context.cacheDir, "shared").apply { mkdirs() }, safe).apply { writeBytes(bytes) }
                }
            } catch (_: Exception) {
                return@launch
            }
            view(file, safe, mimeType)
        }
    }

    private fun view(file: File, safe: String, mimeType: String) {
        val uri = FileProvider.getUriForFile(context, context.packageName + ".gochat.files", file)
        val view = Intent(Intent.ACTION_VIEW).setDataAndType(uri, mimeType.ifBlank { "application/octet-stream" })
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        val chooser = Intent.createChooser(view, safe).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        try {
            context.startActivity(chooser)
        } catch (_: ActivityNotFoundException) {
            val send = Intent(Intent.ACTION_SEND).setType(mimeType).putExtra(Intent.EXTRA_STREAM, uri)
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            context.startActivity(Intent.createChooser(send, safe).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        }
    }

    override fun share(text: String) {
        val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
        context.startActivity(Intent.createChooser(send, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
    }

    override fun copy(text: String) {
        val clipboard = context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
        clipboard.setPrimaryClip(ClipData.newPlainText("GoChat", text))
    }
}

private class AndroidAudioPlayer(private val context: Context, private val scope: CoroutineScope) : AudioPlayer {
    private val _state = MutableStateFlow(PlayerState())
    override val state: StateFlow<PlayerState> = _state.asStateFlow()
    private var player: MediaPlayer? = null
    private var prepared = false
    private var file: File? = null
    private var loading: Job? = null
    private var ticker: Job? = null

    override fun play(id: String, bytes: ByteArray, mimeType: String) {
        stop()
        val ext = when {
            "webm" in mimeType -> "webm"
            "ogg" in mimeType -> "ogg"
            else -> "m4a"
        }
        val p = MediaPlayer()
        player = p
        _state.value = PlayerState(id = id, playing = true)
        fun failed() {
            if (player !== p) return
            stop()
            _state.value = PlayerState(id = id, error = "This voice message cannot play on this device.")
        }
        // The file is written, and the player prepared, off the main thread.
        loading = scope.launch {
            val f = try {
                withContext(Dispatchers.IO) { File.createTempFile("voice-play", ".$ext", context.cacheDir).apply { writeBytes(bytes) } }
            } catch (_: Exception) {
                failed()
                return@launch
            }
            if (player !== p) {
                f.delete()
                return@launch
            }
            file = f
            p.setAudioAttributes(
                AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_MEDIA).setContentType(AudioAttributes.CONTENT_TYPE_SPEECH).build(),
            )
            p.setOnPreparedListener {
                if (player !== p) return@setOnPreparedListener
                prepared = true
                _state.value = _state.value.copy(durationMs = p.duration.toLong().coerceAtLeast(0))
                // Paused while it prepared: it waits for resume.
                if (_state.value.playing) {
                    p.start()
                    tick()
                }
            }
            p.setOnErrorListener { _, _, _ ->
                failed()
                true
            }
            p.setOnCompletionListener {
                ticker?.cancel()
                _state.value = _state.value.copy(playing = false, positionMs = 0)
            }
            try {
                p.setDataSource(f.absolutePath)
                p.prepareAsync()
            } catch (_: Exception) {
                failed()
            }
        }
    }

    private fun tick() {
        ticker?.cancel()
        ticker = scope.launch {
            while (isActive) {
                val p = player ?: break
                if (p.isPlaying) _state.value = _state.value.copy(positionMs = p.currentPosition.toLong())
                delay(200)
            }
        }
    }

    override fun pause() {
        player?.takeIf { prepared && it.isPlaying }?.pause()
        ticker?.cancel()
        _state.value = _state.value.copy(playing = false)
    }

    override fun resume() {
        val p = player ?: return
        _state.value = _state.value.copy(playing = true)
        if (prepared) {
            p.start()
            tick()
        }
    }

    override fun seek(positionMs: Long) {
        if (prepared) player?.seekTo(positionMs.toInt())
        _state.value = _state.value.copy(positionMs = positionMs)
    }

    override fun stop() {
        loading?.cancel()
        ticker?.cancel()
        player?.release()
        player = null
        prepared = false
        file?.delete()
        file = null
        _state.value = PlayerState()
    }
}

private class AndroidVoiceRecorder(
    private val context: Context,
    private val askPermission: suspend () -> Boolean,
) : VoiceRecorder {
    private var recorder: MediaRecorder? = null
    private var file: File? = null
    private var startedAt = 0L

    override suspend fun start(): Boolean {
        if (!askPermission()) return false
        cancel()
        val f = File(context.cacheDir, "voice-${System.currentTimeMillis()}.m4a")
        val r = if (Build.VERSION.SDK_INT >= 31) MediaRecorder(context) else @Suppress("DEPRECATION") MediaRecorder()
        return try {
            // AAC in MP4: the server takes it, and every platform plays it.
            r.setAudioSource(MediaRecorder.AudioSource.MIC)
            r.setOutputFormat(MediaRecorder.OutputFormat.MPEG_4)
            r.setAudioEncoder(MediaRecorder.AudioEncoder.AAC)
            r.setAudioEncodingBitRate(64_000)
            r.setAudioSamplingRate(44_100)
            r.setAudioChannels(1)
            r.setOutputFile(f.absolutePath)
            r.prepare()
            r.start()
            recorder = r
            file = f
            startedAt = SystemClock.elapsedRealtime()
            true
        } catch (_: Exception) {
            r.release()
            f.delete()
            false
        }
    }

    override suspend fun stop(): Recording? {
        val r = recorder ?: return null
        val f = file
        recorder = null
        file = null
        val duration = SystemClock.elapsedRealtime() - startedAt
        // Stopping right after starting throws: there is nothing recorded.
        val ok = runCatching { r.stop() }.isSuccess
        r.release()
        if (!ok || f == null || duration < 500) {
            f?.delete()
            return null
        }
        val bytes = withContext(Dispatchers.IO) { f.readBytes() }
        f.delete()
        return Recording(bytes, "voice-message.m4a", duration)
    }

    override fun cancel() {
        recorder?.let {
            runCatching { it.stop() }
            it.release()
        }
        recorder = null
        file?.delete()
        file = null
    }

    override fun level(): Float = recorder?.let { r -> runCatching { r.maxAmplitude / 32767f }.getOrNull() } ?: 0f
}

@Composable
actual fun rememberVoiceRecorder(): VoiceRecorder {
    val context = LocalContext.current
    val microphone = rememberMicrophoneAccess()
    return remember(context) { AndroidVoiceRecorder(context.applicationContext, microphone) }
}

@Composable
actual fun rememberMicrophoneAccess(): suspend () -> Boolean {
    val context = LocalContext.current
    val pending = remember { arrayOfNulls<CompletableDeferred<Boolean>>(1) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        pending[0]?.complete(granted)
        pending[0] = null
    }
    val currentLauncher by rememberUpdatedState(launcher)
    return remember(context) {
        suspend {
            context.checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED || run {
                val d = CompletableDeferred<Boolean>()
                pending[0] = d
                currentLauncher.launch(Manifest.permission.RECORD_AUDIO)
                d.await()
            }
        }
    }
}

@Composable
actual fun rememberCallAccess(): suspend () -> Boolean {
    val context = LocalContext.current
    val pending = remember { arrayOfNulls<CompletableDeferred<Boolean>>(1) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { results ->
        val microphone = results[Manifest.permission.RECORD_AUDIO]
            ?: (context.checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED)
        pending[0]?.complete(microphone)
        pending[0] = null
    }
    val currentLauncher by rememberUpdatedState(launcher)
    return remember(context) {
        suspend {
            // The call's notice shows only with this permission, from Android 13.
            val wanted = listOfNotNull(
                Manifest.permission.RECORD_AUDIO,
                if (Build.VERSION.SDK_INT >= 33) Manifest.permission.POST_NOTIFICATIONS else null,
            ).filter { context.checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED }
            if (wanted.isEmpty()) {
                true
            } else {
                val d = CompletableDeferred<Boolean>()
                pending[0] = d
                currentLauncher.launch(wanted.toTypedArray())
                d.await()
            }
        }
    }
}

@Composable
actual fun rememberCameraAccess(): suspend () -> Boolean {
    val context = LocalContext.current
    val pending = remember { arrayOfNulls<CompletableDeferred<Boolean>>(1) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        pending[0]?.complete(granted)
        pending[0] = null
    }
    val currentLauncher by rememberUpdatedState(launcher)
    return remember(context) {
        suspend {
            context.checkSelfPermission(Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED || run {
                val d = CompletableDeferred<Boolean>()
                pending[0] = d
                currentLauncher.launch(Manifest.permission.CAMERA)
                d.await()
            }
        }
    }
}

@Composable
actual fun rememberScreenShare(): suspend () -> Boolean {
    val context = LocalContext.current
    val engine = LocalPlatform.current.rtc as? AndroidRtcEngine
    val pending = remember { arrayOfNulls<CompletableDeferred<Intent?>>(1) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        pending[0]?.complete(result.data.takeIf { result.resultCode == Activity.RESULT_OK })
        pending[0] = null
    }
    val currentLauncher by rememberUpdatedState(launcher)
    return remember(context, engine) {
        suspend {
            val projection = context.getSystemService(MediaProjectionManager::class.java)
            if (engine == null || projection == null) {
                false
            } else {
                val d = CompletableDeferred<Intent?>()
                pending[0] = d
                currentLauncher.launch(projection.createScreenCaptureIntent())
                // Android lets a capture start only once the call's service
                // runs for the screen, which it lets only after the consent.
                val consent = d.await()
                if (consent != null && VoiceCallService.allowScreen(context)) {
                    engine.grantScreen(consent)
                    true
                } else {
                    false
                }
            }
        }
    }
}

@Composable
actual fun rememberFilePicker(onPicked: (PickedFile) -> Unit, onError: (String) -> Unit): () -> Unit {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val picked by rememberUpdatedState(onPicked)
    val failed by rememberUpdatedState(onError)
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri: Uri? ->
        if (uri != null) {
            scope.launch {
                try {
                    picked(readPicked(context, uri))
                } catch (e: Exception) {
                    failed(e.message ?: "Could not read the file.")
                }
            }
        }
    }
    return { launcher.launch("*/*") }
}

private suspend fun readPicked(context: Context, uri: Uri): PickedFile = withContext(Dispatchers.IO) {
    val resolver = context.contentResolver
    var name = "file"
    var size = -1L
    resolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)?.use { c ->
        if (c.moveToFirst()) {
            c.getString(0)?.let { name = it }
            if (!c.isNull(1)) size = c.getLong(1)
        }
    }
    if (size > MAX_PICKED_BYTES) throw IllegalArgumentException("The file is too large to send.")
    val bytes = resolver.openInputStream(uri)?.use { it.readBytes() } ?: throw IllegalStateException("Could not read the file.")
    PickedFile(name, resolver.getType(uri) ?: "application/octet-stream", bytes)
}

@Composable
actual fun PlatformBackHandler(enabled: Boolean, onBack: () -> Unit) {
    BackHandler(enabled = enabled, onBack = onBack)
}
