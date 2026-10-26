package io.github.ssenerg.gochat.ui.channel

import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.AttachFile
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Error
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.TextRange
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.TextFieldValue
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.Attachment
import gochat.v1.ChannelCommand
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.main.LocalToasts
import io.github.ssenerg.gochat.ui.rememberFilePicker
import io.github.ssenerg.gochat.ui.rememberVoiceRecorder
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** A file being attached to the next message. */
private class Upload(val key: Int, val name: String) {
    var attachment by mutableStateOf<Attachment?>(null)
    var error by mutableStateOf<String?>(null)
}

/** The longest voice message, as the server takes by default. */
private const val MAX_RECORDING_MS = 10 * 60_000L

// A command being typed: the message so far is / and its name's start.
private val commandTyped = Regex("^/([a-z0-9_]{0,32})$")

/**
 * Where messages are written: text, files, and voice messages when the
 * channel takes them. [commands] suggests the channel's bots' commands
 * while a message starts with /.
 */
@Composable
fun Composer(
    placeholder: String,
    voiceAllowed: Boolean,
    editing: String?,
    onCancelEdit: () -> Unit,
    onSend: (text: String, attachmentIds: List<String>) -> Unit,
    onTyping: () -> Unit,
    upload: suspend (name: String, bytes: ByteArray, voice: Boolean, durationMs: Long) -> Attachment,
    commands: (suspend (prefix: String) -> List<ChannelCommand>)? = null,
) {
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    var field by remember { mutableStateOf(TextFieldValue("")) }
    val text = field.text
    LaunchedEffect(editing) { field = (editing ?: "").let { TextFieldValue(it, TextRange(it.length)) } }
    var suggestions by remember { mutableStateOf(emptyList<ChannelCommand>()) }
    LaunchedEffect(text, editing) {
        val typed = commandTyped.find(text.lowercase())
        if (commands == null || editing != null || typed == null) {
            suggestions = emptyList()
            return@LaunchedEffect
        }
        delay(150)
        suggestions = commands(typed.groupValues[1])
    }
    val uploads = remember { mutableStateListOf<Upload>() }
    var nextKey by remember { mutableStateOf(0) }

    val pick = rememberFilePicker(
        onPicked = { file ->
            val u = Upload(nextKey++, file.name)
            uploads.add(u)
            scope.launch {
                try {
                    u.attachment = upload(file.name, file.bytes, false, 0)
                } catch (e: Exception) {
                    u.error = e.userMessage()
                }
            }
        },
        onError = { toasts.show(it) },
    )

    val recorder = rememberVoiceRecorder()
    var recording by remember { mutableStateOf(false) }
    var sendingVoice by remember { mutableStateOf(false) }
    DisposableEffect(recorder) { onDispose { recorder.cancel() } }

    fun finishRecording() {
        recording = false
        sendingVoice = true
        scope.launch {
            try {
                val r = recorder.stop()
                if (r == null) {
                    toasts.show("The recording was too short.")
                } else {
                    val att = upload(r.fileName, r.bytes, true, r.durationMs)
                    onSend("", listOf(att.id))
                }
            } catch (e: Exception) {
                toasts.show(e.userMessage())
            }
            sendingVoice = false
        }
    }

    val ready = uploads.none { it.attachment == null && it.error == null }
    val doneIds = uploads.mapNotNull { it.attachment?.id }
    val canSend = ready && (text.isNotBlank() || doneIds.isNotEmpty())

    fun send() {
        if (!canSend) return
        onSend(text, if (editing != null) emptyList() else doneIds)
        field = TextFieldValue("")
        uploads.clear()
    }

    Surface(color = MaterialTheme.colorScheme.background) {
        Column(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 6.dp)) {
            if (editing != null) {
                Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Filled.Edit, contentDescription = null, modifier = Modifier.size(16.dp), tint = MaterialTheme.colorScheme.primary)
                    Spacer(Modifier.width(6.dp))
                    Text("Editing message", color = MaterialTheme.colorScheme.primary, fontSize = 13.sp, modifier = Modifier.weight(1f))
                    IconButton(onClick = onCancelEdit, modifier = Modifier.size(32.dp)) {
                        Icon(Icons.Filled.Close, contentDescription = "Stop editing", modifier = Modifier.size(18.dp))
                    }
                }
            }
            if (suggestions.isNotEmpty()) {
                CommandSuggestions(suggestions) { c ->
                    val typed = "/" + c.command?.name.orEmpty() + " "
                    field = TextFieldValue(typed, TextRange(typed.length))
                    suggestions = emptyList()
                }
            }
            if (uploads.isNotEmpty()) {
                Row(
                    Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(bottom = 6.dp),
                    horizontalArrangement = Arrangement.spacedBy(6.dp),
                ) {
                    uploads.forEach { u -> UploadChip(u) { uploads.remove(u) } }
                }
            }
            if (recording) {
                RecordingBar(
                    level = { recorder.level() },
                    onCancel = {
                        recording = false
                        recorder.cancel()
                    },
                    onSend = { finishRecording() },
                )
            } else {
                Row(verticalAlignment = Alignment.Bottom) {
                    if (editing == null) {
                        IconButton(onClick = pick) { Icon(Icons.Filled.AttachFile, contentDescription = "Attach a file") }
                    }
                    Box(
                        Modifier.weight(1f).heightIn(min = 44.dp).clip(RoundedCornerShape(22.dp))
                            .background(MaterialTheme.colorScheme.surfaceContainerHigh)
                            .padding(horizontal = 16.dp, vertical = 11.dp),
                        contentAlignment = Alignment.CenterStart,
                    ) {
                        if (text.isEmpty()) Text(placeholder, color = LocalChatColors.current.muted, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        BasicTextField(
                            value = field,
                            onValueChange = {
                                field = it
                                if (it.text.isNotBlank() && editing == null) onTyping()
                            },
                            textStyle = MaterialTheme.typography.bodyLarge.copy(color = MaterialTheme.colorScheme.onSurface),
                            cursorBrush = SolidColor(MaterialTheme.colorScheme.primary),
                            keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences),
                            maxLines = 6,
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                    Spacer(Modifier.width(4.dp))
                    when {
                        sendingVoice -> Box(Modifier.size(48.dp), contentAlignment = Alignment.Center) {
                            CircularProgressIndicator(Modifier.size(22.dp), strokeWidth = 2.dp)
                        }
                        voiceAllowed && editing == null && text.isBlank() && uploads.isEmpty() -> IconButton(onClick = {
                            scope.launch {
                                if (recorder.start()) recording = true else toasts.show("GoChat needs the microphone to record. Allow it in the system settings.")
                            }
                        }) { Icon(Icons.Filled.Mic, contentDescription = "Record a voice message") }
                        else -> IconButton(onClick = { send() }, enabled = canSend) {
                            Icon(
                                Icons.AutoMirrored.Filled.Send,
                                contentDescription = if (editing != null) "Save" else "Send",
                                tint = if (canSend) MaterialTheme.colorScheme.primary else LocalChatColors.current.muted,
                            )
                        }
                    }
                }
            }
        }
    }
    if (recording) {
        LaunchedEffect(Unit) {
            delay(MAX_RECORDING_MS)
            if (recording) finishRecording()
        }
    }
}

/** The commands that match what is typed: one tap writes it. */
@Composable
private fun CommandSuggestions(items: List<ChannelCommand>, onPick: (ChannelCommand) -> Unit) {
    Surface(
        shape = RoundedCornerShape(12.dp),
        color = MaterialTheme.colorScheme.surfaceContainerHigh,
        modifier = Modifier.fillMaxWidth().padding(bottom = 6.dp),
    ) {
        Column(Modifier.heightIn(max = 240.dp).verticalScroll(rememberScrollState()).padding(vertical = 4.dp)) {
            items.forEach { c ->
                Row(
                    Modifier.fillMaxWidth().clickable { onPick(c) }.padding(horizontal = 14.dp, vertical = 8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Text("/" + c.command?.name.orEmpty(), fontWeight = FontWeight.SemiBold)
                    Spacer(Modifier.width(10.dp))
                    Text(
                        listOf(c.command?.description.orEmpty(), c.bot.displayName()).filter { it.isNotEmpty() }.joinToString(" · "),
                        color = LocalChatColors.current.muted,
                        fontSize = 13.sp,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f),
                    )
                }
            }
        }
    }
}

@Composable
private fun UploadChip(u: Upload, onRemove: () -> Unit) {
    Surface(shape = RoundedCornerShape(16.dp), color = MaterialTheme.colorScheme.surfaceVariant) {
        Row(Modifier.padding(start = 10.dp, end = 2.dp), verticalAlignment = Alignment.CenterVertically) {
            when {
                u.error != null -> Icon(Icons.Filled.Error, contentDescription = u.error, tint = MaterialTheme.colorScheme.error, modifier = Modifier.size(16.dp))
                u.attachment == null -> CircularProgressIndicator(Modifier.size(14.dp), strokeWidth = 2.dp)
            }
            Spacer(Modifier.width(6.dp))
            Text(u.name, maxLines = 1, overflow = TextOverflow.Ellipsis, fontSize = 13.sp, modifier = Modifier.widthIn(max = 160.dp))
            IconButton(onClick = onRemove, modifier = Modifier.size(32.dp)) {
                Icon(Icons.Filled.Close, contentDescription = "Remove ${u.name}", modifier = Modifier.size(16.dp))
            }
        }
    }
}

@Composable
private fun RecordingBar(level: () -> Float, onCancel: () -> Unit, onSend: () -> Unit) {
    var elapsed by remember { mutableLongStateOf(0L) }
    var loudness by remember { mutableFloatStateOf(0f) }
    LaunchedEffect(Unit) {
        val start = Format.nowMillis()
        while (true) {
            elapsed = Format.nowMillis() - start
            loudness = level().coerceIn(0f, 1f)
            delay(100)
        }
    }
    val blink by rememberInfiniteTransition().animateFloat(
        initialValue = 1f,
        targetValue = 0.2f,
        animationSpec = infiniteRepeatable(tween(700), RepeatMode.Reverse),
    )
    Row(Modifier.fillMaxWidth().height(48.dp), verticalAlignment = Alignment.CenterVertically) {
        IconButton(onClick = onCancel) { Icon(Icons.Filled.Delete, contentDescription = "Discard the recording") }
        Dot(MaterialTheme.colorScheme.error, 10.dp)
        Spacer(Modifier.width(8.dp))
        Text(Format.duration(elapsed), modifier = Modifier.alpha(0.5f + blink / 2))
        Spacer(Modifier.width(12.dp))
        LinearProgressIndicator(progress = { loudness }, modifier = Modifier.weight(1f).height(6.dp).clip(RoundedCornerShape(3.dp)))
        Spacer(Modifier.width(8.dp))
        IconButton(onClick = onSend) {
            Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Send the voice message", tint = MaterialTheme.colorScheme.primary)
        }
    }
}
