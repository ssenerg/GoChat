package io.github.ssenerg.gochat.core.state

import gochat.v1.Event
import gochat.v1.GetChannelDifferenceResponse
import gochat.v1.GetDifferenceResponse
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Applies numbered updates in order, once each, as docs/protocol.md says:
 * the account's sequence, and that of each channel whose number it knows.
 * An update after a gap waits half a second for the missing ones, then the
 * server is asked for the difference. Use it from the session's thread.
 */
internal class Updates(
    private val scope: CoroutineScope,
    private val difference: suspend (pts: Long) -> GetDifferenceResponse,
    private val channelDifference: suspend (channelId: String, pts: Long) -> GetChannelDifferenceResponse,
    /** Applies an update; [quiet] when the sidebar already counts it, as after a reconnect. */
    private val apply: (event: Event, quiet: Boolean) -> Unit,
    /** Too much was missed: load everything again. */
    private val reload: () -> Unit,
    /** Too much was missed in a channel: load what shows of it again. */
    private val reloadChannel: (String) -> Unit,
    /** The account's number moved on. */
    private val applied: (Long) -> Unit,
) {
    /** The account's last update applied. */
    var pts: Long = 0
        private set

    // Each channel's last update applied, by id.
    private val channels = HashMap<String, Long>()

    // Updates past a gap, by sequence: "" is the account's.
    private val held = HashMap<String, MutableList<Event>>()
    private val waits = HashMap<String, Job>()
    private val filling = HashSet<String>()

    // While held, updates queue: the session is loading what they change.
    private var holds = 0
    private val queue = ArrayList<Event>()

    /** Starts from the number the app kept, 0 for none. */
    fun start(pts: Long) {
        this.pts = pts
    }

    /** A socket's welcome: the stream goes on from [pts], or from scratch with [reload]. */
    fun welcome(pts: Long, reload: Boolean) {
        if (reload || this.pts == 0L) {
            set(ACCOUNT, pts)
            drain(ACCOUNT)
        }
    }

    fun received(events: List<Event>) {
        if (holds > 0) queue += events else events.forEach { route(it) }
    }

    /** Queues updates until [release]; holds nest. */
    fun hold() {
        holds++
    }

    fun release() {
        if (holds == 0 || --holds > 0) return
        val waiting = queue.toList()
        queue.clear()
        waiting.forEach { route(it) }
    }

    fun knows(channelId: String) = channelId in channels

    /** Takes a channel's number from a fresh read of it, unless the one kept is newer. */
    fun learn(channelId: String, pts: Long) {
        val local = channels[channelId]
        if (local == null || pts > local) {
            channels[channelId] = pts
            drain(channelId)
        }
    }

    /**
     * The person joined a channel: its updates count from [pts]. Some may
     * have come before the join did, so the difference follows.
     */
    fun joined(channelId: String, pts: Long) {
        val local = channels[channelId]
        if (local != null && local >= pts) return
        channels[channelId] = pts
        scope.launch { fill(channelId) }
    }

    fun forget(channelId: String) {
        channels.remove(channelId)
        held.remove(channelId)
        waits.remove(channelId)?.cancel()
    }

    /** Forgets every channel but [keep]: those of another team, once it is left for this one. */
    fun keepOnly(keep: Set<String>) {
        channels.keys.filter { it !in keep }.forEach { forget(it) }
    }

    private fun local(key: String): Long? = if (key == ACCOUNT) pts else channels[key]

    private fun set(key: String, value: Long) {
        if (key == ACCOUNT) {
            pts = value
            applied(value)
        } else {
            channels[key] = value
        }
    }

    private fun route(ev: Event) {
        val key = ev.pts_channel_id
        val local = local(key)
        // Unnumbered, or of a channel not followed: as it comes.
        if (ev.pts <= 0L || local == null) {
            apply(ev, false)
            return
        }
        val count = maxOf(ev.pts_count, 1)
        when {
            local + count == ev.pts -> {
                apply(ev, false)
                set(key, ev.pts)
                drain(key)
            }
            local + count > ev.pts -> {} // applied already
            else -> {
                held.getOrPut(key) { mutableListOf() } += ev
                wait(key)
            }
        }
    }

    /** Applies what was held and now follows on. */
    private fun drain(key: String) {
        val list = held[key] ?: return
        list.sortBy { it.pts }
        while (list.isNotEmpty()) {
            val local = local(key) ?: break
            val ev = list.first()
            val count = maxOf(ev.pts_count, 1)
            if (local + count < ev.pts) break
            list.removeAt(0)
            if (local + count == ev.pts) {
                apply(ev, false)
                set(key, ev.pts)
            }
        }
        if (list.isEmpty()) held.remove(key)
    }

    private fun wait(key: String) {
        if (waits[key]?.isActive == true) return
        waits[key] = scope.launch {
            delay(GAP_WAIT_MS)
            if (held[key].isNullOrEmpty()) return@launch
            val filled = if (key == ACCOUNT) catchUp() else fill(key)
            if (filled) skip(key)
        }
    }

    /**
     * After the difference, what is still held lies past numbers that hold
     * nothing, as after a server restart: it applies in order.
     */
    private fun skip(key: String) {
        drain(key)
        val rest = held.remove(key) ?: return
        for (ev in rest.sortedBy { it.pts }) {
            val local = local(key) ?: return
            if (ev.pts > local) {
                apply(ev, false)
                set(key, ev.pts)
            }
        }
    }

    /** Asks for the account's updates after its number; false when the server could not be asked. */
    private suspend fun catchUp(): Boolean {
        if (!filling.add(ACCOUNT)) return false
        try {
            while (true) {
                val from = pts
                val res = try {
                    difference(from)
                } catch (e: CancellationException) {
                    throw e
                } catch (_: Exception) {
                    return false
                }
                if (res.too_long) {
                    set(ACCOUNT, res.pts)
                    held.remove(ACCOUNT)
                    reload()
                    return true
                }
                for (ev in res.updates) {
                    if (ev.pts > pts) {
                        apply(ev, false)
                        set(ACCOUNT, ev.pts)
                    }
                }
                if (res.pts > pts) set(ACCOUNT, res.pts)
                if (!res.more || res.pts <= from) break
            }
            drain(ACCOUNT)
            return true
        } finally {
            filling.remove(ACCOUNT)
        }
    }

    /**
     * Asks for a channel's updates after its number and applies them, those
     * up to [quietThrough] quietly. False when the server could not be asked.
     */
    suspend fun fill(channelId: String, quietThrough: Long = 0): Boolean {
        if (!filling.add(channelId)) return false
        try {
            while (true) {
                val from = channels[channelId] ?: return true
                val res = try {
                    channelDifference(channelId, from)
                } catch (e: CancellationException) {
                    throw e
                } catch (_: Exception) {
                    return false
                }
                // Left meanwhile.
                if (channels[channelId] == null) return true
                if (res.too_long) {
                    channels[channelId] = res.pts
                    held.remove(channelId)
                    reloadChannel(channelId)
                    return true
                }
                for (ev in res.updates) {
                    val local = channels[channelId] ?: return true
                    if (ev.pts > local) {
                        apply(ev, ev.pts <= quietThrough)
                        channels[channelId] = ev.pts
                    }
                }
                val local = channels[channelId] ?: return true
                if (res.pts > local) channels[channelId] = res.pts
                if (!res.more || res.pts <= from) break
            }
            drain(channelId)
            return true
        } finally {
            filling.remove(channelId)
        }
    }

    companion object {
        private const val ACCOUNT = ""

        /** How long an update after a gap waits for the missing ones. */
        const val GAP_WAIT_MS = 500L
    }
}
