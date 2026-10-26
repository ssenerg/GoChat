package io.github.ssenerg.gochat.ui.channel

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.PushPin
import androidx.compose.material.icons.filled.Tag
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.Channel
import gochat.v1.Message
import gochat.v1.User
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.isArchived
import io.github.ssenerg.gochat.core.model.isConversation
import io.github.ssenerg.gochat.core.model.isPrivate
import io.github.ssenerg.gochat.core.model.moderatedBy
import io.github.ssenerg.gochat.core.model.sentAtMillis
import io.github.ssenerg.gochat.core.model.title
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.ChatState
import io.github.ssenerg.gochat.ui.AvatarWithStatus
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.BotBadge
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import io.github.ssenerg.gochat.ui.main.Route
import io.github.ssenerg.gochat.ui.statusLabel
import kotlinx.coroutines.launch

@Composable
fun ChannelScreen(session: ChatSession, channelId: String) {
    val timeline = remember(channelId) { session.timeline(channelId) }
    val st by timeline.state.collectAsState()
    val chat by session.state.collectAsState()
    val channel = chat.channel(channelId) ?: st.channel
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val moderator = channel?.moderatedBy(chat.me, chat.team) == true
    var selected by remember { mutableStateOf<Message?>(null) }
    var editing by remember { mutableStateOf<Message?>(null) }
    val list = rememberLazyListState()

    LaunchedEffect(st.gone) {
        if (st.gone) {
            if (st.removed) toasts.show("You are no longer a member of this channel.")
            nav.closeChannel(channelId)
        }
    }
    LaunchedEffect(st.error) {
        val e = st.error
        if (e != null && st.messages.isNotEmpty()) {
            toasts.show(e)
            timeline.clearError()
        }
    }
    // Follow the newest message while the person reads the bottom.
    val newest = st.messages.firstOrNull()?.id
    LaunchedEffect(newest, st.pending.size) {
        if (list.firstVisibleItemIndex <= 2) list.animateScrollToItem(0)
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { ChannelTitle(channel, chat) },
                navigationIcon = { BackButton { nav.pop() } },
                actions = {
                    if (channel != null && channel.pinned_count > 0) {
                        IconButton(onClick = { nav.open(Route.Pins(channelId)) }) {
                            BadgedBox(badge = { Badge { Text(channel.pinned_count.toString()) } }) {
                                Icon(Icons.Filled.PushPin, contentDescription = "Pinned messages")
                            }
                        }
                    }
                    IconButton(onClick = { nav.open(Route.ChannelInfo(channelId)) }) {
                        Icon(Icons.Filled.Info, contentDescription = "About the channel")
                    }
                },
            )
        },
    ) { padding ->
        Column(Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding).imePadding()) {
            Box(Modifier.weight(1f)) {
                when {
                    st.loading -> Loading()
                    st.error != null && st.messages.isEmpty() -> ErrorState(st.error ?: "", onRetry = { session.refresh() })
                    else -> LazyColumn(
                        state = list,
                        reverseLayout = true,
                        modifier = Modifier.fillMaxSize(),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        items(st.pending.reversed(), key = { it.localId }) { p ->
                            PendingRow(p, chat.me, onRetry = { timeline.retry(p) }, onDiscard = { timeline.discard(p) })
                        }
                        itemsIndexed(st.messages, key = { _, m -> m.id }) { i, m ->
                            val older = st.messages.getOrNull(i + 1)
                            Column {
                                if (older == null || !Format.sameDay(older.sentAtMillis(), m.sentAtMillis())) DaySeparator(m.sentAtMillis())
                                MessageRow(
                                    m, chat.me, session,
                                    continues = Format.continues(m, older),
                                    onLongPress = { selected = m },
                                    onOpenThread = { nav.open(Route.Thread(channelId, m.id)) },
                                )
                            }
                        }
                        item(key = "start") {
                            if (st.hasMore) {
                                LaunchedEffect(st.messages.lastOrNull()?.id) { timeline.loadOlder() }
                                Box(Modifier.fillMaxWidth().padding(16.dp), contentAlignment = Alignment.Center) {
                                    CircularProgressIndicator(Modifier.size(24.dp), strokeWidth = 2.dp)
                                }
                            } else if (channel != null) {
                                ChannelStart(channel)
                            }
                        }
                    }
                }
            }
            TypingLine(st.typing)
            when {
                channel != null && channel.isArchived -> Notice("This channel is archived: you can read it, not write in it.")
                else -> Composer(
                    placeholder = channel?.let { if (it.isConversation) "Message ${it.title()}" else "Message #${it.title()}" } ?: "Message",
                    voiceAllowed = channel?.voice_enabled == true || channel?.isConversation == true,
                    editing = editing?.content,
                    onCancelEdit = { editing = null },
                    onSend = { text, ids ->
                        val e = editing
                        if (e != null) {
                            editing = null
                            session.launch({ session.edit(e, text) }) { err -> err?.let { toasts.show(it) } }
                        } else {
                            timeline.send(text, ids)
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
        MessageActions(
            m, session, moderator,
            onDismiss = { selected = null },
            onEdit = { editing = m },
            onReply = { nav.open(Route.Thread(channelId, m.id)) },
        )
    }
}

@Composable
private fun ChannelTitle(channel: Channel?, chat: ChatState) {
    if (channel == null) {
        Text("")
        return
    }
    Row(verticalAlignment = Alignment.CenterVertically) {
        val peer = channel.peer
        if (peer != null) {
            AvatarWithStatus(peer, chat.statusOf(peer.id), size = 32.dp)
            Spacer(Modifier.width(10.dp))
        } else if (!channel.isConversation) {
            Icon(if (channel.isPrivate) Icons.Filled.Lock else Icons.Filled.Tag, contentDescription = null, modifier = Modifier.size(20.dp))
            Spacer(Modifier.width(6.dp))
        }
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    channel.title(),
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.weight(1f, fill = false),
                )
                if (peer?.bot == true) {
                    Spacer(Modifier.width(6.dp))
                    BotBadge()
                }
            }
            val sub = when {
                peer != null -> statusLabel(chat.statusOf(peer.id))
                else -> channel.purpose
            }
            if (sub.isNotEmpty()) {
                Text(sub, maxLines = 1, overflow = TextOverflow.Ellipsis, fontSize = 12.sp, color = LocalChatColors.current.muted)
            }
        }
    }
}

@Composable
fun TypingLine(typing: List<User>) {
    val text = when (typing.size) {
        0 -> ""
        1 -> "${typing[0].displayName()} is typing…"
        2 -> "${typing[0].displayName()} and ${typing[1].displayName()} are typing…"
        else -> "Several people are typing…"
    }
    Text(
        text,
        fontSize = 12.sp,
        color = LocalChatColors.current.muted,
        maxLines = 1,
        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 1.dp),
    )
}

@Composable
fun Notice(text: String) {
    Surface(color = MaterialTheme.colorScheme.surfaceVariant, modifier = Modifier.fillMaxWidth()) {
        Text(text, textAlign = TextAlign.Center, modifier = Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun ChannelStart(channel: Channel) {
    Column(Modifier.fillMaxWidth().padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(
            if (channel.isConversation) channel.title() else "Welcome to #${channel.title()}",
            style = MaterialTheme.typography.titleLarge,
            fontWeight = FontWeight.Bold,
        )
        Text(
            when {
                channel.isConversation -> "This is the start of your conversation."
                channel.purpose.isNotEmpty() -> channel.purpose
                else -> "This is the start of the channel."
            },
            color = LocalChatColors.current.muted,
        )
    }
}
