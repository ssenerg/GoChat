package io.github.ssenerg.gochat.core

import io.github.ssenerg.gochat.core.api.Pkce
import io.github.ssenerg.gochat.core.api.SsoReturn
import io.github.ssenerg.gochat.core.api.parseSsoReturn
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.ktor.http.Url
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

class SsoTest {
    @Test
    fun challengeIsS256OfTheVerifier() {
        // RFC 7636, appendix B.
        assertEquals("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", Pkce("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk").challenge)
        val a = Pkce.create()
        assertEquals(43, a.verifier.length)
        assertNotEquals(a.verifier, Pkce.create().verifier)
    }

    @Test
    fun readsWhatTheBrowserCameBackWith() {
        val r = GoChatApp.SSO_REDIRECT
        assertEquals(SsoReturn("abc", null), parseSsoReturn("$r?code=abc", r))
        assertEquals(SsoReturn(null, "no account is linked"), parseSsoReturn("$r?error=no+account+is+linked", r))
        assertNull(parseSsoReturn("$r?other=1", r))
        assertNull(parseSsoReturn("${r}x?code=abc", r))
        assertNull(parseSsoReturn("https://evil.example/sso?code=abc", r))
    }

    @Test
    fun startsAndEndsASignInInTheBrowser() = runTest {
        val store = MemoryStore()
        val app = GoChatApp(store, backgroundScope)
        val url = Url(app.ssoUrl("chat.example.com/", "corp", "https://chat.example.com/join/tok"))
        assertEquals("https://chat.example.com/login/sso/corp", url.toString().substringBefore('?'))
        assertEquals("tok", url.parameters["invite"])
        val challenge = url.parameters["app"]
        assertEquals(43, challenge?.length)
        assertEquals(challenge, Pkce(store.get("sso")!!.substringAfter('\n')).challenge)

        app.ssoReturned("${GoChatApp.SSO_REDIRECT}?error=no+account+is+linked")
        assertEquals(AppState.SignedOut("https://chat.example.com", "No account is linked"), app.state.value)
        assertNull(store.get("sso"))
        // The verifier went with the first return.
        app.ssoReturned("${GoChatApp.SSO_REDIRECT}?code=abc")
        val st = app.state.value
        assertTrue(st is AppState.SignedOut && st.notice == "The sign-in expired. Sign in again.", "$st")
    }
}
