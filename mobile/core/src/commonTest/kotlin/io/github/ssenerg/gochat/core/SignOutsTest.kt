package io.github.ssenerg.gochat.core

import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.ConnectionLost
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.github.ssenerg.gochat.core.state.SignOuts
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue
import kotlin.time.Duration.Companion.seconds

/** A session signed out of offline ends once the server answers, so it pushes to the phone no more. */
@OptIn(ExperimentalCoroutinesApi::class)
class SignOutsTest {
    @Test
    fun aSessionSignedOutOfflineEndsOnceTheServerAnswers() = runTest {
        val store = MemoryStore()
        val tried = mutableListOf<Pair<String, String>>()
        var offline = 2
        val signOuts = SignOuts(store, backgroundScope) { server, token ->
            tried += server to token
            if (offline-- > 0) throw ConnectionLost("Not connected.")
        }
        signOuts.add("https://chat.test", "tok")
        runCurrent()
        assertEquals(1, tried.size)
        advanceTimeBy(4.seconds)
        assertEquals(1, tried.size)
        advanceTimeBy(2.seconds)
        assertEquals(2, tried.size)
        // It waits longer each time.
        advanceTimeBy(9.seconds)
        assertEquals(2, tried.size)
        advanceTimeBy(2.seconds)
        assertEquals(listOf("https://chat.test" to "tok"), tried.distinct())
        assertEquals(3, tried.size)
        assertNull(store.get("signing_out"))
        advanceTimeBy(600.seconds)
        assertEquals(3, tried.size)
    }

    @Test
    fun theNextLaunchGoesOn() = runTest {
        val store = MemoryStore()
        SignOuts(store, backgroundScope) { _, _ -> throw ConnectionLost("Not connected.") }.add("https://chat.test", "tok")
        runCurrent()
        assertTrue(store.get("signing_out") != null)

        val tried = mutableListOf<String>()
        SignOuts(store, backgroundScope) { _, token -> tried += token }.retry()
        runCurrent()
        assertEquals(listOf("tok"), tried)
        assertNull(store.get("signing_out"))
    }

    @Test
    fun aPushCutsTheWaitShort() = runTest {
        val store = MemoryStore()
        var tried = 0
        val signOuts = SignOuts(store, backgroundScope) { _, _ ->
            if (++tried < 3) throw ConnectionLost("Not connected.")
        }
        signOuts.add("https://chat.test", "tok")
        runCurrent()
        advanceTimeBy(2.seconds)
        assertEquals(1, tried)
        signOuts.retry()
        runCurrent()
        assertEquals(2, tried)
        // The wait starts over.
        advanceTimeBy(4.seconds)
        assertEquals(2, tried)
        advanceTimeBy(2.seconds)
        assertEquals(3, tried)
        assertNull(store.get("signing_out"))
    }

    @Test
    fun oneTryAtATime() = runTest {
        val store = MemoryStore()
        val answer = CompletableDeferred<Unit>()
        var tried = 0
        val signOuts = SignOuts(store, backgroundScope) { _, _ ->
            tried++
            answer.await()
        }
        signOuts.add("https://chat.test", "tok")
        runCurrent()
        signOuts.retry()
        runCurrent()
        assertEquals(1, tried)
        answer.complete(Unit)
        runCurrent()
        assertEquals(1, tried)
        assertNull(store.get("signing_out"))
    }

    @Test
    fun onlyAServerThatCannotAnswerIsAskedAgain() = runTest {
        assertTrue(SignOuts.settled {})
        assertTrue(SignOuts.settled { throw ApiException(ApiException.UNAUTHENTICATED, "Sign in again.") })
        assertTrue(SignOuts.settled { throw ApiException(ApiException.PERMISSION_DENIED, "No.") })
        assertFalse(SignOuts.settled { throw ApiException(ApiException.UNAVAILABLE, "Later.") })
        assertFalse(SignOuts.settled { throw ApiException(ApiException.RESOURCE_EXHAUSTED, "Slow down.") })
        assertFalse(SignOuts.settled { throw ConnectionLost("Not connected.") })
    }

    @Test
    fun aFewWaitAtMost() = runTest {
        val store = MemoryStore()
        val signOuts = SignOuts(store, backgroundScope) { _, _ -> throw ConnectionLost("Not connected.") }
        for (i in 1..6) signOuts.add("https://chat.test", "tok$i")
        signOuts.add("https://chat.test", "tok6")
        runCurrent()
        assertEquals((3..6).map { "https://chat.test\ttok$it" }, store.get("signing_out")?.lines())
    }
}
