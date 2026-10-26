package io.github.ssenerg.gochat.core.state

import gochat.v1.Channel
import gochat.v1.Message
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.newer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlin.random.Random

/** A message on its way, shown until the server has it. */
data class Pending(val localId: String, val content: String, val attachmentIds: List<String>, val failed: Boolean = false)

/**
 * A channel's messages as its screen shows them: the newest first, older
 * pages on demand, kept current by the session's events.
 */
class Timeline internal constructor(
    val channelId: String,
    private val session: ChatSession,
    private val scope: CoroutineScope,
) {
    data class State(
        val channel: Channel? = null,
        /** Newest first. */
        val messages: List<Message> = emptyList(),
        val hasMore: Boolean = false,
        val loading: Boolean = true,
        val loadingOlder: Boolean = false,
        val error: String? = null,
        /** Who types now. */
        val typing: List<User> = emptyList(),
        val pending: List<Pending> = emptyList(),
        /** Set when the person left the channel, or was removed from it. */
        val gone: Boolean = false,
        val removed: Boolean = false,
    )

    private val _state = MutableStateFlow(State())
    val state: StateFlow<State> = _state.asStateFlow()

    private val typingJobs = mutableMapOf<String, Job>()

    // What arrives while a page loads, which the page may have missed.
    private var paging = false
    private val arrived = mutableListOf<Message>()

    internal fun reload() {
        scope.launch { load() }
    }

    private suspend fun load() {
        _state.update { it.copy(loading = it.messages.isEmpty(), error = null) }
        paging = true
        arrived.clear()
        try {
            val channel = session.state.value.channel(channelId) ?: session.api.channel(channelId)?.also { session.learn(it) }
            val page = session.api.history(channelId, limit = PAGE)
            val messages = (page.messages + arrived.filter { m -> page.messages.none { it.id == m.id } })
                .sortedByDescending { io.github.ssenerg.gochat.core.model.uuidTime(it.id) }
            _state.update { it.copy(channel = channel, messages = messages, hasMore = page.has_more, loading = false) }
        } catch (e: Exception) {
            _state.update { it.copy(loading = false, error = e.userMessage()) }
        } finally {
            paging = false
            arrived.clear()
        }
    }

    internal fun refreshChannel() {
        _state.update { it.copy(channel = session.state.value.channel(channelId) ?: it.channel) }
    }

    /** Loads the page before the oldest message shown. */
    fun loadOlder() {
        val st = _state.value
        if (st.loadingOlder || !st.hasMore || st.messages.isEmpty()) return
        _state.update { it.copy(loadingOlder = true) }
        scope.launch {
            try {
                val page = session.api.history(channelId, beforeId = st.messages.last().id, limit = PAGE)
                _state.update { s ->
                    val known = s.messages.mapTo(HashSet()) { it.id }
                    s.copy(messages = s.messages + page.messages.filter { it.id !in known }, hasMore = page.has_more, loadingOlder = false)
                }
            } catch (e: Exception) {
                _state.update { it.copy(loadingOlder = false, error = e.userMessage()) }
            }
        }
    }

    /** Posts a message; it shows as pending until the server has it. */
    fun send(content: String, attachmentIds: List<String> = emptyList()) {
        val text = content.trim()
        if (text.isEmpty() && attachmentIds.isEmpty()) return
        val p = Pending(localId = "local-" + Random.nextLong().toString(16), content = text, attachmentIds = attachmentIds)
        _state.update { it.copy(pending = it.pending + p) }
        post(p)
    }

    fun retry(p: Pending) {
        _state.update { s -> s.copy(pending = s.pending.map { if (it.localId == p.localId) it.copy(failed = false) else it }) }
        post(p)
    }

    fun discard(p: Pending) {
        _state.update { s -> s.copy(pending = s.pending.filter { it.localId != p.localId }) }
    }

    private fun post(p: Pending) {
        scope.launch {
            try {
                val m = session.api.post(channelId, p.content, p.attachmentIds)
                _state.update { s -> s.copy(pending = s.pending.filter { it.localId != p.localId }) }
                if (m != null) added(m)
                session.markRead(channelId)
            } catch (e: Exception) {
                _state.update { s ->
                    s.copy(
                        pending = s.pending.map { if (it.localId == p.localId) it.copy(failed = true) else it },
                        error = e.userMessage(),
                    )
                }
            }
        }
    }

    fun clearError() = _state.update { it.copy(error = null) }

    internal fun added(m: Message) {
        if (paging) arrived += m
        _state.update { s ->
            if (s.loading || s.messages.any { it.id == m.id }) return@update s
            val typing = s.typing.filter { it.id != m.author?.id }
            // Newest first; a message seldom arrives out of order.
            val list = if (s.messages.isEmpty() || newer(m.id, s.messages.first().id)) {
                listOf(m) + s.messages
            } else {
                (s.messages + m).sortedByDescending { io.github.ssenerg.gochat.core.model.uuidTime(it.id) }
            }
            s.copy(messages = list, typing = typing)
        }
    }

    /** A reply posted in the thread of one of the messages shown. */
    internal fun replied(reply: Message) {
        update(reply.root_id) { it.copy(reply_count = it.reply_count + 1, last_reply_at = reply.created_at ?: it.last_reply_at) }
    }

    internal fun update(id: String, f: (Message) -> Message) {
        _state.update { s -> if (s.messages.none { it.id == id }) s else s.copy(messages = s.messages.map { if (it.id == id) f(it) else it }) }
    }

    internal fun removed(id: String, rootId: String) {
        if (rootId.isNotEmpty()) {
            update(rootId) { it.copy(reply_count = maxOf(0, it.reply_count - 1)) }
            return
        }
        _state.update { s -> s.copy(messages = s.messages.filter { it.id != id }) }
    }

    internal fun typed(u: User) {
        _state.update { s -> if (s.typing.any { it.id == u.id }) s else s.copy(typing = s.typing + u) }
        typingJobs[u.id]?.cancel()
        typingJobs[u.id] = scope.launch {
            delay(TYPING_LINGER_MS)
            _state.update { s -> s.copy(typing = s.typing.filter { it.id != u.id }) }
        }
    }

    internal fun gone(own: Boolean) {
        _state.update { it.copy(gone = true, removed = !own) }
    }

    companion object {
        private const val PAGE = 50
        private const val TYPING_LINGER_MS = 6_000L
    }
}

/** A thread: its root and replies, the oldest first. */
class ThreadView internal constructor(
    val channelId: String,
    val rootId: String,
    private val session: ChatSession,
    private val scope: CoroutineScope,
) {
    data class State(
        val root: Message? = null,
        /** Oldest first. */
        val replies: List<Message> = emptyList(),
        val hasMore: Boolean = false,
        val following: Boolean = false,
        val loading: Boolean = true,
        val error: String? = null,
        val pending: List<Pending> = emptyList(),
        /** Set when the root was deleted. */
        val gone: Boolean = false,
    )

    private val _state = MutableStateFlow(State())
    val state: StateFlow<State> = _state.asStateFlow()

    // What arrives while the thread loads, which the load may have missed.
    private var paging = false
    private val arrived = mutableListOf<Message>()

    internal fun reload() {
        scope.launch {
            _state.update { it.copy(loading = it.root == null, error = null) }
            paging = true
            arrived.clear()
            try {
                val t = session.api.thread(channelId, rootId)
                val replies = (t.replies + arrived.filter { m -> t.replies.none { it.id == m.id } })
                    .sortedBy { io.github.ssenerg.gochat.core.model.uuidTime(it.id) }
                _state.update { it.copy(root = t.root, replies = replies, hasMore = t.has_more, following = t.following, loading = false) }
            } catch (e: Exception) {
                _state.update { it.copy(loading = false, error = e.userMessage()) }
            } finally {
                paging = false
                arrived.clear()
            }
        }
    }

    private var older: Job? = null

    fun loadOlder() {
        val st = _state.value
        if (!st.hasMore || st.replies.isEmpty() || older?.isActive == true) return
        older = scope.launch {
            runCatching { session.api.thread(channelId, rootId, beforeId = st.replies.first().id) }.onSuccess { t ->
                _state.update { s ->
                    val known = s.replies.mapTo(HashSet()) { it.id }
                    s.copy(replies = t.replies.filter { it.id !in known } + s.replies, hasMore = t.has_more)
                }
            }
        }
    }

    fun send(content: String, attachmentIds: List<String> = emptyList()) {
        val text = content.trim()
        if (text.isEmpty() && attachmentIds.isEmpty()) return
        val p = Pending(localId = "local-" + Random.nextLong().toString(16), content = text, attachmentIds = attachmentIds)
        _state.update { it.copy(pending = it.pending + p) }
        post(p)
    }

    fun retry(p: Pending) {
        _state.update { s -> s.copy(pending = s.pending.map { if (it.localId == p.localId) it.copy(failed = false) else it }) }
        post(p)
    }

    fun discard(p: Pending) {
        _state.update { s -> s.copy(pending = s.pending.filter { it.localId != p.localId }) }
    }

    private fun post(p: Pending) {
        scope.launch {
            try {
                val m = session.api.post(channelId, p.content, p.attachmentIds, rootId)
                _state.update { s -> s.copy(pending = s.pending.filter { it.localId != p.localId }, following = true) }
                if (m != null) added(m)
            } catch (e: Exception) {
                _state.update { s ->
                    s.copy(pending = s.pending.map { if (it.localId == p.localId) it.copy(failed = true) else it }, error = e.userMessage())
                }
            }
        }
    }

    fun clearError() = _state.update { it.copy(error = null) }

    internal fun setFollowing(following: Boolean) = _state.update { it.copy(following = following) }

    internal fun added(m: Message) {
        if (paging) arrived += m
        _state.update { s ->
            if (s.loading || s.replies.any { it.id == m.id }) s else s.copy(replies = s.replies + m)
        }
    }

    internal fun update(id: String, f: (Message) -> Message) {
        _state.update { s ->
            when {
                s.root?.id == id -> s.copy(root = f(s.root))
                s.replies.any { it.id == id } -> s.copy(replies = s.replies.map { if (it.id == id) f(it) else it })
                else -> s
            }
        }
    }

    internal fun removed(id: String) {
        _state.update { s -> s.copy(replies = s.replies.filter { it.id != id }) }
    }

    internal fun rootGone() {
        _state.update { it.copy(gone = true) }
    }
}
