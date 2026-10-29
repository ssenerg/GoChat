package io.github.ssenerg.gochat.core

import com.squareup.wire.Message
import com.squareup.wire.ProtoAdapter
import gochat.v1.DeleteAccountRequest
import gochat.v1.DeleteAccountResponse
import gochat.v1.Event
import gochat.v1.LeaveTeamRequest
import gochat.v1.LeaveTeamResponse
import gochat.v1.ListTeamsResponse
import gochat.v1.Team
import gochat.v1.TeamJoined
import gochat.v1.TeamLeft
import gochat.v1.TeamMembersChanged
import gochat.v1.TeamRole
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.Caller
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.api.Pkce
import io.github.ssenerg.gochat.core.api.SsoReturn
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.state.Reauth
import io.ktor.http.Url
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Leaving a team and deleting the account. */
class ExitsTest {
    private val ann = User(id = "u-ann", username = "ann")

    /** The calls made, by method, and their requests; a refused method fails with its reason. */
    private class Recorder {
        val made = mutableListOf<Pair<String, ByteArray>>()
        val answers = mutableMapOf<String, Message<*, *>>()
        val refused = mutableMapOf<String, ApiException>()

        val caller = Caller { method, payload ->
            made += method to payload
            val name = method.substringAfterLast('/')
            refused[name]?.let { throw it }
            answers[name]?.encode() ?: throw ApiException(ApiException.NOT_FOUND, "not found")
        }

        fun <T : Any> sent(method: String, adapter: ProtoAdapter<T>): T =
            adapter.decode(made.last { it.first == method }.second)
    }

    private fun team(id: String, owner: String = "u-own") = Team(id = id, display_name = id, owner_id = owner, role = TeamRole.TEAM_ROLE_MEMBER)

    @Test
    fun leavingATeamForgetsIt() = runTest {
        val r = Recorder()
        r.answers["LeaveTeam"] = LeaveTeamResponse()
        val session = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = r.caller)), ann, MemoryStore(), backgroundScope, null) {}
        session.event(Event(team_joined = TeamJoined(team = team("t1"))))
        session.event(Event(team_joined = TeamJoined(team = team("t2"))))

        r.refused["LeaveTeam"] = ApiException(ApiException.FAILED_PRECONDITION, "you are the only admin of t1: make another member an admin before you leave")
        assertEquals("You are the only admin of t1: make another member an admin before you leave", session.leaveTeam("t1"))
        assertEquals(listOf("t1", "t2"), session.state.value.teams.map { it.id })

        r.refused.clear()
        assertNull(session.leaveTeam("t1"))
        assertEquals("t1", r.sent("gochat.v1.TeamService/LeaveTeam", LeaveTeamRequest.ADAPTER).team_id)
        assertEquals(listOf("t2"), session.state.value.teams.map { it.id })
        // The server's word of it changes nothing more.
        session.event(Event(team_left = TeamLeft(team_id = "t1")))
        assertEquals(listOf("t2"), session.state.value.teams.map { it.id })
    }

    @Test
    fun someoneLeavingReloadsTheMembersAndAnOwnersTeam() = runTest {
        val r = Recorder()
        r.answers["ListTeams"] = ListTeamsResponse(teams = listOf(team("t1", owner = "u-heir")))
        val session = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = r.caller)), ann, MemoryStore(), backgroundScope, null) {}
        session.event(Event(team_joined = TeamJoined(team = team("t1"))))
        val changed = mutableListOf<String>()
        backgroundScope.launch { session.membersChanged.collect { changed += it } }
        runCurrent()

        session.event(Event(team_members_changed = TeamMembersChanged(team_id = "t1", user_id = "u-bob")))
        runCurrent()
        assertEquals(listOf("t1"), changed)
        assertTrue(r.made.none { it.first == "gochat.v1.TeamService/ListTeams" }, "a member's leaving read the teams")

        // The owner left: the team has another.
        session.event(Event(team_members_changed = TeamMembersChanged(team_id = "t1", user_id = "u-own")))
        runCurrent()
        assertEquals(listOf("t1", "t1"), changed)
        assertEquals("u-heir", session.state.value.teams.single().owner_id)
    }

    @Test
    fun deletingTheAccountSignsOut() = runTest {
        val r = Recorder()
        r.refused["DeleteAccount"] = ApiException(ApiException.INVALID_ARGUMENT, "invalid code")
        var signedOut: String? = null
        val session = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = r.caller)), ann, MemoryStore(), backgroundScope, null) { signedOut = it }

        assertEquals("Invalid code", session.deleteAccount("hunter2", "000000"))
        assertNull(signedOut)

        r.refused.clear()
        r.answers["DeleteAccount"] = DeleteAccountResponse()
        assertNull(session.deleteAccount("hunter2", " 123456 "))
        assertEquals("Your account was deleted.", signedOut)
        val sent = r.sent("gochat.v1.AuthService/DeleteAccount", DeleteAccountRequest.ADAPTER)
        assertEquals(listOf("hunter2", "123456", "", ""), listOf(sent.password, sent.code, sent.sso_token, sent.sso_verifier))
    }

    @Test
    fun anAccountWithoutAPasswordDeletesWithASignInAgain() = runTest {
        val r = Recorder()
        r.answers["DeleteAccount"] = DeleteAccountResponse()
        val session = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = r.caller)), ann, MemoryStore(), backgroundScope, null) {}
        assertNull(session.deleteAccount("", "", Reauth.Proof("code-1", "verifier-1")))
        val sent = r.sent("gochat.v1.AuthService/DeleteAccount", DeleteAccountRequest.ADAPTER)
        assertEquals(listOf("", "code-1", "verifier-1"), listOf(sent.password, sent.sso_token, sent.sso_verifier))
    }

    @Test
    fun aSignInAgainKeepsItsProof() = runTest {
        val store = MemoryStore()
        val app = GoChatApp(store, backgroundScope)
        val url = Url(app.reauthUrl("corp", "chat.example.com/"))
        assertEquals("https://chat.example.com/login/sso/corp", url.toString().substringBefore('?'))
        assertEquals("1", url.parameters["reauth"])
        val verifier = store.get("sso_reauth")!!
        assertEquals(url.parameters["app"], Pkce(verifier).challenge)

        app.reauthReturned(SsoReturn("code-1", null))
        assertEquals(Reauth.Proof("code-1", verifier), app.reauth.first())
        assertNull(store.get("sso_reauth"))
        // The verifier went with it.
        app.reauthUsed()
        app.reauthReturned(SsoReturn("code-2", null))
        assertNull(app.reauth.value)

        app.reauthUrl("corp", "chat.example.com")
        app.reauthReturned(SsoReturn(null, "sign in with the Corp ID account linked to yours"))
        assertEquals(Reauth.Failed("Sign in with the Corp ID account linked to yours"), app.reauth.value)
    }
}
