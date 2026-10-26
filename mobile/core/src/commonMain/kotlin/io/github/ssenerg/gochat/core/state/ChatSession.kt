package io.github.ssenerg.gochat.core.state

import gochat.v1.Attachment
import gochat.v1.CallbackAnswer
import gochat.v1.Channel
import gochat.v1.ChannelCommand
import gochat.v1.Event
import gochat.v1.Message
import gochat.v1.Reaction
import gochat.v1.Status
import gochat.v1.Team
import gochat.v1.User
import gochat.v1.VoiceChannelState
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.excerpt
import io.github.ssenerg.gochat.core.model.isArchived
import io.github.ssenerg.gochat.core.model.isConversation
import io.github.ssenerg.gochat.core.model.isVoice
import io.github.ssenerg.gochat.core.model.mentions
import io.github.ssenerg.gochat.core.model.newer
import io.github.ssenerg.gochat.core.model.inDnd
import io.github.ssenerg.gochat.core.model.title
import io.github.ssenerg.gochat.core.model.uuidTime
import io.github.ssenerg.gochat.core.socket.Connection
import io.github.ssenerg.gochat.core.socket.Socket
import io.github.ssenerg.gochat.core.socket.SocketEvent
import io.github.ssenerg.gochat.core.voice.RtcEngine
import io.github.ssenerg.gochat.core.voice.VoiceCall
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds
import kotlin.time.TimeSource

/** What the sidebar and the rest of the app show of a signed-in person's world. */
data class ChatState(
    val me: User,
    val teams: List<Team> = emptyList(),
    val teamId: String? = null,
    /** The team's channels the person belongs to, its voice channels and every conversation. */
    val channels: List<Channel> = emptyList(),
    val muted: Set<String> = emptySet(),
    val saved: Set<String> = emptySet(),
    /** The people not offline, and how available they are. */
    val presence: Map<String, Status> = emptyMap(),
    /** Who talks in each voice channel, by channel id. */
    val voice: Map<String, VoiceChannelState> = emptyMap(),
    val threadsUnread: Int = 0,
    val loading: Boolean = true,
    val error: String? = null,
    val connection: Connection = Connection.Connecting,
) {
    val team: Team? get() = teams.firstOrNull { it.id == teamId }

    val textChannels: List<Channel>
        get() = channels.filter { !it.isConversation && !it.isVoice && !it.isArchived }.sortedBy { it.display_name.lowercase() }

    val voiceChannels: List<Channel>
        get() = channels.filter { it.isVoice && !it.isArchived }.sortedBy { it.display_name.lowercase() }

    /** Direct and group conversations, the latest first. */
    val conversations: List<Channel>
        get() = channels.filter { it.isConversation }.sortedByDescending { uuidTime(it.last_message_id) }

    fun channel(id: String): Channel? = channels.firstOrNull { it.id == id }

    fun statusOf(userId: String): Status = presence[userId] ?: Status.STATUS_OFFLINE
}

/** A message elsewhere that calls for the person: a mention, a direct message, a reply. */
data class Notice(val title: String, val body: String, val channelId: String, val rootId: String = "")

/** A bot's answer to a button the person pressed: a passing notice, or an [alert] to dismiss. */
data class BotAnswer(val bot: String, val text: String, val alert: Boolean)

/**
 * A signed-in person's session: it loads their teams and channels, keeps
 * them current with the socket's updates, and makes the calls the screens
 * need. Use it from one thread, the main one.
 */
class ChatSession internal constructor(
    val api: GoChatApi,
    me: User,
    private val store: KeyValueStore,
    parent: CoroutineScope,
    rtc: RtcEngine?,
    /** The server no longer takes this app: the session ends, its token kept for an updated app. */
    private val onRefused: ((String) -> Unit)? = null,
    private val onSignedOut: (String) -> Unit,
) {
    private val scope = CoroutineScope(parent.coroutineContext + SupervisorJob(parent.coroutineContext[Job]))
    private val _state = MutableStateFlow(ChatState(me = me))
    val state: StateFlow<ChatState> = _state.asStateFlow()

    /** The socket the session's calls and updates come over. */
    val socket: Socket get() = api.client.socket

    private val updates = Updates(
        scope,
        difference = { api.difference(it) },
        channelDifference = { id, pts -> api.channelDifference(id, pts) },
        apply = { ev, quiet -> event(ev, quiet) },
        reload = { sync(full = true) },
        reloadChannel = { id ->
            timelines[id]?.reload()
            threads.values.filter { it.channelId == id }.forEach { it.reload() }
        },
        applied = { keepPts(it) },
    )

    /** Set while the person signs out, when the socket's end is expected. */
    internal var signingOut = false

    /** The voice channel the person talks in, if any. */
    val voice = VoiceCall(api, rtc, store, me.id, scope)

    private val _notices = MutableSharedFlow<Notice>(extraBufferCapacity = 16)
    val notices: SharedFlow<Notice> = _notices.asSharedFlow()

    private val _answers = MutableSharedFlow<BotAnswer>(extraBufferCapacity = 8)

    /** Bots' answers to the buttons this device pressed. */
    val answers: SharedFlow<BotAnswer> = _answers.asSharedFlow()

    private val timelines = LinkedHashMap<String, Timeline>()
    private val threads = LinkedHashMap<String, ThreadView>()

    /** The channel and thread on screen, which read what arrives. */
    private var visibleChannel: String? = null
    private var visibleThread: String? = null
    private var lastTyping = mutableMapOf<String, TimeSource.Monotonic.ValueTimeMark>()

    val me: User get() = _state.value.me

    /** Voice messages played lately, to replay without fetching again; they go with the session. */
    val voiceCache = VoiceCache()

    /**
     * Runs [work] for as long as the session lasts rather than the screen
     * that asked, so a sheet or dialog that closes does not cancel it; [then]
     * gets its result.
     */
    fun <T> launch(work: suspend () -> T, then: (T) -> Unit = {}): Job = scope.launch { then(work()) }

    /** Starts after the account's update [pts] the app kept, 0 for none; the socket's welcome loads the rest. */
    internal fun start(pts: Long) {
        updates.start(pts)
        scope.launch { for (e in socket.events) handle(e) }
        // Presence reaches only the sockets that show the person: the
        // sidebar shows the conversations' people.
        scope.launch {
            _state.map { st -> st.channels.flatMap { ch -> listOfNotNull(ch.peer?.id) + ch.peers.map { it.id } }.distinct().sorted() }
                .distinctUntilChanged()
                .collect { socket.watch(it) }
        }
        scope.launch {
            socket.connection.collect { c ->
                _state.update { it.copy(connection = c) }
                when {
                    signingOut -> {}
                    c is Connection.SignedOut -> onSignedOut("Your session ended. Sign in again.")
                    c is Connection.Refused -> (onRefused ?: onSignedOut)(c.reason)
                }
            }
        }
        socket.start()
    }

    internal fun stop() {
        savePts()
        voice.close()
        scope.cancel()
        voiceCache.clear()
    }

    // The account's number, kept so the app resumes after it once restarted.

    private var saving: Job? = null

    private fun keepPts(pts: Long) {
        socket.pts = pts
        if (saving?.isActive == true) return
        saving = scope.launch {
            delay(SAVE_PTS_MS)
            savePts()
        }
    }

    private fun savePts() {
        if (updates.pts > 0) store.put(KEY_PTS, updates.pts.toString())
    }

    // Syncing: after each welcome, what the socket may have missed loads.

    private var synced = false
    private var syncing: Job? = null

    /**
     * Loads what the app shows, holding updates until it has: everything
     * when [full], else what changed while the socket was down.
     */
    private fun sync(full: Boolean) {
        val previous = syncing
        syncing = scope.launch(start = CoroutineStart.UNDISPATCHED) {
            updates.hold()
            try {
                previous?.cancelAndJoin()
                if (full) {
                    loadTeams()
                    reloadViews()
                    runCatching { api.savedIds() }.onSuccess { ids -> _state.update { it.copy(saved = ids.toSet()) } }
                    synced = _state.value.error == null
                } else {
                    catchUp()
                }
            } finally {
                updates.release()
            }
        }
    }

    private fun reloadViews() {
        timelines.values.forEach { it.reload() }
        threads.values.forEach { it.reload() }
    }

    /** The channels whose messages the screens hold. */
    private fun shownChannels(): Set<String> = timelines.keys + threads.values.map { it.channelId }

    /**
     * Brings the sidebar and the open screens up to date after a reconnect:
     * the list of channels gives the counts, and each open channel takes
     * what it missed, the list counting it already.
     */
    private suspend fun catchUp() {
        val teamId = _state.value.teamId ?: return loadTeams()
        try {
            val channels = api.listChannels(teamId)
            if (teamId != _state.value.teamId) return
            _state.update { it.copy(channels = channels, loading = false, error = null) }
            val shown = shownChannels()
            val listed = channels.associate { it.id to it.pts }
            channels.filter { it.id !in shown }.forEach { updates.learn(it.id, it.pts) }
            for (id in shown) {
                val ok = updates.knows(id) && updates.fill(id, quietThrough = listed[id] ?: 0)
                if (!ok) {
                    timelines[id]?.reload()
                    threads.values.filter { it.channelId == id }.forEach { it.reload() }
                }
                listed[id]?.let { updates.learn(id, it) }
            }
            visibleChannel?.let { markRead(it) }
            val thread = visibleThread
            val threadChannel = visibleChannel
            if (thread != null && threadChannel != null) runCatching { api.markThreadRead(threadChannel, thread) }
            loadAround(teamId, channels)
        } catch (e: Exception) {
            fail(e)
        }
    }

    // Loading.

    private suspend fun loadTeams() {
        try {
            val teams = api.listTeams()
            val saved = store.get(KEY_TEAM)
            val teamId = teams.firstOrNull { it.id == saved }?.id ?: teams.firstOrNull()?.id
            _state.update { it.copy(teams = teams, teamId = teamId, error = null) }
            if (teamId != null) loadSidebar() else _state.update { it.copy(loading = false) }
        } catch (e: Exception) {
            fail(e)
        }
    }

    /** Opens another team. */
    fun selectTeam(teamId: String) {
        if (teamId == _state.value.teamId) return
        store.put(KEY_TEAM, teamId)
        _state.update { it.copy(teamId = teamId, channels = emptyList(), voice = emptyMap(), loading = true) }
        scope.launch {
            updates.hold()
            try {
                loadSidebar()
            } finally {
                updates.release()
            }
        }
    }

    /** Loads the team's channels, mutes, voice states and the peers' presence again. */
    suspend fun loadSidebar() {
        val teamId = _state.value.teamId ?: return
        try {
            val channels = api.listChannels(teamId)
            val muted = api.mutedChannels().toSet()
            if (teamId != _state.value.teamId) return
            _state.update { it.copy(channels = channels, muted = muted, loading = false, error = null) }
            // Each channel's updates count from the list's numbers; another
            // team's channels are no longer followed.
            updates.keepOnly(channels.map { it.id }.toSet() + shownChannels())
            channels.forEach { updates.learn(it.id, it.pts) }
            loadAround(teamId, channels)
        } catch (e: Exception) {
            fail(e)
        }
    }

    /** What updates do not carry again after a reconnect: presence, voice states, unread threads. */
    private suspend fun loadAround(teamId: String, channels: List<Channel>) {
        val peers = channels.flatMap { ch -> listOfNotNull(ch.peer?.id) + ch.peers.map { it.id } }.distinct()
        // The server answers presence for so many people at a time.
        for (batch in peers.chunked(PRESENCE_BATCH)) {
            val p = runCatching { api.presence(batch) }.getOrNull() ?: continue
            val online = p.online_user_ids.toSet()
            _state.update { st ->
                st.copy(presence = st.presence + batch.associateWith { id -> statusFor(id in online, p.statuses[id]) })
            }
        }
        runCatching { api.voiceStates(teamId) }.onSuccess { list ->
            _state.update { st -> st.copy(voice = list.associateBy { it.channel_id }) }
        }
        runCatching { api.threads(teamId, limit = 0) }.onSuccess { t ->
            _state.update { it.copy(threadsUnread = t.unread_count) }
        }
    }

    private fun statusFor(online: Boolean, status: Status?): Status = when {
        !online -> Status.STATUS_OFFLINE
        status == null || status == Status.STATUS_UNSPECIFIED -> Status.STATUS_ONLINE
        else -> status
    }

    private fun fail(e: Exception) {
        if (e is ApiException && e.isUnauthenticated) {
            onSignedOut("Your session ended. Sign in again.")
            return
        }
        _state.update { it.copy(loading = false, error = e.userMessage()) }
    }

    /** Loads everything again. */
    fun refresh() = sync(full = true)

    // Screens.

    /** The timeline of a channel, kept while a few are open. */
    fun timeline(channelId: String): Timeline = timelines.getOrPut(channelId) {
        Timeline(channelId, this, scope).also { it.reload() }
    }.also { trim(timelines) }

    /** A thread's root and replies. */
    fun thread(channelId: String, rootId: String): ThreadView = threads.getOrPut(rootId) {
        ThreadView(channelId, rootId, this, scope).also { it.reload() }
    }.also { trim(threads) }

    private fun <T> trim(map: LinkedHashMap<String, T>) {
        while (map.size > KEPT_VIEWS) map.remove(map.keys.first())
    }

    private var background: Job? = null
    private var asleep = false

    /**
     * Tells the session whether the app is in the foreground. A minute in
     * the background closes the event socket, unless a call goes on; back in
     * the foreground it opens again and loads what may have changed.
     */
    fun setForeground(foreground: Boolean) {
        background?.cancel()
        background = null
        if (foreground) {
            // The socket's welcome catches the session up.
            if (asleep) {
                asleep = false
                socket.start()
            } else {
                socket.retryNow()
            }
            return
        }
        // The system may end the app in the background.
        savePts()
        background = scope.launch {
            delay(BACKGROUND_GRACE_MS)
            if (voice.state.value == null) {
                asleep = true
                socket.stop()
            }
        }
    }

    /** Tells the session which channel, and thread, the person looks at; null when none. */
    fun setVisible(channelId: String?, rootId: String? = null) {
        visibleChannel = channelId
        visibleThread = rootId
        // Who types reaches only the sockets that show the channel.
        socket.view(channelId)
        if (channelId != null) markRead(channelId)
        if (channelId != null && rootId != null) {
            scope.launch { runCatching { api.markThreadRead(channelId, rootId) } }
        }
    }

    /** Marks a channel read up to its newest message. */
    fun markRead(channelId: String) {
        val ch = _state.value.channel(channelId) ?: return
        val newest = timelines[channelId]?.state?.value?.messages?.firstOrNull()?.id ?: ch.last_message_id
        if (newest.isEmpty() || (ch.mention_count == 0 && !newer(newest, ch.last_read_id))) return
        updateChannel(channelId) { it.copy(last_read_id = newest, mention_count = 0) }
        scope.launch { runCatching { api.markRead(channelId, newest) } }
    }

    private fun updateChannel(id: String, f: (Channel) -> Channel) {
        _state.update { st -> st.copy(channels = st.channels.map { if (it.id == id) f(it) else it }) }
    }

    // Actions.

    /** Tells the channel's members the person types, at most every few seconds. */
    fun typing(channelId: String) {
        val now = TimeSource.Monotonic.markNow()
        val last = lastTyping[channelId]
        if (last != null && last.elapsedNow().inWholeMilliseconds < TYPING_EVERY_MS) return
        lastTyping[channelId] = now
        scope.launch { runCatching { api.typing(channelId) } }
    }

    suspend fun upload(channelId: String, name: String, bytes: ByteArray, voice: Boolean = false, durationMs: Long = 0): Attachment =
        api.client.upload(channelId, name, bytes, voice, durationMs)

    /** Adds the person's reaction, or takes it back when they had reacted so. */
    fun react(m: Message, emoji: String) {
        val mine = m.reactions.any { it.emoji == emoji && me.id in it.user_ids }
        scope.launch {
            runCatching { api.react(m, emoji, remove = mine) }.onSuccess { applyReactions(m.channel_id, m.id, m.root_id, it) }
        }
    }

    suspend fun edit(m: Message, content: String): String? = act { apply(api.edit(m, content)) }

    suspend fun delete(m: Message): String? = act {
        api.delete(m)
        removeMessage(m.channel_id, m.id, m.root_id)
    }

    suspend fun pin(m: Message, pinned: Boolean): String? = act { apply(api.pin(m, pinned)) }

    suspend fun save(m: Message, saved: Boolean): String? = act {
        api.save(m, saved)?.let { apply(it) }
        _state.update { st -> st.copy(saved = if (saved) st.saved + m.id else st.saved - m.id) }
    }

    suspend fun setMuted(channelId: String, muted: Boolean): String? = act {
        api.setMuted(channelId, muted)
        _state.update { st -> st.copy(muted = if (muted) st.muted + channelId else st.muted - channelId) }
    }

    suspend fun follow(channelId: String, rootId: String, follow: Boolean): String? = act {
        api.follow(channelId, rootId, follow)
        threads[rootId]?.setFollowing(follow)
    }

    /** Opens, or creates, the conversation with [userIds] in the team on screen. */
    suspend fun openConversation(userIds: List<String>): Channel {
        val team = _state.value.teamId ?: throw ApiException(0, "Pick a team first.")
        val ch = if (userIds.size == 1) api.openDirect(team, userIds[0]) else api.openGroup(team, userIds)
        if (ch != null) addChannel(ch)
        return ch ?: throw ApiException(0, "The server opened no conversation.")
    }

    /**
     * Deletes a direct conversation for the person, who sees it again with
     * its next message; with [everyone], for both people, for good.
     */
    suspend fun deleteConversation(channelId: String, everyone: Boolean): String? = act {
        api.deleteConversation(channelId, everyone)
        _state.update { st -> st.copy(channels = st.channels.filter { it.id != channelId }) }
        updates.forget(channelId)
    }

    suspend fun joinChannel(id: String): Channel? = api.joinChannel(id)?.also { addChannel(it) }

    /** Takes someone out of a voice channel's talk, for its moderators. */
    suspend fun disconnectVoice(channelId: String, userId: String): String? = act { api.disconnectVoiceMember(channelId, userId) }

    suspend fun leaveChannel(id: String): String? = act {
        api.leaveChannel(id)
        _state.update { st -> st.copy(channels = st.channels.filter { it.id != id }) }
        updates.forget(id)
    }

    suspend fun createChannel(req: gochat.v1.CreateChannelRequest): Channel? = api.createChannel(req)?.also { addChannel(it) }

    suspend fun setStatus(status: Status, dndMinutes: Int = 0): String? = act {
        api.setStatus(status, dndMinutes)?.let { u -> _state.update { it.copy(me = u) } }
    }

    suspend fun setCustomStatus(emoji: String, text: String, minutes: Int): String? = act {
        api.setCustomStatus(emoji, text, minutes)?.let { u -> _state.update { it.copy(me = u) } }
    }

    suspend fun updateProfile(displayName: String): String? = act {
        api.updateProfile(displayName)?.let { u -> _state.update { it.copy(me = u) } }
    }

    suspend fun setPhoto(picture: ByteArray): String? = act {
        val u = api.client.setPhoto(picture)
        _state.update { it.copy(me = u) }
    }

    suspend fun removePhoto(): String? = act {
        api.removePhoto()?.let { u -> _state.update { it.copy(me = u) } }
    }

    private suspend fun act(block: suspend () -> Unit): String? = try {
        block()
        null
    } catch (e: Exception) {
        if (e is ApiException && e.isUnauthenticated) onSignedOut("Your session ended. Sign in again.")
        e.userMessage()
    }

    private fun addChannel(ch: Channel) {
        _state.update { st -> st.copy(channels = st.channels.filter { it.id != ch.id } + ch) }
        updates.learn(ch.id, ch.pts)
    }

    /** Follows the updates of a channel read on its own, as a screen does for one not in the sidebar. */
    internal fun learn(ch: Channel) = updates.learn(ch.id, ch.pts)

    // Bots.

    private class Press(val at: TimeSource.Monotonic.ValueTimeMark, val mine: Boolean, val answer: CallbackAnswer? = null)

    // Buttons this device pressed, by query, until answered; and answers
    // that came before the press's own answer said it was this device's.
    private val presses = LinkedHashMap<String, Press>()

    /** Presses a button of a bot's message; the bot's answer comes in [answers]. Returns why not, or null. */
    suspend fun press(m: Message, data: String): String? = act {
        val queryId = api.pressButton(m, data)
        sweepPresses()
        val early = presses.remove(queryId)?.answer
        if (early != null) showAnswer(early) else presses[queryId] = Press(TimeSource.Monotonic.markNow(), mine = true)
    }

    private fun answered(a: CallbackAnswer) {
        val p = presses[a.query_id]
        if (p != null && p.mine) {
            presses.remove(a.query_id)
            showAnswer(a)
            return
        }
        sweepPresses()
        if (p == null && presses.size < MAX_PRESSES) presses[a.query_id] = Press(TimeSource.Monotonic.markNow(), mine = false, answer = a)
    }

    private fun sweepPresses() {
        presses.entries.removeAll { (_, p) -> p.at.elapsedNow() > if (p.mine) PRESS_TTL else EARLY_TTL }
    }

    private fun showAnswer(a: CallbackAnswer) {
        if (a.text.isNotEmpty()) _answers.tryEmit(BotAnswer(a.bot.displayName(), a.text, a.alert))
    }

    private val commands = HashMap<String, Pair<TimeSource.Monotonic.ValueTimeMark, List<ChannelCommand>>>()

    /** The commands of the channel's bots starting with [prefix], for the composer to suggest. */
    suspend fun commands(channelId: String, prefix: String): List<ChannelCommand> {
        val kept = commands[channelId]?.takeIf { it.first.elapsedNow() < COMMANDS_TTL }?.second
        val all = kept ?: try {
            api.channelCommands(channelId).sortedBy { it.command?.name }.also { commands[channelId] = TimeSource.Monotonic.markNow() to it }
        } catch (e: Exception) {
            if (e is kotlinx.coroutines.CancellationException) throw e
            return emptyList()
        }
        return all.filter { it.command?.name?.startsWith(prefix) == true }.take(MAX_SUGGESTIONS)
    }

    // Updates.

    private fun handle(e: SocketEvent) {
        when (e) {
            is SocketEvent.Welcome -> {
                val w = e.welcome
                _state.update { it.copy(me = w.user ?: it.me) }
                updates.welcome(w.pts, w.reload)
                // The first welcome loads everything, as does one after too
                // much was missed or a load that failed; the others catch up.
                sync(full = !synced || w.reload)
            }
            is SocketEvent.Updates -> updates.received(e.events)
            is SocketEvent.Ephemeral -> event(e.event)
        }
    }

    /** Applies one update; [quiet] when the sidebar's counts already hold it. */
    internal fun event(ev: Event, quiet: Boolean = false) {
        val st = _state.value
        ev.callback_answer?.let { answered(it) }
        ev.message_posted?.let { posted(it, quiet) }
        ev.message_edited?.message?.let { apply(it) }
        ev.message_deleted?.let { removeMessage(it.channel_id, it.message_id, it.root_id) }
        ev.typing?.let { t ->
            val u = t.user ?: return@let
            if (u.id != st.me.id) timelines[t.channel_id]?.typed(u)
        }
        ev.reactions_changed?.let { applyReactions(it.channel_id, it.message_id, it.root_id, it.reactions) }
        ev.channel_joined?.channel?.let { ch ->
            // Conversations belong to a team, as channels do.
            if (ch.team_id == st.teamId || (ch.isConversation && ch.team_id.isEmpty())) {
                _state.update { s -> s.copy(channels = s.channels.filter { it.id != ch.id } + ch) }
                updates.joined(ch.id, ch.pts)
            }
        }
        ev.channel_left?.let { left ->
            _state.update { s -> s.copy(channels = s.channels.filter { it.id != left.channel_id }) }
            timelines[left.channel_id]?.gone(left.own)
            updates.forget(left.channel_id)
        }
        ev.channel_updated?.channel?.let { ch ->
            if (st.channel(ch.id) != null) {
                // The event is built for the person who changed the channel:
                // keep what is the reader's own.
                updateChannel(ch.id) { old ->
                    old.copy(
                        name = ch.name, display_name = ch.display_name, purpose = ch.purpose, type = ch.type,
                        archived_at = ch.archived_at, voice_enabled = ch.voice_enabled, pinned_count = ch.pinned_count,
                    )
                }
                timelines[ch.id]?.refreshChannel()
            } else if (ch.team_id == st.teamId && !ch.isArchived) {
                scope.launch { loadSidebar() }
            }
        }
        ev.channel_muted?.let { mu ->
            _state.update { s -> s.copy(muted = if (mu.muted) s.muted + mu.channel_id else s.muted - mu.channel_id) }
        }
        ev.saved_changed?.let { sv ->
            _state.update { s -> s.copy(saved = if (sv.saved) s.saved + sv.message_id else s.saved - sv.message_id) }
            timelines[sv.channel_id]?.update(sv.message_id) { it.copy(saved = sv.saved) }
            threads.values.forEach { t -> t.update(sv.message_id) { it.copy(saved = sv.saved) } }
        }
        ev.pins_changed?.let { p -> updateChannel(p.channel_id) { it.copy(pinned_count = p.count) } }
        ev.presence?.let { p ->
            _state.update { s ->
                val status = statusFor(p.online, p.status)
                val me = if (p.user_id == s.me.id) s.me.copy(status = if (p.online) p.status else s.me.status) else s.me
                s.copy(presence = s.presence + (p.user_id to status), me = me)
            }
        }
        ev.profile_changed?.user?.let { u ->
            _state.update { s ->
                s.copy(
                    me = if (u.id == s.me.id) u else s.me,
                    channels = s.channels.map { ch ->
                        when {
                            ch.peer?.id == u.id -> ch.copy(peer = u)
                            ch.peers.any { it.id == u.id } -> ch.copy(peers = ch.peers.map { if (it.id == u.id) u else it })
                            else -> ch
                        }
                    },
                )
            }
        }
        ev.team_joined?.team?.let { t ->
            _state.update { s -> s.copy(teams = s.teams.filter { it.id != t.id } + t) }
            // While the first load runs, it picks the team.
            if (st.teamId == null && synced) selectTeam(t.id)
        }
        ev.team_left?.let { left ->
            _state.update { s -> s.copy(teams = s.teams.filter { it.id != left.team_id }) }
            if (st.teamId == left.team_id) {
                val next = _state.value.teams.firstOrNull()?.id
                _state.update { it.copy(teamId = null, channels = emptyList()) }
                if (next != null) selectTeam(next)
            }
        }
        ev.voice_channel_update?.let { up ->
            val ch = up.channel ?: return@let
            if (up.team_id != st.teamId) return@let
            _state.update { s ->
                val old = s.voice[ch.channel_id]
                if (old != null && old.version >= ch.version) s else s.copy(voice = s.voice + (ch.channel_id to ch))
            }
        }
    }

    private fun posted(p: gochat.v1.MessagePosted, quiet: Boolean) {
        val m = p.message ?: return
        val participants = p.participant_ids
        val st = _state.value
        val mine = m.author?.id == st.me.id
        val ch = st.channel(m.channel_id)
        // Another team's conversations and threads show in that team alone,
        // as on the web; its mentions still call for the person.
        val elsewhere = p.team_id.isNotEmpty() && p.team_id != st.teamId
        if (quiet) {
            // Caught up after a reconnect: the screens take it, the counts
            // came with the list.
            if (m.root_id.isNotEmpty()) {
                threads[m.root_id]?.added(m)
                timelines[m.channel_id]?.replied(m)
            } else {
                timelines[m.channel_id]?.added(m)
            }
            return
        }
        if (!mine && (!elsewhere || !p.conversation)) notify(m, ch, p.conversation, participants)
        if (m.root_id.isNotEmpty()) {
            threads[m.root_id]?.added(m)
            timelines[m.channel_id]?.replied(m)
            val looking = visibleThread == m.root_id
            if (!mine && !elsewhere && !looking && st.me.id in participants) {
                _state.update { it.copy(threadsUnread = it.threadsUnread + 1) }
            }
            if (looking && !mine) scope.launch { runCatching { api.markThreadRead(m.channel_id, m.root_id) } }
            // A reply that mentions the person counts on its channel's badge.
            if (!mine && !elsewhere && !looking && visibleChannel != m.channel_id && m.mentions(st.me)) {
                updateChannel(m.channel_id) { it.copy(mention_count = it.mention_count + 1) }
            }
            return
        }
        timelines[m.channel_id]?.added(m)
        if (elsewhere) return
        if (ch == null) {
            // A conversation someone else just opened here: it alone joins
            // the sidebar.
            if (p.conversation) scope.launch { runCatching { api.channel(m.channel_id) }.getOrNull()?.let { addChannel(it) } }
            return
        }
        val looking = visibleChannel == m.channel_id
        updateChannel(m.channel_id) { old ->
            var c = if (newer(m.id, old.last_message_id)) old.copy(last_message_id = m.id) else old
            if (mine || looking) {
                c = c.copy(last_read_id = m.id)
            } else if (ch.isConversation || m.mentions(st.me)) {
                c = c.copy(mention_count = c.mention_count + 1)
            }
            c
        }
        if (looking && !mine) scope.launch { runCatching { api.markRead(m.channel_id, m.id) } }
    }

    /**
     * Shows a notice of a message that calls for the person, by the web's
     * rules: a direct or group conversation, a reply in a thread they take
     * part in, or a mention; never in a muted channel, while they do not want
     * to be disturbed, or for what is on screen.
     */
    private fun notify(m: Message, ch: Channel?, conversation: Boolean, participants: List<String>) {
        val st = _state.value
        if (m.channel_id in st.muted || st.me.inDnd(Format.nowMillis())) return
        val rootId = m.root_id
        val onScreen = if (rootId.isNotEmpty()) visibleThread == rootId else visibleChannel == m.channel_id
        if (onScreen) return
        val reply = rootId.isNotEmpty() && st.me.id in participants
        if (!conversation && !reply && !m.mentions(st.me)) return
        // A message may come twice, before and after its channel was followed.
        if (m.id in notified) return
        notified += m.id
        if (notified.size > MAX_NOTIFIED) notified.remove(notified.first())
        val where = ch?.let { if (it.isConversation) "" else " in #${it.title()}" } ?: ""
        val title = if (rootId.isNotEmpty()) "${m.author.displayName()} replied$where" else "${m.author.displayName()}$where"
        _notices.tryEmit(Notice(title, m.excerpt(), m.channel_id, rootId))
    }

    private val notified = LinkedHashSet<String>()

    private fun apply(m: Message?) {
        if (m == null) return
        timelines[m.channel_id]?.update(m.id) { m }
        threads.values.forEach { t -> t.update(m.id) { m } }
    }

    private fun removeMessage(channelId: String, id: String, rootId: String) {
        timelines[channelId]?.removed(id, rootId)
        threads[rootId]?.removed(id)
        threads[id]?.rootGone()
    }

    private fun applyReactions(channelId: String, id: String, rootId: String, reactions: List<Reaction>) {
        timelines[channelId]?.update(id) { it.copy(reactions = reactions) }
        (threads[rootId] ?: threads[id])?.update(id) { it.copy(reactions = reactions) }
    }

    companion object {
        private const val KEY_TEAM = "team"

        /** The last account update applied, to resume after once the app restarts. */
        internal const val KEY_PTS = "pts"
        private const val PRESENCE_BATCH = 200
        private const val BACKGROUND_GRACE_MS = 60_000L
        private const val KEPT_VIEWS = 8
        private const val TYPING_EVERY_MS = 3_000L
        private const val SAVE_PTS_MS = 2_000L
        private const val MAX_NOTIFIED = 200

        // A press waits so long for its bot's answer; an answer that came
        // first waits so long for its press, among so many.
        private val PRESS_TTL = 2.minutes
        private val EARLY_TTL = 10.seconds
        private const val MAX_PRESSES = 64
        private val COMMANDS_TTL = 1.minutes
        private const val MAX_SUGGESTIONS = 8
    }
}
