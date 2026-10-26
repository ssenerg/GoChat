package io.github.ssenerg.gochat.ui.main

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Bookmark
import androidx.compose.material.icons.filled.CloudOff
import androidx.compose.material.icons.filled.Forum
import androidx.compose.material.icons.filled.Group
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Tag
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
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
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.Channel
import gochat.v1.Status
import gochat.v1.Team
import gochat.v1.VoiceChannelState
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.socket.Connection
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.isPrivate
import io.github.ssenerg.gochat.core.model.isUnread
import io.github.ssenerg.gochat.core.model.title
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.ChatState
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.ui.Avatar
import io.github.ssenerg.gochat.ui.AvatarWithStatus
import io.github.ssenerg.gochat.ui.BotBadge
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.LocalImages
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.MentionBadge
import io.github.ssenerg.gochat.ui.ServerImage
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.voice.VoicePanel
import kotlinx.coroutines.launch

/** The team rail and the team's channels and conversations. */
@Composable
fun Sidebar(session: ChatSession, app: GoChatApp, modifier: Modifier = Modifier) {
    val st by session.state.collectAsState()
    val nav = LocalNavigator.current
    val colors = LocalChatColors.current
    val active = when (val top = nav.top) {
        is Route.Channel -> top.id
        is Route.Thread -> top.channelId
        is Route.Voice -> top.id
        else -> null
    }
    Row(modifier.background(colors.sidebar)) {
        TeamRail(st, onSelect = { session.selectTeam(it) })
        Column(Modifier.weight(1f).fillMaxHeight().statusBarsPadding()) {
            TeamHeader(st, session)
            ConnectionBanner(st.connection)
            when {
                st.loading && st.channels.isEmpty() -> Loading(Modifier.weight(1f))
                st.error != null && st.channels.isEmpty() -> Box(Modifier.weight(1f)) {
                    ErrorState(st.error ?: "", onRetry = { session.refresh() })
                }
                st.teams.isEmpty() -> Box(Modifier.weight(1f)) {
                    ErrorState("You are not in a team yet. Ask a team admin for an invite, or create a team on the web.")
                }
                else -> ChannelList(st, active, session, Modifier.weight(1f))
            }
            VoicePanel(session)
            MeBar(st, onClick = { nav.open(Route.Settings) })
        }
    }
}

@Composable
private fun TeamRail(st: ChatState, onSelect: (String) -> Unit) {
    val colors = LocalChatColors.current
    val api = LocalImages.current?.api
    LazyColumn(
        Modifier.width(68.dp).fillMaxHeight().background(colors.rail).statusBarsPadding(),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(8.dp),
        contentPadding = androidx.compose.foundation.layout.PaddingValues(vertical = 12.dp),
    ) {
        items(st.teams, key = { it.id }) { team ->
            val selected = team.id == st.teamId
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(
                    Modifier.width(4.dp).height(if (selected) 36.dp else 0.dp)
                        .clip(RoundedCornerShape(topEnd = 4.dp, bottomEnd = 4.dp))
                        .background(MaterialTheme.colorScheme.primary),
                )
                Spacer(Modifier.width(8.dp))
                TeamIcon(team, selected, api?.teamPhotoUrl(team.id, team.photo), onClick = { onSelect(team.id) })
                Spacer(Modifier.width(12.dp))
            }
        }
    }
}

@Composable
private fun TeamIcon(team: Team, selected: Boolean, photo: String?, onClick: () -> Unit) {
    val shape = RoundedCornerShape(if (selected) 14.dp else 22.dp)
    Box(
        Modifier.size(44.dp).clip(shape)
            .background(if (selected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.surfaceContainerHigh)
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        if (photo != null) {
            ServerImage(photo, contentDescription = team.display_name, modifier = Modifier.fillMaxSize())
        } else {
            Text(
                team.display_name.split(' ').filter { it.isNotEmpty() }.take(2).joinToString("") { it.take(1) }.uppercase().ifEmpty { "?" },
                color = if (selected) MaterialTheme.colorScheme.onPrimary else MaterialTheme.colorScheme.onSurface,
                fontWeight = FontWeight.SemiBold,
            )
        }
    }
}

@Composable
private fun TeamHeader(st: ChatState, session: ChatSession) {
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val platform = LocalPlatform.current
    val scope = rememberCoroutineScope()
    var menu by remember { mutableStateOf(false) }
    Row(Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 8.dp, bottom = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(
            st.team?.display_name ?: "GoChat",
            style = MaterialTheme.typography.titleMedium,
            fontWeight = FontWeight.Bold,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f),
        )
        IconButton(onClick = { nav.open(Route.Search) }, enabled = st.team != null) {
            Icon(Icons.Filled.Search, contentDescription = "Search")
        }
        Box {
            IconButton(onClick = { menu = true }, enabled = st.team != null) {
                Icon(Icons.Filled.MoreVert, contentDescription = "Team menu")
            }
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                DropdownMenuItem(text = { Text("Invite people") }, onClick = {
                    menu = false
                    val team = st.team ?: return@DropdownMenuItem
                    scope.launch {
                        try {
                            val invite = session.api.createInvite(team.id)
                            platform.share("Join ${team.display_name} on GoChat: ${session.api.client.baseUrl}/join/${invite.token}")
                        } catch (e: Exception) {
                            toasts.show(e.userMessage())
                        }
                    }
                })
                DropdownMenuItem(text = { Text("Browse channels") }, onClick = { menu = false; nav.open(Route.Browse) })
                DropdownMenuItem(text = { Text("Create a channel") }, onClick = { menu = false; nav.open(Route.NewChannel) })
                DropdownMenuItem(text = { Text("New message") }, onClick = { menu = false; nav.open(Route.NewConversation) })
            }
        }
    }
}

@Composable
private fun ConnectionBanner(c: Connection) {
    val text = when (c) {
        is Connection.Online, Connection.SignedOut -> return
        Connection.Connecting -> "Connecting…"
        is Connection.Offline -> "${c.reason}…"
        is Connection.Refused -> c.reason
    }
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp)
            .clip(RoundedCornerShape(8.dp)).background(MaterialTheme.colorScheme.errorContainer)
            .padding(horizontal = 10.dp, vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(Icons.Filled.CloudOff, contentDescription = null, modifier = Modifier.size(16.dp))
        Spacer(Modifier.width(8.dp))
        Text(text, style = MaterialTheme.typography.labelMedium)
    }
}

@Composable
private fun ChannelList(st: ChatState, active: String?, session: ChatSession, modifier: Modifier) {
    val nav = LocalNavigator.current
    val call by session.voice.state.collectAsState()
    LazyColumn(modifier.fillMaxWidth(), contentPadding = androidx.compose.foundation.layout.PaddingValues(bottom = 8.dp)) {
        item {
            SidebarLink(Icons.Filled.Forum, "Threads", unread = st.threadsUnread > 0) { nav.open(Route.Threads) }
        }
        item {
            SidebarLink(Icons.Filled.Bookmark, "Saved", unread = false) { nav.open(Route.Saved) }
        }
        item { SectionHeader("Channels", onAdd = { nav.open(Route.NewChannel) }) }
        items(st.textChannels, key = { it.id }) { ch ->
            ChannelRow(ch, st, active == ch.id) { nav.openChannel(ch.id) }
        }
        if (st.voiceChannels.isNotEmpty()) {
            item { SectionHeader("Voice channels", onAdd = null) }
            items(st.voiceChannels, key = { it.id }) { ch ->
                val here = call?.takeIf { it.channelId == ch.id }
                VoiceRow(ch, st.voice[ch.id], active == ch.id, here?.connected == true, here?.speaking.orEmpty()) {
                    nav.open(Route.Voice(ch.id))
                }
            }
        }
        item { SectionHeader("Direct messages", onAdd = { nav.open(Route.NewConversation) }) }
        items(st.conversations, key = { it.id }) { ch ->
            ConversationRow(ch, st, active == ch.id) { nav.openChannel(ch.id) }
        }
    }
}

@Composable
private fun SidebarLink(icon: ImageVector, text: String, unread: Boolean, onClick: () -> Unit) {
    val colors = LocalChatColors.current
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, contentDescription = null, tint = colors.sidebarMuted, modifier = Modifier.size(20.dp))
        Spacer(Modifier.width(12.dp))
        Text(text, color = colors.sidebarText, fontWeight = if (unread) FontWeight.Bold else FontWeight.Normal, modifier = Modifier.weight(1f))
        if (unread) Box(Modifier.size(8.dp).clip(CircleShape).background(colors.unread))
    }
}

@Composable
private fun SectionHeader(title: String, onAdd: (() -> Unit)?) {
    val colors = LocalChatColors.current
    Row(Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 12.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(
            title.uppercase(),
            color = colors.sidebarMuted,
            fontSize = 12.sp,
            fontWeight = FontWeight.SemiBold,
            letterSpacing = 0.5.sp,
            modifier = Modifier.weight(1f),
        )
        if (onAdd != null) {
            IconButton(onClick = onAdd, modifier = Modifier.size(36.dp)) {
                Icon(Icons.Filled.Add, contentDescription = "Add to $title", tint = colors.sidebarMuted, modifier = Modifier.size(18.dp))
            }
        } else {
            Spacer(Modifier.height(36.dp))
        }
    }
}

@Composable
private fun RowShell(active: Boolean, onClick: () -> Unit, content: @Composable () -> Unit) {
    val colors = LocalChatColors.current
    Box(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 1.dp)
            .clip(RoundedCornerShape(8.dp))
            .background(if (active) colors.sidebarActive else androidx.compose.ui.graphics.Color.Transparent)
            .clickable(onClick = onClick)
            .padding(horizontal = 8.dp, vertical = 8.dp),
    ) { content() }
}

@Composable
private fun ChannelRow(ch: Channel, st: ChatState, active: Boolean, onClick: () -> Unit) {
    val colors = LocalChatColors.current
    val muted = ch.id in st.muted
    val unread = ch.isUnread() && !active && !muted
    RowShell(active, onClick) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Icon(
                if (ch.isPrivate) Icons.Filled.Lock else Icons.Filled.Tag,
                contentDescription = if (ch.isPrivate) "Private channel" else null,
                tint = colors.sidebarMuted,
                modifier = Modifier.size(18.dp),
            )
            Spacer(Modifier.width(10.dp))
            Text(
                ch.title(),
                color = if (unread) colors.sidebarText else colors.sidebarText.copy(alpha = if (muted) 0.5f else 0.85f),
                fontWeight = if (unread) FontWeight.Bold else FontWeight.Normal,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.weight(1f),
            )
            if (ch.mention_count > 0 && !active) MentionBadge(ch.mention_count)
        }
    }
}

@Composable
private fun VoiceRow(ch: Channel, state: VoiceChannelState?, active: Boolean, connected: Boolean, speaking: Set<String>, onClick: () -> Unit) {
    val colors = LocalChatColors.current
    RowShell(active, onClick) {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    Icons.AutoMirrored.Filled.VolumeUp,
                    contentDescription = "Voice channel",
                    tint = if (connected) colors.online else colors.sidebarMuted,
                    modifier = Modifier.size(18.dp),
                )
                Spacer(Modifier.width(10.dp))
                Text(ch.title(), color = colors.sidebarText.copy(alpha = 0.85f), maxLines = 1, overflow = TextOverflow.Ellipsis)
            }
            state?.states?.forEach { vs ->
                Row(Modifier.padding(start = 28.dp, top = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    val ring = if (vs.user?.id in speaking) colors.online else androidx.compose.ui.graphics.Color.Transparent
                    Avatar(vs.user, size = 22.dp, modifier = Modifier.border(2.dp, ring, CircleShape))
                    Spacer(Modifier.width(8.dp))
                    Text(vs.user.displayName(), color = colors.sidebarText, fontSize = 13.sp, modifier = Modifier.weight(1f, fill = false))
                    if (vs.self_mute || vs.self_deaf) {
                        Spacer(Modifier.width(6.dp))
                        Icon(Icons.Filled.MicOff, contentDescription = "Muted", tint = colors.sidebarMuted, modifier = Modifier.size(14.dp))
                    }
                }
            }
        }
    }
}

@Composable
private fun ConversationRow(ch: Channel, st: ChatState, active: Boolean, onClick: () -> Unit) {
    val colors = LocalChatColors.current
    val unread = ch.isUnread() && !active && ch.id !in st.muted
    RowShell(active, onClick) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            val peer = ch.peer
            if (peer != null) {
                AvatarWithStatus(peer, st.statusOf(peer.id), size = 30.dp)
            } else {
                Box(
                    Modifier.size(30.dp).clip(CircleShape).background(MaterialTheme.colorScheme.surfaceContainerHigh),
                    contentAlignment = Alignment.Center,
                ) {
                    Icon(Icons.Filled.Group, contentDescription = null, modifier = Modifier.size(18.dp))
                }
            }
            Spacer(Modifier.width(10.dp))
            Row(Modifier.weight(1f), verticalAlignment = Alignment.CenterVertically) {
                Text(
                    ch.title(),
                    color = colors.sidebarText.copy(alpha = if (unread) 1f else 0.85f),
                    fontWeight = if (unread) FontWeight.Bold else FontWeight.Normal,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = false),
                )
                if (peer?.bot == true) {
                    Spacer(Modifier.width(6.dp))
                    BotBadge()
                }
            }
            if (ch.mention_count > 0 && !active) MentionBadge(ch.mention_count)
        }
    }
}

@Composable
private fun MeBar(st: ChatState, onClick: () -> Unit) {
    val colors = LocalChatColors.current
    val me = st.me
    val status = if (me.status == Status.STATUS_UNSPECIFIED) Status.STATUS_ONLINE else me.status
    Row(
        Modifier.fillMaxWidth().background(colors.rail.copy(alpha = 0.6f)).clickable(onClick = onClick)
            .navigationBarsPadding().padding(horizontal = 12.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        AvatarWithStatus(me, status, size = 34.dp)
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f)) {
            Text(me.displayName(), color = colors.sidebarText, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
            val custom = me.custom_status
            val line = if (custom != null && (custom.text.isNotEmpty() || custom.emoji.isNotEmpty())) {
                listOfNotNull(io.github.ssenerg.gochat.core.model.Emojis.char(custom.emoji), custom.text.ifEmpty { null }).joinToString(" ")
            } else {
                io.github.ssenerg.gochat.ui.statusLabel(status)
            }
            Text(line, color = colors.sidebarMuted, fontSize = 12.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        Icon(Icons.Filled.Settings, contentDescription = "Settings", tint = colors.sidebarMuted)
    }
}
