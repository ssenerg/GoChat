package io.github.ssenerg.gochat.core

import com.squareup.wire.Message
import com.squareup.wire.ProtoAdapter
import com.squareup.wire.ofEpochSecond
import gochat.v1.BeginSSOLinkRequest
import gochat.v1.BeginSSOLinkResponse
import gochat.v1.GetStorageUsageResponse
import gochat.v1.LinkSSORequest
import gochat.v1.LinkSSOResponse
import gochat.v1.ListSSOIdentitiesResponse
import gochat.v1.ListSessionsResponse
import gochat.v1.MeResponse
import gochat.v1.RevokeSessionRequest
import gochat.v1.RevokeSessionResponse
import gochat.v1.RevokeSessionsResponse
import gochat.v1.SSOIdentity
import gochat.v1.SessionInfo
import gochat.v1.UnlinkSSORequest
import gochat.v1.UnlinkSSOResponse
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.Caller
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.api.Pkce
import io.github.ssenerg.gochat.core.api.SsoReturn
import io.github.ssenerg.gochat.core.model.about
import io.github.ssenerg.gochat.core.model.address
import io.github.ssenerg.gochat.core.model.canUnlink
import io.github.ssenerg.gochat.core.model.describe
import io.github.ssenerg.gochat.core.model.deviceName
import io.github.ssenerg.gochat.core.model.fraction
import io.github.ssenerg.gochat.core.model.seen
import io.github.ssenerg.gochat.core.model.signedIn
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.state.SignIns
import io.github.ssenerg.gochat.core.state.SsoLink
import io.ktor.http.Url
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.datetime.LocalDateTime
import kotlinx.datetime.TimeZone
import kotlinx.datetime.toInstant
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Settings' sessions, linked sign-ins and storage. */
class AccountTest {
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

    private fun TestScope.session(r: Recorder, signedOut: (String) -> Unit = {}) =
        ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = r.caller)), ann, MemoryStore(), backgroundScope, null, onSignedOut = signedOut)

    private val utc = TimeZone.UTC

    private fun at(text: String): Long = LocalDateTime.parse(text).toInstant(utc).toEpochMilliseconds()

    private fun instant(text: String) = ofEpochSecond(at(text) / 1000, 0)

    @Test
    fun namesTheDeviceAsTheWebDoes() {
        // The web's render.Device cases.
        val cases = mapOf(
            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15" to "Safari on macOS",
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36" to "Chrome on Windows",
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0" to "Edge on Windows",
            "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0" to "Firefox on Linux",
            "Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36" to "Chrome on Android",
            "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1" to "Safari on iPhone",
            "Go-http-client/1.1" to "Go-http-client",
            "GoChat/0.2.0 (Android 16; Pixel 8)" to "GoChat app on Pixel 8",
            "GoChat/0.2.0 (iOS 18.4; iPhone)" to "GoChat app on iPhone",
            "GoChat/0.2.0 (Android 16)" to "GoChat app on Android",
            "GoChat/0.2.0" to "GoChat app",
            "" to "Unknown device",
            "<script> (x)" to "Unknown device",
        )
        for ((ua, want) in cases) assertEquals(want, deviceName(ua), ua)
    }

    @Test
    fun sessionsSayWhenAndWhere() {
        val now = at("2026-11-01T15:00:00")
        val s = SessionInfo(
            id = "s1", ip = "10.0.0.1", last_ip = "10.0.0.2",
            created_at = instant("2026-10-26T09:30:00"), last_seen_at = instant("2026-11-01T14:05:00"),
        )
        assertEquals("10.0.0.2", s.address)
        assertEquals("Last seen today at 14:05", s.seen(now, utc))
        assertEquals("Signed in Monday from 10.0.0.1", s.signedIn(now, utc))
        assertEquals("Last seen yesterday at 23:59", s.copy(last_seen_at = instant("2026-10-31T23:59:00")).seen(now, utc))
        assertEquals("Not seen since signing in", s.copy(last_seen_at = null).seen(now, utc))
        assertEquals("Active now", s.copy(current = true).seen(now, utc))
        val unmoved = s.copy(last_ip = "")
        assertEquals("10.0.0.1", unmoved.address)
        assertEquals("Signed in Monday", unmoved.signedIn(now, utc))
    }

    @Test
    fun listsAndSignsOutSessions() = runTest {
        val r = Recorder()
        var signedOut: String? = null
        val session = session(r) { signedOut = it }
        val list = listOf(SessionInfo(id = "this", current = true), SessionInfo(id = "other"))
        r.answers["ListSessions"] = ListSessionsResponse(sessions = list)
        assertEquals(list, session.sessions())

        r.answers["RevokeSession"] = RevokeSessionResponse()
        assertNull(session.revokeSession("other"))
        assertEquals("other", r.sent("gochat.v1.AuthService/RevokeSession", RevokeSessionRequest.ADAPTER).session_id)
        r.answers["RevokeSessions"] = RevokeSessionsResponse(revoked = 3)
        assertNull(session.revokeOtherSessions())

        r.refused["RevokeSession"] = ApiException(ApiException.NOT_FOUND, "no such session")
        assertEquals("No such session", session.revokeSession("gone"))
        assertNull(signedOut)
        // This session went meanwhile.
        r.refused["RevokeSessions"] = ApiException(ApiException.UNAUTHENTICATED, "unauthenticated")
        session.revokeOtherSessions()
        assertEquals("Your session ended. Sign in again.", signedOut)
    }

    @Test
    fun theLastWayToSignInStaysLinked() = runTest {
        val corp = SSOIdentity(provider_id = "corp", provider_name = "Corp ID", issuer = "https://id.corp.test", subject = "ann")
        val other = corp.copy(provider_id = "other", issuer = "https://other.test")
        val now = at("2026-11-01T15:00:00")
        assertEquals("linked Monday", corp.copy(linked_at = instant("2026-10-26T09:30:00")).about(now, utc))
        assertEquals("ann@corp.test · linked today", corp.copy(email = "ann@corp.test", linked_at = instant("2026-11-01T09:30:00")).about(now, utc))
        assertFalse(canUnlink(listOf(corp), hasPassword = false))
        assertTrue(canUnlink(listOf(corp, other), hasPassword = false))
        assertTrue(canUnlink(listOf(corp), hasPassword = true))

        val r = Recorder()
        val session = session(r)
        r.answers["Me"] = MeResponse(user = ann, has_password = false)
        r.answers["ListSSOIdentities"] = ListSSOIdentitiesResponse(identities = listOf(corp))
        assertEquals(SignIns(listOf(corp), hasPassword = false), session.signIns())

        r.refused["UnlinkSSO"] = ApiException(ApiException.FAILED_PRECONDITION, "this is the only way you sign in, so it stays linked")
        assertEquals("This is the only way you sign in, so it stays linked", session.unlinkSso(corp))
        r.refused.clear()
        r.answers["UnlinkSSO"] = UnlinkSSOResponse()
        assertNull(session.unlinkSso(corp))
        val sent = r.sent("gochat.v1.AuthService/UnlinkSSO", UnlinkSSORequest.ADAPTER)
        assertEquals(listOf("", "https://id.corp.test", "ann"), listOf(sent.user_id, sent.issuer, sent.subject))
    }

    @Test
    fun showsTheStorageUsed() = runTest {
        val r = Recorder()
        val session = session(r)
        r.answers["GetStorageUsage"] = GetStorageUsageResponse(used = 3L shl 20, quota = 10L shl 20)
        val usage = session.storageUsage()!!
        assertEquals("3.0 MB of 10 MB used", usage.describe())
        assertEquals(0.3f, usage.fraction)
        assertTrue(r.made.any { it.first == "gochat.v1.FileService/GetStorageUsage" })

        val unlimited = GetStorageUsageResponse(used = 812)
        assertEquals("812 B used", unlimited.describe())
        assertNull(unlimited.fraction)
        assertEquals(1f, GetStorageUsageResponse(used = 12, quota = 10).fraction)

        // A server from before apps could ask.
        r.refused["GetStorageUsage"] = ApiException(ApiException.UNIMPLEMENTED, "no such call")
        assertNull(session.storageUsage())
        r.refused["GetStorageUsage"] = ApiException(ApiException.UNAVAILABLE, "busy, try again")
        assertFailsWith<ApiException> { session.storageUsage() }
    }

    @Test
    fun linksAnAccountFromTheBrowser() = runTest {
        val r = Recorder()
        var signedOut: String? = null
        val session = session(r) { signedOut = it }
        val pkce = Pkce.create()
        r.answers["BeginSSOLink"] = BeginSSOLinkResponse(ticket = "ticket+1")
        val url = Url(session.linkPage("corp", pkce))
        assertEquals("https://chat.test/login/sso/corp", url.toString().substringBefore('?'))
        assertEquals(pkce.challenge, url.parameters["app"])
        assertEquals("ticket+1", url.parameters["link"])
        val began = r.sent("gochat.v1.AuthService/BeginSSOLink", BeginSSOLinkRequest.ADAPTER)
        assertEquals(listOf("corp", pkce.challenge), listOf(began.provider_id, began.app_challenge))

        r.refused["BeginSSOLink"] = ApiException(ApiException.UNIMPLEMENTED, "no such call")
        assertEquals("This server links accounts in Settings on the web.", assertFailsWith<ApiException> { session.linkPage("corp", pkce) }.message)

        r.answers["LinkSSO"] = LinkSSOResponse(provider_name = "Corp ID", email = "ann@corp.test")
        assertEquals(SsoLink.Linked("Corp ID", "ann@corp.test"), session.linkSso("code-1", pkce.verifier))
        val linked = r.sent("gochat.v1.AuthService/LinkSSO", LinkSSORequest.ADAPTER)
        assertEquals(listOf("code-1", pkce.verifier), listOf(linked.link_token, linked.verifier))
        r.refused["LinkSSO"] = ApiException(ApiException.FAILED_PRECONDITION, "this Corp ID account is linked to another GoChat account")
        assertEquals(SsoLink.Failed("This Corp ID account is linked to another GoChat account"), session.linkSso("code-2", pkce.verifier))
        assertNull(signedOut)
    }

    @Test
    fun aLinkComesBackToTheApp() = runTest {
        val store = MemoryStore()
        val app = GoChatApp(store, backgroundScope)
        // What the browser says went wrong.
        store.put("sso_link", "verifier-1")
        app.ssoReturned("${GoChatApp.SSO_REDIRECT}?error=this+Corp+ID+account+is+linked+to+another+GoChat+account")
        assertEquals(SsoLink.Failed("This Corp ID account is linked to another GoChat account"), app.link.value)
        assertNull(store.get("sso_link"))
        app.linkSeen()
        assertNull(app.link.value)

        // A code waits for the session, and goes once it is not coming.
        store.put("sso_link", "verifier-2")
        app.linkReturned(SsoReturn("code-1", null))
        assertNull(store.get("sso_link"))
        app.start()
        runCurrent()
        assertNull(app.link.value)
        // Taken by a link, the return is no sign-in again.
        assertNull(app.reauth.value)

        // Starting another flow forgets the link's verifier, and the reverse.
        store.put("sso_link", "verifier-3")
        app.reauthUrl("corp", "chat.example.com")
        assertNull(store.get("sso_link"))
        store.put("sso_link", "verifier-4")
        app.ssoUrl("chat.example.com", "corp")
        assertNull(store.get("sso_link"))
        assertNull(store.get("sso_reauth"))
    }
}
