package io.github.ssenerg.gochat.core

import com.squareup.wire.Message
import com.squareup.wire.ProtoAdapter
import com.squareup.wire.ofEpochSecond
import gochat.v1.ApiError
import gochat.v1.BeginTOTPSetupRequest
import gochat.v1.BeginTOTPSetupResponse
import gochat.v1.ConfirmTOTPSetupRequest
import gochat.v1.ConfirmTOTPSetupResponse
import gochat.v1.DisableTOTPRequest
import gochat.v1.DisableTOTPResponse
import gochat.v1.Event
import gochat.v1.InviteSummary
import gochat.v1.ListInvitesRequest
import gochat.v1.ListInvitesResponse
import gochat.v1.ListTeamMembersRequest
import gochat.v1.ListTeamMembersResponse
import gochat.v1.ProfileChanged
import gochat.v1.RemoveTeamMemberRequest
import gochat.v1.RemoveTeamMemberResponse
import gochat.v1.RemoveTeamPhotoRequest
import gochat.v1.RemoveTeamPhotoResponse
import gochat.v1.RevokeInviteRequest
import gochat.v1.RevokeInviteResponse
import gochat.v1.Status
import gochat.v1.SystemRole
import gochat.v1.Team
import gochat.v1.TeamJoined
import gochat.v1.TeamMember
import gochat.v1.TeamRole
import gochat.v1.UpdateTeamMemberRoleRequest
import gochat.v1.UpdateTeamMemberRoleResponse
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.Caller
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.model.MAX_PHOTO_BYTES
import io.github.ssenerg.gochat.core.model.canChange
import io.github.ssenerg.gochat.core.model.photoProblem
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpMethod
import io.ktor.http.HttpStatusCode
import io.ktor.http.content.OutgoingContent
import io.ktor.http.headersOf
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Two-factor setup, team settings and team photos: the calls the settings screens make. */
class SettingsTest {
    private val proto = headersOf(HttpHeaders.ContentType, "application/proto")

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

    @Test
    fun twoFactorIsSetUpWithThePasswordAndACode() = runTest {
        val r = Recorder()
        r.answers["BeginTOTPSetup"] = BeginTOTPSetupResponse(secret = "JBSWY3DPEHPK3PXP", url = "otpauth://totp/GoChat:ann?secret=JBSWY3DPEHPK3PXP")
        r.answers["ConfirmTOTPSetup"] = ConfirmTOTPSetupResponse(recovery_codes = listOf("aaaa-bbbb", "cccc-dddd"))
        r.answers["DisableTOTP"] = DisableTOTPResponse()
        val api = api(r)

        val setup = api.beginTotp("hunter2")
        assertEquals("JBSWY3DPEHPK3PXP", setup.secret)
        assertTrue(setup.url.startsWith("otpauth://totp/"))
        assertEquals("hunter2", r.sent("gochat.v1.AuthService/BeginTOTPSetup", BeginTOTPSetupRequest.ADAPTER).password)

        assertEquals(listOf("aaaa-bbbb", "cccc-dddd"), api.confirmTotp("123456"))
        assertEquals("123456", r.sent("gochat.v1.AuthService/ConfirmTOTPSetup", ConfirmTOTPSetupRequest.ADAPTER).code)

        api.disableTotp("hunter2", "aaaa-bbbb")
        val off = r.sent("gochat.v1.AuthService/DisableTOTP", DisableTOTPRequest.ADAPTER)
        assertEquals("hunter2", off.password)
        assertEquals("aaaa-bbbb", off.code)
    }

    @Test
    fun aWrongCodeComesBackAsTheServersReason() = runTest {
        val api = GoChatApi(ApiClient("https://chat.test", "tok", caller = { _, _ -> throw ApiException(ApiException.INVALID_ARGUMENT, "invalid code") }))
        val e = assertFailsWith<ApiException> { api.confirmTotp("000000") }
        assertEquals(ApiException.INVALID_ARGUMENT, e.code)
        assertEquals("invalid code", e.message)
    }

    @Test
    fun membersAndInvitesAreManagedByTeam() = runTest {
        val r = Recorder()
        val ann = TeamMember(user = User(id = "u-ann", username = "ann"), role = TeamRole.TEAM_ROLE_ADMIN)
        r.answers["ListTeamMembers"] = ListTeamMembersResponse(members = listOf(ann), next = "cursor-2")
        r.answers["UpdateTeamMemberRole"] = UpdateTeamMemberRoleResponse()
        r.answers["RemoveTeamMember"] = RemoveTeamMemberResponse()
        r.answers["ListInvites"] = ListInvitesResponse(invites = listOf(InviteSummary(id = "i1", active = true)))
        r.answers["RevokeInvite"] = RevokeInviteResponse()
        r.answers["RemoveTeamPhoto"] = RemoveTeamPhotoResponse(team = Team(id = "t1", photo = ""))
        val api = api(r)

        val page = api.teamMembersPage("t1", after = "cursor-1")
        assertEquals(listOf(ann), page.members)
        assertEquals("cursor-2", page.next)
        val listed = r.sent("gochat.v1.TeamService/ListTeamMembers", ListTeamMembersRequest.ADAPTER)
        assertEquals("t1", listed.team_id)
        assertEquals("cursor-1", listed.after)
        assertEquals(100, listed.limit)

        api.setMemberRole("t1", "u-bob", TeamRole.TEAM_ROLE_ADMIN)
        val role = r.sent("gochat.v1.TeamService/UpdateTeamMemberRole", UpdateTeamMemberRoleRequest.ADAPTER)
        assertEquals(Triple("t1", "u-bob", TeamRole.TEAM_ROLE_ADMIN), Triple(role.team_id, role.user_id, role.role))

        api.removeMember("t1", "u-bob")
        val removed = r.sent("gochat.v1.TeamService/RemoveTeamMember", RemoveTeamMemberRequest.ADAPTER)
        assertEquals("t1" to "u-bob", removed.team_id to removed.user_id)

        assertEquals("i1", api.invites("t1").invites.single().id)
        assertEquals("t1", r.sent("gochat.v1.TeamService/ListInvites", ListInvitesRequest.ADAPTER).team_id)
        api.revokeInvite("t1", "i1")
        val revoked = r.sent("gochat.v1.TeamService/RevokeInvite", RevokeInviteRequest.ADAPTER)
        assertEquals("t1" to "i1", revoked.team_id to revoked.invite_id)

        assertEquals("", api.removeTeamPhoto("t1")?.photo)
        assertEquals("t1", r.sent("gochat.v1.TeamService/RemoveTeamPhoto", RemoveTeamPhotoRequest.ADAPTER).team_id)
    }

    @Test
    fun aTeamPhotoGoesOverHttpAtApiV2() = runTest {
        val picture = byteArrayOf(0x89.toByte(), 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3)
        val engine = MockEngine { req ->
            assertEquals(HttpMethod.Post, req.method)
            assertEquals("https://chat.test/api/v2/teams/t1/photo", req.url.toString())
            assertEquals("Bearer tok", req.headers[HttpHeaders.Authorization])
            assertContentEquals(picture, (req.body as OutgoingContent.ByteArrayContent).bytes())
            respond(Team(id = "t1", photo = "v2").encode(), HttpStatusCode.OK, proto)
        }
        assertEquals("v2", ApiClient("https://chat.test", "tok", engine).setTeamPhoto("t1", picture).photo)
    }

    @Test
    fun anOlderServerSaysItTakesNoTeamPhotos() = runTest {
        val old = MockEngine { respond("404 page not found", HttpStatusCode.NotFound) }
        val e = assertFailsWith<ApiException> { ApiClient("https://chat.test", "tok", old).setTeamPhoto("t1", byteArrayOf(1)) }
        assertEquals(ApiException.NOT_FOUND, e.code)
        assertTrue(e.message!!.contains("does not take team photos"))
        // A team the server does not know is the server's own reason.
        val current = MockEngine { respond(ApiError(code = 5, message = "no such team").encode(), HttpStatusCode.NotFound, proto) }
        assertEquals("no such team", assertFailsWith<ApiException> { ApiClient("https://chat.test", "tok", current).setTeamPhoto("t1", byteArrayOf(1)) }.message)
    }

    @Test
    fun onlyTheOwnerOrASystemAdminChangesAnotherAdmin() {
        val owner = User(id = "u-own")
        val admin = User(id = "u-adm")
        val member = User(id = "u-mem")
        val system = User(id = "u-sys", role = SystemRole.SYSTEM_ROLE_ADMIN)
        fun team(role: TeamRole) = Team(id = "t1", owner_id = owner.id, role = role)
        fun m(u: User, role: TeamRole) = TeamMember(user = u, role = role)
        val asAdmin = team(TeamRole.TEAM_ROLE_ADMIN)

        // The owner changes admins and members, never themself.
        assertTrue(asAdmin.canChange(owner, m(admin, TeamRole.TEAM_ROLE_ADMIN)))
        assertTrue(asAdmin.canChange(owner, m(member, TeamRole.TEAM_ROLE_MEMBER)))
        assertFalse(asAdmin.canChange(owner, m(owner, TeamRole.TEAM_ROLE_ADMIN)))
        // Another admin changes members only.
        assertTrue(asAdmin.canChange(admin, m(member, TeamRole.TEAM_ROLE_MEMBER)))
        assertFalse(asAdmin.canChange(admin, m(User(id = "u-adm2"), TeamRole.TEAM_ROLE_ADMIN)))
        assertFalse(asAdmin.canChange(admin, m(owner, TeamRole.TEAM_ROLE_ADMIN)))
        // Members change no one.
        assertFalse(team(TeamRole.TEAM_ROLE_MEMBER).canChange(member, m(User(id = "u-x"), TeamRole.TEAM_ROLE_MEMBER)))
        // A system admin changes anyone else, even outside the team.
        assertTrue(team(TeamRole.TEAM_ROLE_UNSPECIFIED).canChange(system, m(owner, TeamRole.TEAM_ROLE_ADMIN)))
        assertFalse(asAdmin.canChange(system, m(system, TeamRole.TEAM_ROLE_ADMIN)))
    }

    @Test
    fun photosAreThePicturesTheServerTakes() {
        fun bytes(vararg head: Int, size: Int = 64) = ByteArray(size).also { b -> head.forEachIndexed { i, v -> b[i] = v.toByte() } }
        fun text(t: String, at: Int = 0, into: ByteArray = ByteArray(64)) = into.also { t.forEachIndexed { i, c -> it[at + i] = c.code.toByte() } }

        assertNull(photoProblem(bytes(0xFF, 0xD8, 0xFF, 0xE0)))
        assertNull(photoProblem(bytes(0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A)))
        assertNull(photoProblem(text("GIF89a")))
        assertNull(photoProblem(text("WEBPVP8L", at = 8, into = text("RIFF"))))
        // An iPhone's HEIC, and a document.
        assertEquals("Use a JPEG, PNG, GIF or WebP picture.", photoProblem(text("ftypheic", at = 4)))
        assertEquals("Use a JPEG, PNG, GIF or WebP picture.", photoProblem(text("%PDF-1.7")))
        assertNotNull(photoProblem(ByteArray(0)))
        assertNull(photoProblem(bytes(0xFF, 0xD8, size = MAX_PHOTO_BYTES)))
        assertTrue(photoProblem(bytes(0xFF, 0xD8, size = MAX_PHOTO_BYTES + 1))!!.contains("10 MB"))
    }

    @Test
    fun theSessionKeepsTheTeamsPhotoAndTwoFactorCurrent() = runTest {
        val r = Recorder()
        r.answers["RemoveTeamPhoto"] = RemoveTeamPhotoResponse(team = Team(id = "t1", display_name = "Acme", role = TeamRole.TEAM_ROLE_ADMIN))
        r.answers["ConfirmTOTPSetup"] = ConfirmTOTPSetupResponse(recovery_codes = listOf("aaaa-bbbb"))
        r.answers["DisableTOTP"] = DisableTOTPResponse()
        val me = User(id = "u-ann", username = "ann", display_name = "Ann")
        val session = ChatSession(api(r), me, MemoryStore(), backgroundScope, null) {}
        session.event(Event(team_joined = TeamJoined(team = Team(id = "t1", display_name = "Acme", photo = "v1", role = TeamRole.TEAM_ROLE_ADMIN))))
        assertEquals("v1", session.state.value.teams.single().photo)

        assertNull(session.removeTeamPhoto("t1"))
        assertEquals("", session.state.value.teams.single().photo)
        // A file that is no picture never leaves the phone.
        val before = r.made.size
        assertEquals("Use a JPEG, PNG, GIF or WebP picture.", session.setTeamPhoto("t1", "not a picture".encodeToByteArray()))
        assertEquals(before, r.made.size)

        assertEquals(listOf("aaaa-bbbb"), session.confirmTotp("123456"))
        assertTrue(session.state.value.me.totp_enabled)
        // Others see Ann without two-factor; her own state keeps it.
        session.event(Event(profile_changed = ProfileChanged(user = me.copy(display_name = "Ann B"))))
        assertEquals("Ann B", session.state.value.me.display_name)
        assertTrue(session.state.value.me.totp_enabled)
        session.disableTotp("hunter2", "123456")
        assertFalse(session.state.value.me.totp_enabled)
    }

    @Test
    fun aProfileEventAboutMeKeepsWhatOnlyISee() = runTest {
        val me = User(
            id = "u-ann", username = "ann", display_name = "Ann", totp_enabled = true,
            created_at = ofEpochSecond(1_700_000_000, 0), status = Status.STATUS_DND, dnd_until = ofEpochSecond(2_000_000_000, 0),
        )
        val session = ChatSession(api(Recorder()), me, MemoryStore(), backgroundScope, null) {}
        // Renaming herself while on Do not disturb: the event is what others see of Ann.
        session.event(Event(profile_changed = ProfileChanged(user = User(id = "u-ann", username = "ann", display_name = "Ann B", photo = "v2"))))
        assertEquals(me.copy(display_name = "Ann B", photo = "v2"), session.state.value.me)
    }
}
