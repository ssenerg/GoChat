package io.github.ssenerg.gochat.core

import com.squareup.wire.ofEpochSecond
import gochat.v1.Channel
import gochat.v1.ChannelType
import gochat.v1.Event
import gochat.v1.ListChannelsResponse
import gochat.v1.ListMutedChannelsResponse
import gochat.v1.Message
import gochat.v1.MessagePosted
import gochat.v1.Status
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.Caller
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.state.Notice
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/** The session follows the web's rules for what notifies and what counts. */
class ChatSessionTest {
    private val ann = User(id = "u-ann", username = "ann", display_name = "Ann")
    private val bob = User(id = "u-bob", username = "bob", display_name = "Bob")
    private val town = Channel(id = "c-town", team_id = "t1", name = "town", display_name = "Town", type = ChannelType.CHANNEL_TYPE_PUBLIC)
    private val quiet = Channel(id = "c-quiet", team_id = "t1", name = "quiet", display_name = "Quiet", type = ChannelType.CHANNEL_TYPE_PUBLIC)
    private val direct = Channel(id = "c-bob", team_id = "t1", type = ChannelType.CHANNEL_TYPE_DIRECT, peer = bob)

    private class Opened(val session: ChatSession, val notices: List<Notice>)

    /** Ann's session in team t1, with "quiet" muted. */
    private suspend fun TestScope.open(me: User = ann): Opened {
        val calls = Caller { method, _ ->
            when (method.substringAfterLast('/')) {
                "ListChannels" -> ListChannelsResponse(channels = listOf(town, quiet, direct)).encode()
                "ListMutedChannels" -> ListMutedChannelsResponse(channel_ids = listOf(quiet.id)).encode()
                else -> throw ApiException(ApiException.NOT_FOUND, "not found")
            }
        }
        val session = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = calls)), me, MemoryStore(), backgroundScope, null) {}
        val notices = mutableListOf<Notice>()
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) { session.notices.collect { notices += it } }
        session.selectTeam("t1")
        session.state.first { it.channels.isNotEmpty() && it.muted.isNotEmpty() }
        return Opened(session, notices)
    }

    private var seq = 0

    private fun posted(
        channel: Channel,
        content: String,
        team: String = "t1",
        rootId: String = "",
        participants: List<String> = emptyList(),
    ) = Event(
        message_posted = MessagePosted(
            message = Message(id = "d0f1c0${seq++.toString().padStart(2, '0')}-b1a2-11f0-8000-000000000001", channel_id = channel.id, author = bob, content = content, root_id = rootId),
            team_id = team,
            conversation = channel.type == ChannelType.CHANNEL_TYPE_DIRECT,
            participant_ids = participants,
        ),
    )

    private fun ChatSession.mentions(channel: Channel) = state.value.channel(channel.id)?.mention_count

    @Test
    fun mentionsAndConversationsNotifyAndCount() = runTest {
        val o = open()
        o.session.event(posted(town, "hello all"))
        o.session.event(posted(town, "hey @ann"))
        o.session.event(posted(direct, "psst"))
        assertEquals(listOf("Bob in #Town", "Bob"), o.notices.map { it.title })
        assertEquals(1, o.session.mentions(town))
        assertEquals(1, o.session.mentions(direct))
    }

    @Test
    fun anotherTeamsConversationsStayThere() = runTest {
        val o = open()
        o.session.event(posted(direct.copy(id = "c-bob-t2", team_id = "t2"), "psst", team = "t2"))
        o.session.event(posted(town.copy(id = "c-town-t2", team_id = "t2"), "reply", team = "t2", rootId = "root", participants = listOf(ann.id)))
        assertEquals(0, o.session.state.value.threadsUnread)
        // A mention there still calls for her, as on the web.
        o.session.event(posted(town.copy(id = "c-elsewhere", team_id = "t2"), "@ann look", team = "t2"))
        assertEquals(listOf("Bob replied", "Bob"), o.notices.map { it.title })
    }

    @Test
    fun mutedChannelsCountButDoNotNotify() = runTest {
        val o = open()
        o.session.event(posted(quiet, "hey @ann"))
        assertTrue(o.notices.isEmpty())
        assertEquals(1, o.session.mentions(quiet))
    }

    @Test
    fun repliesCallForTheThreadsParticipants() = runTest {
        val o = open()
        o.session.event(posted(town, "a reply", rootId = "root-1", participants = listOf(bob.id)))
        assertTrue(o.notices.isEmpty())
        assertEquals(0, o.session.state.value.threadsUnread)

        o.session.event(posted(town, "another", rootId = "root-1", participants = listOf(bob.id, ann.id)))
        assertEquals(listOf("Bob replied in #Town"), o.notices.map { it.title })
        assertEquals(1, o.session.state.value.threadsUnread)

        // A reply that mentions her counts on the channel's badge.
        o.session.event(posted(town, "@ann see this", rootId = "root-2", participants = listOf(bob.id)))
        assertEquals(1, o.session.mentions(town))
    }

    @Test
    fun doNotDisturbHoldsUntilItEnds() = runTest {
        val now = Format.nowMillis() / 1000
        val held = open(ann.copy(status = Status.STATUS_DND, dnd_until = ofEpochSecond(now + 3600, 0)))
        held.session.event(posted(direct, "psst"))
        assertTrue(held.notices.isEmpty())

        val forever = open(ann.copy(status = Status.STATUS_DND))
        forever.session.event(posted(direct, "psst"))
        assertTrue(forever.notices.isEmpty())

        val ended = open(ann.copy(status = Status.STATUS_DND, dnd_until = ofEpochSecond(now - 60, 0)))
        ended.session.event(posted(direct, "psst"))
        assertEquals(1, ended.notices.size)
    }
}
