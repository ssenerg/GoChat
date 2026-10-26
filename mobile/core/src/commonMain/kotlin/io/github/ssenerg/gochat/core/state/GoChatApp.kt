package io.github.ssenerg.gochat.core.state

import gochat.v1.SignUpRequest
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.AppInfo
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.api.normalizeServerUrl
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.socket.Connection
import io.github.ssenerg.gochat.core.voice.RtcEngine
import io.ktor.client.engine.HttpClientEngine
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlin.io.encoding.Base64
import kotlin.io.encoding.ExperimentalEncodingApi

/** Durable key-value storage the platform provides: the Keychain, preferences. */
interface KeyValueStore {
    fun get(key: String): String?

    /** Stores [value], or removes the key when it is null. */
    fun put(key: String, value: String?)
}

/** A store that forgets everything, for tests and previews. */
class MemoryStore : KeyValueStore {
    private val map = mutableMapOf<String, String>()

    override fun get(key: String) = map[key]

    override fun put(key: String, value: String?) {
        if (value == null) map.remove(key) else map[key] = value
    }
}

/** Which screen the app is on, as far as signing in goes. */
sealed interface AppState {
    data object Starting : AppState

    data class SignedOut(val server: String, val notice: String? = null) : AppState

    /** The password was right; the account asks for its second factor. */
    data class Verifying(val server: String, val mfaToken: String) : AppState

    data class SignedIn(val session: ChatSession) : AppState
}

/**
 * The app as a whole: it restores the saved session, signs people in and
 * out, and holds the [ChatSession] while they are signed in. Use it from
 * one thread, the main one.
 */
@OptIn(ExperimentalEncodingApi::class)
class GoChatApp(
    private val store: KeyValueStore,
    private val scope: CoroutineScope,
    private val engine: HttpClientEngine? = null,
    /** The platform's WebRTC, for voice channels; without it they only show who talks. */
    private val rtc: RtcEngine? = null,
    /** This app's version and device, for the server to know it by. */
    private val app: AppInfo? = null,
) {
    private val _state = MutableStateFlow<AppState>(AppState.Starting)
    val state: StateFlow<AppState> = _state.asStateFlow()

    private var session: ChatSession? = null

    /** The server people signed in to last, to fill the sign-in form. */
    val lastServer: String get() = store.get(KEY_SERVER) ?: ""

    /** Restores the saved session, if it is still valid. */
    fun start() {
        val server = store.get(KEY_SERVER)
        val token = store.get(KEY_TOKEN)
        if (server == null || token == null) {
            _state.value = AppState.SignedOut(server ?: "")
            return
        }
        scope.launch {
            val api = api(server, token)
            // Keep the token: once the app is updated, the session goes on.
            tooOld(api)?.let { notice ->
                api.client.close()
                _state.value = AppState.SignedOut(server, notice)
                return@launch
            }
            // The socket resumes after the last update the app applied.
            api.client.socket.pts = store.get(KEY_PTS)?.toLongOrNull() ?: 0
            when (val c = api.client.socket.connect()) {
                is Connection.Online -> open(api, c.welcome.user ?: cachedMe() ?: return@launch signedOut(api, server, "Sign in again."))
                Connection.SignedOut -> {
                    store.put(KEY_TOKEN, null)
                    store.put(KEY_PTS, null)
                    signedOut(api, server, "Your session ended. Sign in again.")
                }
                is Connection.Refused -> signedOut(api, server, c.reason)
                else -> {
                    // Offline: open as the person last seen, and load once
                    // the socket gets through; without them, try again from
                    // the sign-in screen, keeping the token.
                    val me = cachedMe()
                    if (me != null) open(api, me) else signedOut(api, server, "Cannot reach the server. Check your connection.")
                }
            }
        }
    }

    private fun signedOut(api: GoChatApi, server: String, notice: String) {
        api.client.close()
        _state.value = AppState.SignedOut(server, notice)
    }

    /** Signs in over an anonymous socket, which then serves the session; returns why not, or null. */
    suspend fun signIn(server: String, login: String, password: String): String? {
        val url = normalizeServerUrl(server)
        val api = api(url)
        tooOld(api)?.let {
            api.client.close()
            return it
        }
        return try {
            val res = api.login(login.trim(), password)
            when {
                res.mfa_token != null -> {
                    api.client.close()
                    _state.value = AppState.Verifying(url, res.mfa_token)
                    null
                }
                res.session != null -> {
                    signedIn(api, url, res.session.token)
                    null
                }
                else -> {
                    api.client.close()
                    "The server answered in a way this app does not understand."
                }
            }
        } catch (e: Exception) {
            api.client.close()
            e.userMessage()
        }
    }

    /** Answers the second-factor challenge with a TOTP or recovery code. */
    suspend fun verify(code: String): String? {
        val st = _state.value as? AppState.Verifying ?: return null
        val api = api(st.server)
        return try {
            val s = api.verifyMfa(st.mfaToken, code.trim()) ?: return "The server gave no session."
            signedIn(api, st.server, s.token)
            null
        } catch (e: ApiException) {
            api.client.close()
            // The server tells an expired attempt from a wrong code by its code.
            if (e.code == ApiException.FAILED_PRECONDITION) {
                _state.value = AppState.SignedOut(st.server, "The sign-in attempt expired. Sign in again.")
                null
            } else {
                e.userMessage()
            }
        } catch (e: Exception) {
            api.client.close()
            e.userMessage()
        }
    }

    fun cancelVerify() {
        val st = _state.value as? AppState.Verifying ?: return
        _state.value = AppState.SignedOut(st.server)
    }

    /** Creates an account, with an invite unless the server is open; returns why not, or null. */
    suspend fun signUp(server: String, invite: String, username: String, email: String, displayName: String, password: String): String? {
        val url = normalizeServerUrl(server)
        val api = api(url)
        tooOld(api)?.let {
            api.client.close()
            return it
        }
        return try {
            val res = api.signUp(
                SignUpRequest(
                    username = username.trim(), email = email.trim(), display_name = displayName.trim(),
                    password = password, invite_token = inviteToken(invite),
                ),
            )
            val s = res.session ?: return "The server gave no session."
            signedIn(api, url, s.token)
            null
        } catch (e: Exception) {
            api.client.close()
            e.userMessage()
        }
    }

    /** Signs out of this device. */
    suspend fun signOut() {
        val s = session ?: return
        // Signing out ends the socket too: that is no surprise.
        s.signingOut = true
        runCatching { s.api.logout() }
        close(null)
    }

    private fun api(server: String, token: String? = null) = GoChatApi(ApiClient(server, token, engine, app))

    /** Why the server does not take this app, or null; one that cannot say takes it. */
    private suspend fun tooOld(api: GoChatApi): String? {
        val app = app ?: return null
        val min = try {
            api.client.info().min_app_version
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // Offline, or a server from before it said: the calls that follow tell.
            return null
        }
        return if (app.olderThan(min)) "This server needs version $min of the app or later. Update the app to sign in." else null
    }

    /** Says hello again on the socket that signed in, now with the session's token. */
    private suspend fun signedIn(api: GoChatApi, server: String, token: String) {
        api.client.token = token
        val me = api.client.socket.authenticate(token).user ?: throw ApiException(ApiException.UNAUTHENTICATED, "Sign in again.")
        store.put(KEY_SERVER, server)
        store.put(KEY_TOKEN, token)
        store.put(KEY_PTS, null)
        open(api, me)
    }

    /** The signed-in person as last seen, to open the app offline. */
    private fun cachedMe(): gochat.v1.User? = store.get(KEY_ME)?.let { raw ->
        runCatching { gochat.v1.User.ADAPTER.decode(Base64.decode(raw)) }.getOrNull()
    }

    private fun open(api: GoChatApi, me: gochat.v1.User) {
        store.put(KEY_ME, Base64.encode(gochat.v1.User.ADAPTER.encode(me)))
        val s = ChatSession(api, me, store, scope, rtc, onRefused = { notice -> close(notice, keepToken = true) }) { notice -> close(notice) }
        session = s
        _state.value = AppState.SignedIn(s)
        s.start(store.get(KEY_PTS)?.toLongOrNull() ?: 0)
    }

    /** Ends the session; [keepToken] when an updated app may resume it. */
    private fun close(notice: String?, keepToken: Boolean = false) {
        val s = session ?: return
        session = null
        s.stop()
        if (!keepToken) {
            store.put(KEY_TOKEN, null)
            store.put(KEY_ME, null)
            store.put(KEY_PTS, null)
        }
        _state.value = AppState.SignedOut(s.api.client.baseUrl, notice)
        s.api.client.close()
    }

    companion object {
        private const val KEY_SERVER = "server"
        private const val KEY_TOKEN = "token"
        private const val KEY_ME = "me"
        private const val KEY_PTS = ChatSession.KEY_PTS

        /** Takes an invite link or its bare token. */
        fun inviteToken(input: String): String = input.trim().substringAfterLast("/join/").substringBefore('?').trim('/')
    }
}
