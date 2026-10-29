package io.github.ssenerg.gochat.core

import com.squareup.wire.Message
import com.squareup.wire.ProtoAdapter
import gochat.v1.BlockUserRequest
import gochat.v1.BlockUserResponse
import gochat.v1.BlockedUser
import gochat.v1.BlocksChanged
import gochat.v1.Channel
import gochat.v1.ChannelType
import gochat.v1.CountReportsResponse
import gochat.v1.Event
import gochat.v1.ListBlockedResponse
import gochat.v1.ListReportsRequest
import gochat.v1.ListReportsResponse
import gochat.v1.MessagePosted
import gochat.v1.Report
import gochat.v1.ReportAction
import gochat.v1.ReportReason
import gochat.v1.ReportRequest
import gochat.v1.ReportResponse
import gochat.v1.ReportsChanged
import gochat.v1.ResolveReportRequest
import gochat.v1.ResolveReportResponse
import gochat.v1.Team
import gochat.v1.TeamJoined
import gochat.v1.TeamRole
import gochat.v1.UnblockUserRequest
import gochat.v1.UnblockUserResponse
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.Caller
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.state.Notice
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Blocking people and reporting messages and people: the calls and what the session keeps. */
class SafetyTest {
    /** The calls made, by method, and their requests. */
    private class Recorder {
        val made = mutableListOf<Pair<String, ByteArray>>()
        val answers = mutableMapOf<String, Message<*, *>>()

        val caller = Caller { method, payload ->
            made += method to payload
            answers[method.substringAfterLast('/')]?.encode() ?: throw ApiException(ApiException.NOT_FOUND, "not found")
        }

        fun <T : Any> sent(method: String, adapter: ProtoAdapter<T>): T =
            adapter.decode(made.last { it.first == method }.second)
    }

    private fun api(r: Recorder) = GoChatApi(ApiClient("https://chat.test", "tok", caller = r.caller))

    private val me = User(id = "u-ann", username = "ann")
    private val bob = User(id = "u-bob", username = "bob", display_name = "Bob")
    private val cat = User(id = "u-cat", username = "cat", display_name = "Cat")

    @Test
    fun blockingGoesToTheSafetyService() = runTest {
        val r = Recorder()
        r.answers["BlockUser"] = BlockUserResponse()
        r.answers["UnblockUser"] = UnblockUserResponse()
        r.answers["ListBlocked"] = ListBlockedResponse(blocked = listOf(BlockedUser(user = bob)))
        val session = ChatSession(api(r), me, MemoryStore(), backgroundScope, null) {}

        assertNull(session.block("u-bob", true))
        assertEquals("u-bob", r.sent("gochat.v1.SafetyService/BlockUser", BlockUserRequest.ADAPTER).user_id)
        assertEquals(setOf("u-bob"), session.state.value.blocked)
        assertNull(session.block("u-bob", false))
        assertEquals("u-bob", r.sent("gochat.v1.SafetyService/UnblockUser", UnblockUserRequest.ADAPTER).user_id)
        assertTrue(session.state.value.blocked.isEmpty())

        // The list for Settings also brings the session up to date.
        assertEquals(listOf("u-bob"), session.blockedPeople().map { it.user?.id })
        assertEquals(setOf("u-bob"), session.state.value.blocked)

        // A block made on another device comes as an update.
        session.event(Event(blocks_changed = BlocksChanged(user = cat, blocked = true)))
        assertEquals(setOf("u-bob", "u-cat"), session.state.value.blocked)
        session.event(Event(blocks_changed = BlocksChanged(user = bob, blocked = false)))
        assertEquals(setOf("u-cat"), session.state.value.blocked)
    }

    @Test
    fun aBlockedPersonsMessagesCallForNothing() = runTest {
        val session = ChatSession(api(Recorder()), me, MemoryStore(), backgroundScope, null) {}
        val dm = Channel(id = "c-dm", type = ChannelType.CHANNEL_TYPE_GROUP, team_id = "", peers = listOf(bob, cat))
        val notices = mutableListOf<Notice>()
        backgroundScope.launch { session.notices.toList(notices) }
        session.event(Event(channel_joined = gochat.v1.ChannelJoined(channel = dm)))
        session.event(Event(blocks_changed = BlocksChanged(user = bob, blocked = true)))
        fun post(id: String, from: User) = session.event(
            Event(message_posted = MessagePosted(message = gochat.v1.Message(id = id, channel_id = dm.id, author = from, content = "@ann hi"), conversation = true)),
        )

        post("m1", bob)
        runCurrent()
        assertTrue(notices.isEmpty())
        assertEquals(0, session.state.value.channel(dm.id)?.mention_count)
        assertTrue(session.state.value.fromBlocked(gochat.v1.Message(author = bob)))
        assertFalse(session.state.value.fromBlocked(gochat.v1.Message(author = cat)))
        assertFalse(session.state.value.fromBlocked(gochat.v1.Message(author = me)))

        post("m2", cat)
        runCurrent()
        assertEquals(listOf("Cat"), notices.map { it.title })
        assertEquals(1, session.state.value.channel(dm.id)?.mention_count)
    }

    @Test
    fun reportsGoToTheAdminsAndCountForThem() = runTest {
        val r = Recorder()
        r.answers["Report"] = ReportResponse()
        r.answers["ListReports"] = ListReportsResponse(reports = listOf(Report(id = "r1", team_id = "t1")), next = "r1")
        r.answers["ResolveReport"] = ResolveReportResponse(report = Report(id = "r1", action = ReportAction.REPORT_ACTION_DISMISS))
        val api = api(r)
        val session = ChatSession(api, me, MemoryStore(), backgroundScope, null) {}

        val m = gochat.v1.Message(id = "m1", channel_id = "c1", root_id = "m0", author = bob)
        assertNull(session.report(m, ReportReason.REPORT_REASON_HARASSMENT, "again"))
        val sent = r.sent("gochat.v1.SafetyService/Report", ReportRequest.ADAPTER)
        assertEquals(ReportReason.REPORT_REASON_HARASSMENT, sent.reason)
        assertEquals("again", sent.details)
        assertEquals(Triple("c1", "m1", "m0"), Triple(sent.message?.channel_id, sent.message?.message_id, sent.message?.root_id))

        assertNull(session.reportPerson("t1", "u-bob", ReportReason.REPORT_REASON_SPAM, ""))
        val person = r.sent("gochat.v1.SafetyService/Report", ReportRequest.ADAPTER)
        assertEquals("t1" to "u-bob", person.user?.team_id to person.user?.user_id)
        assertNull(person.message)

        val page = api.reports("t1", after = "r0")
        assertEquals("r1", page.next)
        assertEquals("t1" to "r0", r.sent("gochat.v1.SafetyService/ListReports", ListReportsRequest.ADAPTER).let { it.team_id to it.after })
        assertNull(session.resolveReport("r1", ReportAction.REPORT_ACTION_DISMISS))
        val resolved = r.sent("gochat.v1.SafetyService/ResolveReport", ResolveReportRequest.ADAPTER)
        assertEquals("r1" to ReportAction.REPORT_ACTION_DISMISS, resolved.report_id to resolved.action)

        // A refusal comes back as the server's reason.
        r.answers.remove("Report")
        assertEquals("Not found", session.report(m, ReportReason.REPORT_REASON_SPAM, ""))

        // Admins hear how many reports wait, by team and for every team.
        session.event(Event(reports_changed = ReportsChanged(team_id = "t1", count = 3)))
        session.event(Event(reports_changed = ReportsChanged(count = 5)))
        assertEquals(mapOf("t1" to 3), session.state.value.reports)
        assertEquals(5, session.state.value.allReports)
    }

    @Test
    fun becomingAnAdminReadsTheReportCounts() = runTest {
        val r = Recorder()
        r.answers["CountReports"] = CountReportsResponse(teams = mapOf("t1" to 2))
        val session = ChatSession(api(r), me, MemoryStore(), backgroundScope, null) {}
        session.event(Event(team_joined = TeamJoined(team = Team(id = "t1", role = TeamRole.TEAM_ROLE_ADMIN))))
        runCurrent()
        assertEquals(mapOf("t1" to 2), session.state.value.reports)
    }
}
