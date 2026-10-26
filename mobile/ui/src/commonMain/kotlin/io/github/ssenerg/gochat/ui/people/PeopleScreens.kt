package io.github.ssenerg.gochat.ui.people

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Logout
import androidx.compose.material.icons.filled.Archive
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.NotificationsOff
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Tag
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import gochat.v1.Channel
import gochat.v1.ChannelMember
import gochat.v1.ChannelType
import gochat.v1.CreateChannelRequest
import gochat.v1.TeamMember
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.isConversation
import io.github.ssenerg.gochat.core.model.isDirect
import io.github.ssenerg.gochat.core.model.isPrivate
import io.github.ssenerg.gochat.core.model.isVoice
import io.github.ssenerg.gochat.core.model.moderatedBy
import io.github.ssenerg.gochat.core.model.title
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.ui.AvatarWithStatus
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.BotBadge
import io.github.ssenerg.gochat.ui.EmptyState
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

@Composable
private fun Screen(title: String, actions: @Composable () -> Unit = {}, content: @Composable (Modifier) -> Unit) {
    val nav = LocalNavigator.current
    Scaffold(
        topBar = {
            TopAppBar(title = { Text(title) }, navigationIcon = { BackButton { nav.pop() } }, actions = { actions() })
        },
    ) { padding -> content(Modifier.padding(padding).fillMaxSize()) }
}

/** Searches the team's people as the person types. */
@Composable
private fun rememberPeople(session: ChatSession, teamId: String, query: String): Pair<List<TeamMember>?, String?> {
    var people by remember { mutableStateOf<List<TeamMember>?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(teamId, query) {
        delay(250)
        try {
            people = session.api.teamMembers(teamId, query.trim(), limit = 50)
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    return people to error
}

/** Starts a direct conversation, or a group one of up to seven others. */
@Composable
fun NewConversationScreen(session: ChatSession) {
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    val chat by session.state.collectAsState()
    val teamId = chat.teamId ?: return
    var query by remember { mutableStateOf("") }
    val picked = remember { mutableStateListOf<User>() }
    var busy by remember { mutableStateOf(false) }
    val (people, error) = rememberPeople(session, teamId, query)

    fun open() {
        if (picked.isEmpty() || busy) return
        busy = true
        scope.launch {
            try {
                val ch = session.openConversation(picked.map { it.id })
                nav.pop()
                nav.openChannel(ch.id)
            } catch (e: Exception) {
                toasts.show(e.userMessage())
            }
            busy = false
        }
    }

    Screen("New message", actions = {
        TextButton(onClick = { open() }, enabled = picked.isNotEmpty() && !busy) { Text(if (picked.size > 1) "Start group" else "Go") }
    }) { modifier ->
        Column(modifier) {
            OutlinedTextField(
                value = query,
                onValueChange = { query = it },
                placeholder = { Text("Find people") },
                leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                singleLine = true,
                modifier = Modifier.fillMaxWidth().padding(12.dp),
            )
            if (picked.isNotEmpty()) {
                Text(
                    picked.joinToString(", ") { it.displayName() },
                    color = MaterialTheme.colorScheme.primary,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
                )
            }
            when {
                error != null && people == null -> ErrorState(error)
                people == null -> Loading()
                else -> {
                    val others = people.mapNotNull { it.user }.filter { it.id != chat.me.id && !it.deactivated }
                    if (others.isEmpty()) {
                        EmptyState("Nobody matches.")
                    } else {
                        LazyColumn {
                            items(others, key = { it.id }) { u ->
                                val chosen = picked.any { it.id == u.id }
                                ListItem(
                                    headlineContent = { NameWithBadge(u) },
                                    supportingContent = { Text("@" + u.username) },
                                    leadingContent = { AvatarWithStatus(u, chat.statusOf(u.id)) },
                                    trailingContent = {
                                        Checkbox(checked = chosen, onCheckedChange = null)
                                    },
                                    modifier = Modifier.clickable {
                                        if (chosen) {
                                            picked.removeAll { it.id == u.id }
                                        } else if (picked.size < 7) {
                                            picked.add(u)
                                        } else {
                                            toasts.show("A group conversation takes up to eight people.")
                                        }
                                    },
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}

/** The team's public channels the person has not joined. */
@Composable
fun BrowseChannelsScreen(session: ChatSession) {
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    val chat by session.state.collectAsState()
    val teamId = chat.teamId ?: return
    var channels by remember { mutableStateOf<List<Channel>?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var attempt by remember { mutableStateOf(0) }
    LaunchedEffect(teamId, attempt) {
        try {
            channels = session.api.browseChannels(teamId).channels
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    Screen("Browse channels") { modifier ->
        val list = channels
        when {
            error != null -> ErrorState(error ?: "", { attempt++ }, modifier)
            list == null -> Loading(modifier)
            list.isEmpty() -> EmptyState("You are in every public channel of this team.", modifier)
            else -> LazyColumn(modifier) {
                items(list, key = { it.id }) { ch ->
                    ListItem(
                        headlineContent = { Text("#" + ch.title()) },
                        supportingContent = if (ch.purpose.isNotEmpty()) ({ Text(ch.purpose) }) else null,
                        trailingContent = {
                            OutlinedButton(onClick = {
                                scope.launch {
                                    try {
                                        session.joinChannel(ch.id)
                                        nav.pop()
                                        nav.openChannel(ch.id)
                                    } catch (e: Exception) {
                                        toasts.show(e.userMessage())
                                    }
                                }
                            }) { Text("Join") }
                        },
                    )
                    HorizontalDivider()
                }
            }
        }
    }
}

private enum class Kind(val label: String, val detail: String, val type: ChannelType) {
    Public("Public", "Anyone in the team can find and join it.", ChannelType.CHANNEL_TYPE_PUBLIC),
    Private("Private", "Only the people its members add see it.", ChannelType.CHANNEL_TYPE_PRIVATE),
    Voice("Voice", "A room the team talks in.", ChannelType.CHANNEL_TYPE_VOICE),
}

/** Turns a display name into a URL name, as the web suggests it. */
private fun slug(name: String): String =
    name.lowercase().map { if (it in 'a'..'z' || it in '0'..'9') it else '-' }.joinToString("")
        .replace(Regex("-+"), "-").trim('-').take(50)

@Composable
fun NewChannelScreen(session: ChatSession) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    val chat by session.state.collectAsState()
    val teamId = chat.teamId ?: return
    var displayName by remember { mutableStateOf("") }
    var name by remember { mutableStateOf("") }
    var nameEdited by remember { mutableStateOf(false) }
    var purpose by remember { mutableStateOf("") }
    var kind by remember { mutableStateOf(Kind.Public) }
    var voiceMessages by remember { mutableStateOf(true) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }

    fun create() {
        if (displayName.isBlank() || busy) return
        busy = true
        error = null
        scope.launch {
            try {
                val ch = session.createChannel(
                    CreateChannelRequest(
                        team_id = teamId,
                        name = name.ifBlank { slug(displayName) },
                        display_name = displayName.trim(),
                        purpose = purpose.trim(),
                        type = kind.type,
                        voice_disabled = !voiceMessages,
                    ),
                )
                nav.pop()
                if (ch != null && !ch.isVoice) nav.openChannel(ch.id)
            } catch (e: Exception) {
                error = e.userMessage()
            }
            busy = false
        }
    }

    Screen("Create a channel") { modifier ->
        Column(modifier.verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            OutlinedTextField(
                value = displayName,
                onValueChange = {
                    displayName = it
                    if (!nameEdited) name = slug(it)
                },
                label = { Text("Name") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences, imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = name,
                onValueChange = {
                    name = it.lowercase()
                    nameEdited = true
                },
                label = { Text("URL name") },
                supportingText = { Text("Lowercase letters, digits and '-'.") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = purpose,
                onValueChange = { purpose = it },
                label = { Text("Purpose (optional)") },
                modifier = Modifier.fillMaxWidth(),
            )
            Kind.entries.forEach { k ->
                Row(Modifier.fillMaxWidth().clickable { kind = k }, verticalAlignment = Alignment.CenterVertically) {
                    RadioButton(selected = kind == k, onClick = { kind = k })
                    Column {
                        Text(k.label, fontWeight = FontWeight.SemiBold)
                        Text(k.detail, color = LocalChatColors.current.muted)
                    }
                }
            }
            if (kind != Kind.Voice) {
                Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                    Text("Allow voice messages", modifier = Modifier.weight(1f))
                    Switch(checked = voiceMessages, onCheckedChange = { voiceMessages = it })
                }
            }
            if (error != null) Text(error ?: "", color = MaterialTheme.colorScheme.error)
            Button(onClick = { create() }, enabled = displayName.isNotBlank() && !busy, modifier = Modifier.fillMaxWidth().height(48.dp)) {
                if (busy) CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp) else Text("Create channel")
            }
        }
    }
}

/** A channel's members and settings: mute, leave, and for moderators the rest. */
@Composable
fun ChannelInfoScreen(session: ChatSession, channelId: String) {
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val chat by session.state.collectAsState()
    val channel = chat.channel(channelId)
    if (channel == null) {
        Screen("Channel") { EmptyState("This channel is gone.", it) }
        return
    }
    val moderator = channel.moderatedBy(chat.me, chat.team) && !channel.isConversation
    var members by remember { mutableStateOf<List<ChannelMember>?>(null) }
    var reload by remember { mutableStateOf(0) }
    var confirmLeave by remember { mutableStateOf(false) }
    var confirmArchive by remember { mutableStateOf(false) }
    var confirmDelete by remember { mutableStateOf(false) }
    var edit by remember { mutableStateOf(false) }
    LaunchedEffect(channelId, reload) {
        members = runCatching { session.api.channelMembers(channelId) }.getOrNull()
    }
    // Each runs in the session: leaving the screen does not cancel it.
    fun run(block: suspend () -> Unit) {
        session.launch({ runCatching { block() }.exceptionOrNull() }) { e -> e?.let { toasts.show(it.userMessage()) } }
    }

    Screen(if (channel.isConversation) channel.title() else "#${channel.title()}") { modifier ->
        LazyColumn(modifier) {
            if (channel.purpose.isNotEmpty()) {
                item { Text(channel.purpose, modifier = Modifier.padding(16.dp)) }
            }
            item {
                val muted = channelId in chat.muted
                ListItem(
                    headlineContent = { Text(if (muted) "Unmute" else "Mute") },
                    supportingContent = { Text(if (muted) "Muted: no unread marks or notices." else "Hide its unread marks and notices.") },
                    leadingContent = { Icon(if (muted) Icons.Filled.Notifications else Icons.Filled.NotificationsOff, contentDescription = null) },
                    modifier = Modifier.clickable { run { session.setMuted(channelId, !muted)?.let { toasts.show(it) } } },
                )
            }
            if (moderator) {
                item {
                    ListItem(
                        headlineContent = { Text("Edit name and purpose") },
                        leadingContent = { Icon(if (channel.isPrivate) Icons.Filled.Lock else Icons.Filled.Tag, contentDescription = null) },
                        modifier = Modifier.clickable { edit = true },
                    )
                }
                item {
                    ListItem(
                        headlineContent = { Text("Voice messages") },
                        supportingContent = { Text(if (channel.voice_enabled) "Members can send voice messages." else "Nobody can send new ones.") },
                        trailingContent = {
                            Switch(checked = channel.voice_enabled, onCheckedChange = { on -> run { session.api.setVoiceMessages(channelId, on) } })
                        },
                    )
                }
                if (channel.name != "town-square") {
                    item {
                        ListItem(
                            headlineContent = { Text(if (channel.isPrivate) "Make public" else "Make private") },
                            supportingContent = {
                                Text(if (channel.isPrivate) "Only team admins can make it public." else "Only its members will see it from now on.")
                            },
                            modifier = Modifier.clickable { run { session.api.setPrivacy(channelId, !channel.isPrivate) } },
                        )
                    }
                    item {
                        ListItem(
                            headlineContent = { Text("Archive channel", color = MaterialTheme.colorScheme.error) },
                            leadingContent = { Icon(Icons.Filled.Archive, contentDescription = null, tint = MaterialTheme.colorScheme.error) },
                            modifier = Modifier.clickable { confirmArchive = true },
                        )
                    }
                }
            }
            if (channel.isDirect) {
                item {
                    ListItem(
                        headlineContent = { Text("Delete conversation", color = MaterialTheme.colorScheme.error) },
                        leadingContent = { Icon(Icons.Filled.Delete, contentDescription = null, tint = MaterialTheme.colorScheme.error) },
                        modifier = Modifier.clickable { confirmDelete = true },
                    )
                }
            }
            if (!channel.isConversation && channel.name != "town-square") {
                item {
                    ListItem(
                        headlineContent = { Text("Leave channel", color = MaterialTheme.colorScheme.error) },
                        leadingContent = { Icon(Icons.AutoMirrored.Filled.Logout, contentDescription = null, tint = MaterialTheme.colorScheme.error) },
                        modifier = Modifier.clickable { confirmLeave = true },
                    )
                }
            }
            item {
                HorizontalDivider()
                Text(
                    "Members" + (members?.let { " (${it.size})" } ?: ""),
                    fontWeight = FontWeight.SemiBold,
                    modifier = Modifier.padding(16.dp),
                )
            }
            val list = members
            if (list == null) {
                item { Loading(Modifier.height(80.dp)) }
            } else {
                items(list.filter { it.user != null }.distinctBy { it.user?.id }, key = { it.user?.id.orEmpty() }) { m ->
                    val u = m.user ?: return@items
                    ListItem(
                        headlineContent = { NameWithBadge(u) },
                        supportingContent = { Text("@" + u.username) },
                        leadingContent = { AvatarWithStatus(u, chat.statusOf(u.id)) },
                        trailingContent = if (moderator && u.id != chat.me.id && !channel.isConversation) {
                            {
                                TextButton(onClick = {
                                    run {
                                        session.api.removeChannelMember(channelId, u.id)
                                        reload++
                                    }
                                }) { Text("Remove") }
                            }
                        } else {
                            null
                        },
                    )
                }
            }
        }
    }

    if (confirmLeave) {
        AlertDialog(
            onDismissRequest = { confirmLeave = false },
            title = { Text("Leave #${channel.title()}?") },
            text = { Text(if (channel.isPrivate) "You will need a member to add you back." else "You can join it again from Browse channels.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmLeave = false
                    session.launch({ session.leaveChannel(channelId) }) { err ->
                        if (err != null) toasts.show(err) else nav.closeChannel(channelId)
                    }
                }) { Text("Leave", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { confirmLeave = false }) { Text("Cancel") } },
        )
    }
    if (confirmDelete) {
        val peer = channel.peer.displayName()
        var everyone by remember { mutableStateOf(false) }
        AlertDialog(
            onDismissRequest = { confirmDelete = false },
            title = { Text("Delete conversation") },
            text = {
                Column {
                    Text("Delete your conversation with $peer? You stop seeing its messages; it comes back, with only new ones, when someone writes in it.")
                    Row(Modifier.padding(top = 12.dp).clickable { everyone = !everyone }, verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(checked = everyone, onCheckedChange = { everyone = it })
                        Text("Also delete it for $peer")
                    }
                    if (everyone) {
                        Text(
                            "Its messages, replies, reactions, pins and files are deleted for good, for both of you.",
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
            },
            confirmButton = {
                TextButton(onClick = {
                    confirmDelete = false
                    session.launch({ session.deleteConversation(channelId, everyone) }) { err ->
                        if (err != null) toasts.show(err) else nav.closeChannel(channelId)
                    }
                }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { confirmDelete = false }) { Text("Cancel") } },
        )
    }
    if (confirmArchive) {
        AlertDialog(
            onDismissRequest = { confirmArchive = false },
            title = { Text("Archive #${channel.title()}?") },
            text = { Text("It keeps its messages for its members to read and takes no new ones.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmArchive = false
                    run { session.api.archive(channelId, true) }
                }) { Text("Archive", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { confirmArchive = false }) { Text("Cancel") } },
        )
    }
    if (edit) EditChannelDialog(channel, session) { edit = false }
}

@Composable
private fun EditChannelDialog(channel: Channel, session: ChatSession, onDone: () -> Unit) {
    val scope = rememberCoroutineScope()
    var displayName by remember { mutableStateOf(channel.display_name) }
    var name by remember { mutableStateOf(channel.name) }
    var purpose by remember { mutableStateOf(channel.purpose) }
    var error by remember { mutableStateOf<String?>(null) }
    AlertDialog(
        onDismissRequest = onDone,
        title = { Text("Edit channel") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(value = displayName, onValueChange = { displayName = it }, label = { Text("Name") }, singleLine = true)
                OutlinedTextField(
                    value = name, onValueChange = { name = it.lowercase() }, label = { Text("URL name") }, singleLine = true,
                    enabled = channel.name != "town-square",
                )
                OutlinedTextField(value = purpose, onValueChange = { purpose = it }, label = { Text("Purpose") })
                if (error != null) Text(error ?: "", color = MaterialTheme.colorScheme.error)
            }
        },
        confirmButton = {
            TextButton(onClick = {
                scope.launch {
                    try {
                        session.api.updateChannel(channel.id, displayName.trim(), name.trim(), purpose.trim())
                        onDone()
                    } catch (e: Exception) {
                        error = e.userMessage()
                    }
                }
            }) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDone) { Text("Cancel") } },
    )
}

/** A person's name, marked when they are a bot. */
@Composable
private fun NameWithBadge(u: User) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Text(u.displayName(), maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
        if (u.bot) {
            Spacer(Modifier.width(6.dp))
            BotBadge()
        }
    }
}
