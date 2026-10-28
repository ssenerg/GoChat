package io.github.ssenerg.gochat.core

import gochat.v1.PushPlatform
import gochat.v1.RegisterDeviceRequest
import gochat.v1.RegisterDeviceResponse
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.Caller
import io.github.ssenerg.gochat.core.api.ConnectionLost
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.state.PushService
import io.github.ssenerg.gochat.core.state.PushTarget
import io.github.ssenerg.gochat.core.state.PushToken
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue
import kotlin.time.Duration.Companion.seconds

/** The app ties its push token to the session, and opens what a tapped push shows. */
@OptIn(ExperimentalCoroutinesApi::class)
class PushTest {
    private val ann = User(id = "u-ann", username = "ann")

    private class FakePush : PushService {
        val tokens = MutableStateFlow<PushToken?>(null)
        override val token: StateFlow<PushToken?> = tokens
        var enabled = 0

        override fun enable() {
            enabled++
        }

        override fun disable() {
            tokens.value = null
        }
    }

    /** A session whose RegisterDevice calls fail [failures] times, then [refuse] or succeed. */
    private fun TestScope.session(registered: MutableList<RegisterDeviceRequest>, failures: Int = 0, refuse: Boolean = false): ChatSession {
        var left = failures
        val calls = Caller { method, payload ->
            if (method != "gochat.v1.PushService/RegisterDevice") throw ApiException(ApiException.NOT_FOUND, "not found")
            if (left-- > 0) throw ConnectionLost("Not connected.")
            if (refuse) throw ApiException(12, "no such call")
            registered += RegisterDeviceRequest.ADAPTER.decode(payload)
            RegisterDeviceResponse(enabled = true).encode()
        }
        return ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = calls)), ann, MemoryStore(), backgroundScope, null) {}
    }

    @Test
    fun eachTokenIsRegisteredAgainUntilTheServerTakesIt() = runTest {
        val registered = mutableListOf<RegisterDeviceRequest>()
        val push = FakePush()
        session(registered, failures = 2).registerPush(push)
        assertEquals(1, push.enabled)
        runCurrent()
        assertTrue(registered.isEmpty(), "registered without a token")

        push.tokens.value = PushToken(PushPlatform.PUSH_PLATFORM_FCM, "fcm-1")
        runCurrent()
        assertTrue(registered.isEmpty())
        advanceTimeBy(46.seconds)
        assertEquals(listOf("fcm-1"), registered.map { it.token })

        // A new token replaces it.
        push.tokens.value = PushToken(PushPlatform.PUSH_PLATFORM_APNS, "ab".repeat(32), sandbox = true)
        runCurrent()
        assertEquals(2, registered.size)
        val last = registered.last()
        assertEquals(PushPlatform.PUSH_PLATFORM_APNS, last.platform)
        assertTrue(last.sandbox)
    }

    @Test
    fun aServerWithoutPushesIsNotAskedAgain() = runTest {
        val registered = mutableListOf<RegisterDeviceRequest>()
        var asked = 0
        val calls = Caller { _, _ ->
            asked++
            throw ApiException(12, "no such call")
        }
        val s = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = calls)), ann, MemoryStore(), backgroundScope, null) {}
        val push = FakePush()
        s.registerPush(push)
        push.tokens.value = PushToken(PushPlatform.PUSH_PLATFORM_FCM, "fcm-1")
        advanceTimeBy(600.seconds)
        assertEquals(1, asked)
        assertTrue(registered.isEmpty())
    }

    @Test
    fun aPushNamesWhatItOpens() {
        val data = mapOf("channel_id" to "c1", "root_id" to "r1", "team_id" to "t1", "google.message_id" to "x")
        assertEquals(PushTarget("c1", "r1", "t1"), PushTarget.from { data[it] })
        assertEquals(PushTarget("c1"), PushTarget.from { mapOf("channel_id" to "c1")[it] })
        assertNull(PushTarget.from { mapOf("channel_id" to " ")[it] })
        assertNull(PushTarget.from { null })
    }

    @Test
    fun aTappedPushWaitsForTheScreens() = runTest {
        val app = GoChatApp(MemoryStore(), backgroundScope)
        val target = PushTarget("c1", teamId = "t1")
        app.openPush(target)
        assertEquals(target, app.opening.value)
        app.pushOpened(PushTarget("c2"))
        assertEquals(target, app.opening.value)
        app.pushOpened(target)
        assertNull(app.opening.value)
    }

    @Test
    fun aPushOfAnotherTeamOpensItOnceTeamsLoad() = runTest {
        val store = MemoryStore()
        val s = ChatSession(GoChatApi(ApiClient("https://chat.test", "tok", caller = { _, _ -> ByteArray(0) })), ann, store, backgroundScope, null) {}
        s.showTeam("t2")
        assertEquals("t2", store.get("team"))
    }
}
