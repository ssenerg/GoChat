package io.github.ssenerg.gochat.ui.channel

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Reply
import androidx.compose.material.icons.filled.AddReaction
import androidx.compose.material.icons.filled.Bookmark
import androidx.compose.material.icons.filled.BookmarkBorder
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Flag
import androidx.compose.material.icons.filled.LinkOff
import androidx.compose.material.icons.filled.PushPin
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.Message
import io.github.ssenerg.gochat.core.model.Emojis
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.main.LocalToasts
import io.github.ssenerg.gochat.ui.safety.REPORT_SENT
import io.github.ssenerg.gochat.ui.safety.ReportDialog

/** What can be done with a message: react, reply, copy, edit, remove its preview, delete, pin, save, report. */
@Composable
fun MessageActions(
    m: Message,
    session: ChatSession,
    moderator: Boolean,
    onDismiss: () -> Unit,
    onEdit: () -> Unit,
    onReply: (() -> Unit)?,
) {
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    val toasts = LocalToasts.current
    val platform = LocalPlatform.current
    val st by session.state.collectAsState()
    val mine = m.author?.id == st.me.id
    val saved = m.saved || m.id in st.saved
    var allEmoji by remember { mutableStateOf(false) }
    var confirmDelete by remember { mutableStateOf(false) }
    var reporting by remember { mutableStateOf(false) }

    fun act(block: suspend () -> String?) {
        onDismiss()
        session.launch(block) { err -> err?.let { toasts.show(it) } }
    }

    if (!reporting) ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheet) {
        Column(Modifier.navigationBarsPadding().padding(bottom = 8.dp)) {
            if (allEmoji) {
                LazyVerticalGrid(
                    columns = GridCells.Adaptive(48.dp),
                    modifier = Modifier.fillMaxWidth().heightIn(max = 360.dp).padding(horizontal = 12.dp),
                ) {
                    items(Emojis.all, key = { it.name }) { e ->
                        Box(
                            Modifier.size(48.dp).clip(CircleShape).clickable {
                                onDismiss()
                                session.react(m, e.name)
                            },
                            contentAlignment = Alignment.Center,
                        ) { Text(e.char, fontSize = 26.sp) }
                    }
                }
                return@Column
            }
            Row(
                Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
                horizontalArrangement = Arrangement.SpaceBetween,
            ) {
                Emojis.quick.forEach { e ->
                    Surface(shape = CircleShape, color = MaterialTheme.colorScheme.surfaceVariant) {
                        Box(
                            Modifier.size(46.dp).clickable {
                                onDismiss()
                                session.react(m, e.name)
                            },
                            contentAlignment = Alignment.Center,
                        ) { Text(e.char, fontSize = 22.sp) }
                    }
                }
                Surface(shape = CircleShape, color = MaterialTheme.colorScheme.surfaceVariant) {
                    Box(Modifier.size(46.dp).clickable { allEmoji = true }, contentAlignment = Alignment.Center) {
                        Icon(Icons.Filled.AddReaction, contentDescription = "More reactions")
                    }
                }
            }
            HorizontalDivider(Modifier.padding(vertical = 8.dp))
            if (onReply != null) {
                Action(Icons.AutoMirrored.Filled.Reply, "Reply in thread") {
                    onDismiss()
                    onReply()
                }
            }
            if (m.content.isNotBlank()) {
                Action(Icons.Filled.ContentCopy, "Copy text") {
                    onDismiss()
                    platform.copy(m.content)
                    toasts.show("Copied.")
                }
            }
            if (mine && m.content.isNotBlank()) {
                Action(Icons.Filled.Edit, "Edit") {
                    onDismiss()
                    onEdit()
                }
            }
            if (mine && m.link_preview != null) {
                Action(Icons.Filled.LinkOff, "Remove link preview") {
                    act { session.removeLinkPreview(m) }
                }
            }
            Action(Icons.Filled.PushPin, if (m.pinned) "Unpin" else "Pin to channel") {
                act { session.pin(m, !m.pinned) }
            }
            Action(if (saved) Icons.Filled.Bookmark else Icons.Filled.BookmarkBorder, if (saved) "Remove from saved" else "Save") {
                act { session.save(m, !saved) }
            }
            if (mine || moderator) {
                Action(Icons.Filled.Delete, "Delete", danger = true) { confirmDelete = true }
            }
            if (!mine) {
                Action(Icons.Filled.Flag, "Report message", danger = true) { reporting = true }
            }
        }
    }

    if (reporting) {
        ReportDialog("Report this message", onDismiss = onDismiss) { reason, details ->
            onDismiss()
            session.launch({ session.report(m, reason, details) }) { err -> toasts.show(err ?: REPORT_SENT) }
        }
    }
    if (confirmDelete) {
        AlertDialog(
            onDismissRequest = { confirmDelete = false },
            title = { Text("Delete message?") },
            text = { Text("It will be gone for everyone.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmDelete = false
                    act { session.delete(m) }
                }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { confirmDelete = false }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun Action(icon: ImageVector, text: String, danger: Boolean = false, onClick: () -> Unit) {
    val color = if (danger) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface
    ListItem(
        headlineContent = { Text(text, color = color) },
        leadingContent = { Icon(icon, contentDescription = null, tint = color) },
        colors = ListItemDefaults.colors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
        modifier = Modifier.clickable(onClick = onClick),
    )
}
