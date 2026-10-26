package io.github.ssenerg.gochat.ui.thread

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.NotificationsOff
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.Message
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.isArchived
import io.github.ssenerg.gochat.core.model.isConversation
import io.github.ssenerg.gochat.core.model.moderatedBy
import io.github.ssenerg.gochat.core.model.title
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.channel.Composer
import io.github.ssenerg.gochat.ui.channel.MessageActions
import io.github.ssenerg.gochat.ui.channel.MessageRow
import io.github.ssenerg.gochat.ui.channel.Notice
import io.github.ssenerg.gochat.ui.channel.PendingRow
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import kotlinx.coroutines.launch

/** A thread: its root, its replies and a composer that replies. */
@Composable
fun ThreadScreen(session: ChatSession, channelId: String, rootId: String) {
    val thread = remember(rootId) { session.thread(channelId, rootId) }
    val st by thread.state.collectAsState()
    val chat by session.state.collectAsState()
    val channel = chat.channel(channelId)
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val moderator = channel?.moderatedBy(chat.me, chat.team) == true
    var selected by remember { mutableStateOf<Message?>(null) }
    var editing by remember { mutableStateOf<Message?>(null) }
    val list = rememberLazyListState()

    LaunchedEffect(st.gone) {
        if (st.gone) {
            toasts.show("The thread's first message was deleted.")
            nav.pop()
        }
    }
    LaunchedEffect(st.error) {
        val e = st.error
        if (e != null && st.root != null) {
            toasts.show(e)
            thread.clearError()
        }
    }
    LaunchedEffect(st.replies.lastOrNull()?.id, st.pending.size) {
        if (list.firstVisibleItemIndex <= 2) list.animateScrollToItem(0)
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("Thread", style = MaterialTheme.typography.titleMedium)
                        if (channel != null) {
                            Text(
                                if (channel.isConversation) channel.title() else "#${channel.title()}",
                                fontSize = 12.sp,
                                color = LocalChatColors.current.muted,
                            )
                        }
                    }
                },
                navigationIcon = { BackButton { nav.pop() } },
                actions = {
                    IconButton(onClick = {
                        session.launch({ session.follow(channelId, rootId, !st.following) }) { err -> err?.let { toasts.show(it) } }
                    }) {
                        Icon(
                            if (st.following) Icons.Filled.Notifications else Icons.Filled.NotificationsOff,
                            contentDescription = if (st.following) "Stop following" else "Follow the thread",
                        )
                    }
                },
            )
        },
    ) { padding ->
        Column(Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding).imePadding()) {
            Box(Modifier.weight(1f)) {
                val root = st.root
                when {
                    st.loading -> Loading()
                    root == null -> ErrorState(st.error ?: "The thread could not load.", onRetry = { session.refresh() })
                    else -> LazyColumn(
                        state = list,
                        reverseLayout = true,
                        modifier = Modifier.fillMaxSize(),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        items(st.pending.reversed(), key = { it.localId }) { p ->
                            PendingRow(p, chat.me, onRetry = { thread.retry(p) }, onDiscard = { thread.discard(p) })
                        }
                        val replies = st.replies.asReversed()
                        itemsIndexed(replies, key = { _, m -> m.id }) { i, m ->
                            MessageRow(
                                m, chat.me, session,
                                continues = Format.continues(m, replies.getOrNull(i + 1)),
                                onLongPress = { selected = m },
                                onOpenThread = null,
                            )
                        }
                        if (st.hasMore) {
                            item(key = "older") {
                                TextButton(onClick = { thread.loadOlder() }, modifier = Modifier.fillMaxWidth()) {
                                    Text("Show earlier replies")
                                }
                            }
                        }
                        item(key = "root") {
                            Column {
                                MessageRow(root, chat.me, session, continues = false, onLongPress = { selected = root }, onOpenThread = null)
                                Text(
                                    when (st.replies.size) {
                                        0 -> "No replies yet"
                                        1 -> "1 reply"
                                        else -> "${st.replies.size} replies"
                                    },
                                    fontWeight = FontWeight.SemiBold,
                                    fontSize = 13.sp,
                                    color = LocalChatColors.current.muted,
                                    modifier = Modifier.padding(start = 16.dp, top = 12.dp, bottom = 4.dp),
                                )
                                HorizontalDivider()
                            }
                        }
                    }
                }
            }
            if (channel != null && channel.isArchived) {
                Notice("This channel is archived: you can read it, not write in it.")
            } else {
                Composer(
                    placeholder = "Reply…",
                    voiceAllowed = channel?.voice_enabled == true || channel?.isConversation == true,
                    editing = editing?.content,
                    onCancelEdit = { editing = null },
                    onSend = { text, ids ->
                        val e = editing
                        if (e != null) {
                            editing = null
                            session.launch({ session.edit(e, text) }) { err -> err?.let { toasts.show(it) } }
                        } else {
                            thread.send(text, ids)
                        }
                    },
                    onTyping = { session.typing(channelId) },
                    upload = { name, bytes, voice, duration -> session.upload(channelId, name, bytes, voice, duration) },
                    commands = { prefix -> session.commands(channelId, prefix) },
                )
            }
        }
    }

    selected?.let { m ->
        MessageActions(m, session, moderator, onDismiss = { selected = null }, onEdit = { editing = m }, onReply = null)
    }
}
