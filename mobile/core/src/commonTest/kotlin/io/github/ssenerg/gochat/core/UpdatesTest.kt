package io.github.ssenerg.gochat.core

import gochat.v1.Event
import gochat.v1.GetChannelDifferenceResponse
import gochat.v1.GetDifferenceResponse
import gochat.v1.MessageDeleted
import gochat.v1.SavedChanged
import io.github.ssenerg.gochat.core.state.Updates
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/** Numbered updates apply in order and once, as docs/protocol.md says. */
@OptIn(ExperimentalCoroutinesApi::class)
class UpdatesTest {
    private class Seen(val seq: String, val pts: Long, val quiet: Boolean)

    private class Fixture(scope: TestScope) {
        val seen = mutableListOf<Seen>()
        val asked = mutableListOf<String>()
        var reloads = 0
        val reloadedChannels = mutableListOf<String>()
        var kept = 0L

        // What the server answers, by sequence.
        var account: (Long) -> GetDifferenceResponse = { GetDifferenceResponse(pts = it) }
        var channel: (String, Long) -> GetChannelDifferenceResponse = { _, pts -> GetChannelDifferenceResponse(pts = pts) }

        val updates = Updates(
            scope.backgroundScope,
            difference = { pts ->
                asked += "account after $pts"
                account(pts)
            },
            channelDifference = { id, pts ->
                asked += "$id after $pts"
                channel(id, pts)
            },
            apply = { ev, quiet -> seen += Seen(ev.pts_channel_id.ifEmpty { "account" }, ev.pts, quiet) },
            reload = { reloads++ },
            reloadChannel = { reloadedChannels += it },
            applied = { kept = it },
        )

        fun applied(seq: String = "account") = seen.filter { it.seq == seq }.map { it.pts }
    }

    private fun account(pts: Long) = Event(saved_changed = SavedChanged(message_id = "m$pts", saved = true), pts = pts, pts_count = 1)

    private fun channel(id: String, pts: Long) =
        Event(message_deleted = MessageDeleted(channel_id = id, message_id = "x$pts"), pts = pts, pts_count = 1, pts_channel_id = id)

    @Test
    fun accountUpdatesApplyOnceInOrder() = runTest {
        val f = Fixture(this)
        f.updates.welcome(10, reload = false)
        f.updates.received(listOf(account(11), account(12), account(11), account(12), account(13)))
        assertEquals(listOf(11L, 12L, 13L), f.applied())
        assertEquals(13, f.kept)
        assertTrue(f.asked.isEmpty())
    }

    @Test
    fun aGapWaitsForWhatIsMissing() = runTest {
        val f = Fixture(this)
        f.updates.welcome(10, reload = false)
        f.updates.received(listOf(account(12)))
        assertEquals(emptyList(), f.applied())
        advanceTimeBy(200)
        f.updates.received(listOf(account(11)))
        assertEquals(listOf(11L, 12L), f.applied())
        advanceTimeBy(1_000)
        runCurrent()
        assertTrue(f.asked.isEmpty(), "nothing was missing in the end")
    }

    @Test
    fun aGapThatStaysAsksForTheDifference() = runTest {
        val f = Fixture(this)
        f.account = { GetDifferenceResponse(updates = listOf(account(11), account(12), account(13)), pts = 13) }
        f.updates.welcome(10, reload = false)
        f.updates.received(listOf(account(13)))
        advanceTimeBy(600)
        runCurrent()
        assertEquals(listOf("account after 10"), f.asked)
        // 13 came live and is in the state: it is not applied twice.
        assertEquals(listOf(11L, 12L, 13L), f.applied())
        f.updates.received(listOf(account(14)))
        assertEquals(listOf(11L, 12L, 13L, 14L), f.applied())
    }

    @Test
    fun numbersThatHoldNothingAreSkipped() = runTest {
        val f = Fixture(this)
        // A server restart skipped 11 to 19: the difference holds nothing.
        f.account = { GetDifferenceResponse(pts = 19) }
        f.updates.welcome(10, reload = false)
        f.updates.received(listOf(account(20)))
        advanceTimeBy(600)
        runCurrent()
        assertEquals(listOf(20L), f.applied())
        assertEquals(20, f.kept)
    }

    @Test
    fun tooMuchMissedReloads() = runTest {
        val f = Fixture(this)
        f.account = { GetDifferenceResponse(pts = 500, too_long = true) }
        f.updates.welcome(10, reload = false)
        f.updates.received(listOf(account(400)))
        advanceTimeBy(600)
        runCurrent()
        assertEquals(1, f.reloads)
        assertEquals(500, f.updates.pts)
        f.updates.received(listOf(account(501)))
        assertEquals(listOf(501L), f.applied())
    }

    @Test
    fun aWelcomeResumesFromWhatWasApplied() = runTest {
        val f = Fixture(this)
        f.updates.start(30)
        // The server's welcome names the hello's number: it changes nothing.
        f.updates.welcome(30, reload = false)
        f.updates.received(listOf(account(31)))
        assertEquals(listOf(31L), f.applied())
        // One that says to reload takes the server's number.
        f.updates.welcome(90, reload = true)
        assertEquals(90, f.updates.pts)
        f.updates.received(listOf(account(60), account(91)))
        assertEquals(listOf(31L, 91L), f.applied())
    }

    @Test
    fun channelsCountTheirOwnOnceKnown() = runTest {
        val f = Fixture(this)
        f.updates.welcome(1, reload = false)
        // Not followed yet: as it comes, for the notices.
        f.updates.received(listOf(channel("c1", 7)))
        assertEquals(listOf(7L), f.applied("c1"))
        f.updates.learn("c1", 7)
        f.updates.received(listOf(channel("c1", 7), channel("c1", 8), channel("c2", 3)))
        assertEquals(listOf(7L, 8L), f.applied("c1"))
        assertEquals(listOf(3L), f.applied("c2"))
    }

    @Test
    fun aChannelGapAsksForThatChannel() = runTest {
        val f = Fixture(this)
        f.channel = { _, _ -> GetChannelDifferenceResponse(updates = listOf(channel("c1", 6), channel("c1", 7)), pts = 7) }
        f.updates.learn("c1", 5)
        f.updates.received(listOf(channel("c1", 8)))
        advanceTimeBy(600)
        runCurrent()
        assertEquals(listOf("c1 after 5"), f.asked)
        assertEquals(listOf(6L, 7L, 8L), f.applied("c1"))
    }

    @Test
    fun heldUpdatesWaitForTheLoad() = runTest {
        val f = Fixture(this)
        f.updates.welcome(1, reload = false)
        f.updates.hold()
        f.updates.received(listOf(channel("c1", 4), channel("c1", 5), account(2)))
        assertTrue(f.seen.isEmpty())
        // The load read the channel at 4: only what came after applies.
        f.updates.learn("c1", 4)
        f.updates.release()
        assertEquals(listOf(5L), f.applied("c1"))
        assertEquals(listOf(2L), f.applied())
    }

    @Test
    fun joiningCatchesUpFromTheChannelsNumber() = runTest {
        val f = Fixture(this)
        f.channel = { _, pts ->
            if (pts == 3L) GetChannelDifferenceResponse(updates = listOf(channel("c9", 4)), pts = 4) else GetChannelDifferenceResponse(pts = pts)
        }
        f.updates.joined("c9", 3)
        runCurrent()
        assertEquals(listOf("c9 after 3"), f.asked)
        assertEquals(listOf(4L), f.applied("c9"))
        // A join replayed from long ago changes nothing.
        f.updates.joined("c9", 1)
        runCurrent()
        assertEquals(1, f.asked.size)
    }

    @Test
    fun catchingUpIsQuietUpToTheList() = runTest {
        val f = Fixture(this)
        f.channel = { _, _ -> GetChannelDifferenceResponse(updates = listOf(channel("c1", 3), channel("c1", 4), channel("c1", 5)), pts = 5) }
        f.updates.learn("c1", 2)
        assertTrue(f.updates.fill("c1", quietThrough = 4))
        assertEquals(listOf(true, true, false), f.seen.map { it.quiet })
    }

    @Test
    fun tooMuchMissedInAChannelReloadsIt() = runTest {
        val f = Fixture(this)
        f.channel = { _, _ -> GetChannelDifferenceResponse(pts = 900, too_long = true) }
        f.updates.learn("c1", 2)
        f.updates.fill("c1")
        assertEquals(listOf("c1"), f.reloadedChannels)
        f.updates.received(listOf(channel("c1", 901)))
        assertEquals(listOf(901L), f.applied("c1"))
    }

    @Test
    fun aLeftChannelIsNoLongerFollowed() = runTest {
        val f = Fixture(this)
        f.updates.learn("c1", 2)
        f.updates.received(listOf(channel("c1", 5)))
        f.updates.forget("c1")
        advanceTimeBy(600)
        runCurrent()
        assertTrue(f.asked.isEmpty())
    }
}
