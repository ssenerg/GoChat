package io.github.ssenerg.gochat.ui.channel

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.OpenInNew
import androidx.compose.material.icons.filled.AddReaction
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.Error
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.PushPin
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Close
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Slider
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import gochat.v1.Attachment
import gochat.v1.Markup
import gochat.v1.Message
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Emojis
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.mentions
import io.github.ssenerg.gochat.core.model.sentAtMillis
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.Pending
import io.github.ssenerg.gochat.ui.Avatar
import io.github.ssenerg.gochat.ui.BotBadge
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.LocalImages
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.MessageText
import io.github.ssenerg.gochat.ui.ServerImage
import io.github.ssenerg.gochat.ui.main.LocalToasts
import kotlinx.coroutines.launch

/** One message as a channel or a thread shows it. */
@Composable
fun MessageRow(
    m: Message,
    me: User,
    session: ChatSession,
    continues: Boolean,
    onLongPress: () -> Unit,
    onOpenThread: (() -> Unit)?,
) {
    val colors = LocalChatColors.current
    val highlight = m.author?.id != me.id && m.mentions(me)
    Row(
        Modifier.fillMaxWidth()
            .background(if (highlight) colors.mention.copy(alpha = 0.6f) else androidx.compose.ui.graphics.Color.Transparent)
            .combinedClickable(onClick = {}, onLongClick = onLongPress)
            .padding(start = 12.dp, end = 12.dp, top = if (continues) 2.dp else 10.dp, bottom = 2.dp),
    ) {
        if (continues) Spacer(Modifier.width(40.dp)) else Avatar(m.author, size = 40.dp)
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f)) {
            if (!continues) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        m.author.displayName(),
                        fontWeight = FontWeight.SemiBold,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f, fill = false),
                    )
                    if (m.author?.bot == true) {
                        Spacer(Modifier.width(6.dp))
                        BotBadge()
                    }
                    Spacer(Modifier.width(8.dp))
                    Text(Format.clock(m.sentAtMillis()), color = colors.muted, fontSize = 12.sp)
                    if (m.pinned) {
                        Spacer(Modifier.width(6.dp))
                        Icon(Icons.Filled.PushPin, contentDescription = "Pinned", tint = colors.muted, modifier = Modifier.size(14.dp))
                    }
                }
            }
            if (m.content.isNotBlank()) MessageText(m.content)
            if (m.edited_at != null) Text("(edited)", color = colors.muted, fontSize = 11.sp)
            m.attachments.forEach { att -> AttachmentView(att, session) }
            m.markup?.takeIf { it.rows.isNotEmpty() }?.let { BotButtons(m, it, session) }
            if (m.reactions.isNotEmpty()) Reactions(m, me, session, onMore = onLongPress)
            if (m.reply_count > 0 && onOpenThread != null) {
                Text(
                    if (m.reply_count == 1) "1 reply" else "${m.reply_count} replies",
                    color = MaterialTheme.colorScheme.primary,
                    fontWeight = FontWeight.SemiBold,
                    fontSize = 13.sp,
                    modifier = Modifier.padding(top = 4.dp).clip(RoundedCornerShape(6.dp)).clickable(onClick = onOpenThread)
                        .padding(horizontal = 4.dp, vertical = 2.dp),
                )
            }
        }
    }
}

/**
 * The buttons a bot put under its message: one with data tells the bot,
 * whose answer the session shows; one with a link opens it, once the
 * person saw where it leads.
 */
@Composable
private fun BotButtons(m: Message, markup: Markup, session: ChatSession) {
    val toasts = LocalToasts.current
    var pressing by remember(m.id) { mutableStateOf<String?>(null) }
    var link by remember { mutableStateOf<String?>(null) }
    Column(Modifier.padding(top = 6.dp).widthIn(max = 420.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
        markup.rows.forEach { row ->
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                row.buttons.forEach { b ->
                    val data = b.callback_data
                    val url = b.url
                    OutlinedButton(
                        onClick = {
                            when {
                                url != null -> link = url
                                data != null -> {
                                    pressing = data
                                    session.launch({ session.press(m, data) }) { err ->
                                        pressing = null
                                        err?.let { toasts.show(it) }
                                    }
                                }
                            }
                        },
                        enabled = url != null || (data != null && pressing == null),
                        shape = RoundedCornerShape(10.dp),
                        contentPadding = PaddingValues(horizontal = 10.dp, vertical = 4.dp),
                        modifier = Modifier.weight(1f).heightIn(min = 36.dp),
                    ) {
                        if (data != null && pressing == data) {
                            CircularProgressIndicator(Modifier.size(14.dp), strokeWidth = 2.dp)
                            Spacer(Modifier.width(6.dp))
                        }
                        Text(b.text, maxLines = 1, overflow = TextOverflow.Ellipsis, fontSize = 13.sp)
                        if (url != null) {
                            Spacer(Modifier.width(4.dp))
                            Icon(Icons.AutoMirrored.Filled.OpenInNew, contentDescription = "Opens a link", modifier = Modifier.size(14.dp))
                        }
                    }
                }
            }
        }
    }
    link?.let { url -> OpenLinkDialog(url, onDismiss = { link = null }) }
}

/** Asks before opening a bot's link, whose button may name it as anything. */
@Composable
private fun OpenLinkDialog(url: String, onDismiss: () -> Unit) {
    val uri = LocalUriHandler.current
    val toasts = LocalToasts.current
    val safe = url.startsWith("https://") || url.startsWith("http://")
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Open this link?") },
        text = { Text(if (safe) url else "This link cannot be opened: $url") },
        confirmButton = {
            if (safe) {
                TextButton(onClick = {
                    onDismiss()
                    runCatching { uri.openUri(url) }.onFailure { toasts.show("No app here opens this link.") }
                }) { Text("Open") }
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

/** A message on its way. */
@Composable
fun PendingRow(p: Pending, me: User, onRetry: () -> Unit, onDiscard: () -> Unit) {
    val colors = LocalChatColors.current
    Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Avatar(me, size = 40.dp)
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f)) {
            Text(
                p.content.ifEmpty { if (p.attachmentIds.size == 1) "1 file" else "${p.attachmentIds.size} files" },
                color = colors.muted,
            )
            Text(if (p.failed) "Not sent" else "Sending…", color = if (p.failed) MaterialTheme.colorScheme.error else colors.muted, fontSize = 12.sp)
        }
        if (p.failed) {
            IconButton(onClick = onRetry) { Icon(Icons.Filled.Refresh, contentDescription = "Send again") }
            IconButton(onClick = onDiscard) { Icon(Icons.Filled.Close, contentDescription = "Discard") }
        }
    }
}

/** The line between two days. */
@Composable
fun DaySeparator(ms: Long) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
        HorizontalDivider(Modifier.weight(1f))
        Text(
            Format.day(ms, Format.nowMillis()),
            color = LocalChatColors.current.muted,
            fontSize = 12.sp,
            fontWeight = FontWeight.SemiBold,
            modifier = Modifier.padding(horizontal = 10.dp),
        )
        HorizontalDivider(Modifier.weight(1f))
    }
}

@Composable
private fun Reactions(m: Message, me: User, session: ChatSession, onMore: () -> Unit) {
    FlowRow(
        Modifier.padding(top = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(6.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        m.reactions.forEach { r ->
            val char = Emojis.char(r.emoji) ?: return@forEach
            val mine = me.id in r.user_ids
            val shape = RoundedCornerShape(12.dp)
            Row(
                Modifier.clip(shape)
                    .background(if (mine) MaterialTheme.colorScheme.secondaryContainer else MaterialTheme.colorScheme.surfaceVariant)
                    .border(1.dp, if (mine) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant, shape)
                    .clickable { session.react(m, r.emoji) }
                    .padding(horizontal = 8.dp, vertical = 3.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(char, fontSize = 15.sp)
                Spacer(Modifier.width(4.dp))
                Text(r.user_ids.size.toString(), fontSize = 13.sp, fontWeight = if (mine) FontWeight.SemiBold else FontWeight.Normal)
            }
        }
        Box(
            Modifier.clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.surfaceVariant)
                .clickable(onClick = onMore).padding(horizontal = 8.dp, vertical = 3.dp),
        ) {
            Icon(Icons.Filled.AddReaction, contentDescription = "Add a reaction", modifier = Modifier.size(18.dp))
        }
    }
}

private fun isImage(att: Attachment) = !att.voice && att.content_type.startsWith("image/")

@Composable
private fun AttachmentView(att: Attachment, session: ChatSession) {
    when {
        att.voice -> VoicePlayer(att, session)
        isImage(att) -> ImageAttachment(att)
        else -> FileAttachment(att, session)
    }
}

@Composable
private fun ImageAttachment(att: Attachment) {
    val url = LocalImages.current?.api?.fileUrl(att.id) ?: return
    var open by remember { mutableStateOf(false) }
    ServerImage(
        url,
        contentDescription = att.name,
        modifier = Modifier.padding(top = 6.dp).widthIn(max = 320.dp).heightIn(max = 260.dp)
            .clip(RoundedCornerShape(10.dp)).clickable { open = true },
        contentScale = ContentScale.Fit,
    )
    if (open) {
        Dialog(onDismissRequest = { open = false }) {
            Box(Modifier.clickable { open = false }) {
                ServerImage(url, contentDescription = att.name, modifier = Modifier.fillMaxWidth(), contentScale = ContentScale.Fit)
            }
        }
    }
}

@Composable
private fun FileAttachment(att: Attachment, session: ChatSession) {
    val platform = LocalPlatform.current
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    var busy by remember { mutableStateOf(false) }
    Surface(
        shape = RoundedCornerShape(10.dp),
        color = MaterialTheme.colorScheme.surfaceVariant,
        modifier = Modifier.padding(top = 6.dp).widthIn(max = 320.dp).clip(RoundedCornerShape(10.dp)).clickable(enabled = !busy) {
            busy = true
            scope.launch {
                try {
                    platform.openFile(att.name, att.content_type, session.api.client.download(att.id))
                } catch (e: Exception) {
                    toasts.show(e.userMessage())
                }
                busy = false
            }
        },
    ) {
        Row(Modifier.padding(10.dp), verticalAlignment = Alignment.CenterVertically) {
            if (busy) CircularProgressIndicator(Modifier.size(24.dp), strokeWidth = 2.dp) else Icon(Icons.Filled.Description, contentDescription = null)
            Spacer(Modifier.width(10.dp))
            Column {
                Text(att.name, maxLines = 1, overflow = TextOverflow.Ellipsis, fontWeight = FontWeight.Medium)
                Text(Format.size(att.size), color = LocalChatColors.current.muted, fontSize = 12.sp)
            }
        }
    }
}

@Composable
private fun VoicePlayer(att: Attachment, session: ChatSession) {
    val platform = LocalPlatform.current
    val player = platform.player
    val ps by player.state.collectAsState()
    val scope = rememberCoroutineScope()
    val toasts = LocalToasts.current
    var loading by remember { mutableStateOf(false) }
    val mine = ps.id == att.id
    val duration = if (mine && ps.durationMs > 0) ps.durationMs else att.duration_ms.toLong()
    val position = if (mine) ps.positionMs else 0L
    Surface(
        shape = RoundedCornerShape(22.dp),
        color = MaterialTheme.colorScheme.secondaryContainer,
        modifier = Modifier.padding(top = 6.dp).widthIn(min = 220.dp, max = 320.dp),
    ) {
        Row(Modifier.padding(horizontal = 6.dp, vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = {
                when {
                    mine && ps.playing -> player.pause()
                    mine && ps.error == null && ps.positionMs > 0 -> player.resume()
                    else -> {
                        val cached = session.voiceCache[att.id]
                        if (cached != null) {
                            player.play(att.id, cached, att.content_type)
                        } else {
                            loading = true
                            scope.launch {
                                try {
                                    val bytes = session.api.client.download(att.id)
                                    session.voiceCache[att.id] = bytes
                                    player.play(att.id, bytes, att.content_type)
                                } catch (e: Exception) {
                                    toasts.show(e.userMessage())
                                }
                                loading = false
                            }
                        }
                    }
                }
            }) {
                when {
                    loading -> CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                    mine && ps.error != null -> Icon(Icons.Filled.Error, contentDescription = ps.error)
                    mine && ps.playing -> Icon(Icons.Filled.Pause, contentDescription = "Pause")
                    else -> Icon(Icons.Filled.PlayArrow, contentDescription = "Play voice message")
                }
            }
            Slider(
                value = if (duration > 0) (position.toFloat() / duration).coerceIn(0f, 1f) else 0f,
                onValueChange = { if (mine && duration > 0) player.seek((it * duration).toLong()) },
                enabled = mine && ps.error == null,
                modifier = Modifier.weight(1f),
            )
            Spacer(Modifier.width(6.dp))
            Text(
                Format.duration(if (mine && position > 0) position else duration),
                fontSize = 12.sp,
                color = MaterialTheme.colorScheme.onSecondaryContainer,
                modifier = Modifier.padding(end = 8.dp),
            )
        }
    }
    if (mine && ps.error != null) {
        Text(ps.error ?: "", color = MaterialTheme.colorScheme.error, fontSize = 12.sp)
    }
}

/** A dot for a presence or a recording. */
@Composable
fun Dot(color: androidx.compose.ui.graphics.Color, size: androidx.compose.ui.unit.Dp = 8.dp) {
    Box(Modifier.size(size).clip(CircleShape).background(color))
}
