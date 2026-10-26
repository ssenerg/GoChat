package io.github.ssenerg.gochat.core

import gochat.v1.ApiError
import gochat.v1.Down
import gochat.v1.Event
import gochat.v1.SocketWelcome
import gochat.v1.Typing
import gochat.v1.Up
import gochat.v1.Updates
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.ConnectionLost
import io.github.ssenerg.gochat.core.api.ServerRefused
import io.github.ssenerg.gochat.core.socket.Connection
import io.github.ssenerg.gochat.core.socket.Dialer
import io.github.ssenerg.gochat.core.socket.Link
import io.github.ssenerg.gochat.core.socket.Socket
import io.github.ssenerg.gochat.core.socket.SocketEvent
import kotlinx.coroutines.async
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runTest
import okio.ByteString.Companion.encodeUtf8
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertIs
import kotlin.test.assertTrue

/** The socket speaks docs/protocol.md to a scripted server. */
class SocketTest {
    /** One socket as the server sees it. */
    private class FakeLink : Link {
        val toClient = Channel<ByteArray>(Channel.UNLIMITED)
        val fromClient = Channel<Up>(Channel.UNLIMITED)
        var code: Int? = null

        override suspend fun receive(): ByteArray? = toClient.receiveCatching().getOrNull()

        override suspend fun send(bytes: ByteArray) {
            fromClient.send(Up.ADAPTER.decode(bytes))
        }

        override suspend fun closeCode(): Int? = code

        fun down(d: Down) {
            toClient.trySend(Down.ADAPTER.encode(d))
        }

        fun close(code: Int?) {
            this.code = code
            toClient.close()
        }

        /** The next frame of a kind, skipping the others. */
        suspend fun <T> next(pick: (Up) -> T?): T {
            while (true) pick(fromClient.receive())?.let { return it }
        }

        suspend fun hello() = next { it.hello }

        suspend fun call() = next { it.call }
    }

    private val links = Channel<FakeLink>(Channel.UNLIMITED)
    private val dialer = Dialer { _, protocol, use ->
        assertEquals("gochat.v2", protocol)
        val link = FakeLink()
        links.send(link)
        use(link)
    }
    private val ann = User(id = "u-ann", username = "ann")

    private fun TestScope.socket(token: String?) =
        Socket("ws://chat.test/api/v2/socket", "GoChat/0.3.0 (test)", token, dialer, StandardTestDispatcher(testScheduler))

    private fun welcome(user: User? = null, pts: Long = 0) = Down(welcome = SocketWelcome(user = user, pts = pts, layer = 1))

    private fun result(id: Long, payload: String) = Down(result = gochat.v1.Result(id = id, payload = payload.encodeUtf8()))

    @Test
    fun callsShareTheSocketAndAnswerByTheirIds() = runTest {
        val socket = socket("tok")
        socket.pts = 5
        try {
            val first = async { socket.call("gochat.v1.A/One", byteArrayOf(1)).decodeToString() }
            val link = links.receive()
            val hello = link.hello()
            assertEquals("tok", hello.token)
            assertEquals(1, hello.layer)
            assertEquals(5, hello.pts)
            assertEquals("GoChat/0.3.0 (test)", hello.client)
            link.down(welcome(ann, pts = 5))
            assertEquals(SocketEvent.Welcome(SocketWelcome(user = ann, pts = 5, layer = 1)), socket.events.receive())
            val second = async { socket.call("gochat.v1.A/Two", byteArrayOf(2)).decodeToString() }
            val calls = listOf(link.call(), link.call()).associateBy { it.method }
            assertTrue(calls.getValue("gochat.v1.A/One").id != calls.getValue("gochat.v1.A/Two").id)
            // Results come in any order.
            link.down(result(calls.getValue("gochat.v1.A/Two").id, "two"))
            link.down(result(calls.getValue("gochat.v1.A/One").id, "one"))
            assertEquals("one", first.await())
            assertEquals("two", second.await())

            val refused = async { runCatching { socket.call("gochat.v1.A/Three", ByteArray(0)) } }
            link.down(Down(result = gochat.v1.Result(id = link.call().id, error = ApiError(code = 7, message = "permission denied"))))
            assertEquals(7, assertIs<ApiException>(refused.await().exceptionOrNull()).code)
        } finally {
            socket.close()
        }
    }

    @Test
    fun anAnonymousSocketSignsInWithASecondHello() = runTest {
        val socket = socket(null)
        try {
            val login = async { socket.call("gochat.v1.AuthService/Login", ByteArray(0)).decodeToString() }
            val link = links.receive()
            assertEquals("", link.hello().token)
            link.down(welcome())
            link.down(result(link.call().id, "session"))
            assertEquals("session", login.await())

            val signedIn = async { socket.authenticate("tok") }
            val again = link.hello()
            assertEquals("tok", again.token)
            link.down(welcome(ann, pts = 9))
            assertEquals(ann, signedIn.await().user)
            // The session hears of the signed-in welcome alone.
            assertEquals(9, assertIs<SocketEvent.Welcome>(socket.events.receive()).welcome.pts)
        } finally {
            socket.close()
        }
    }

    @Test
    fun aDroppedSocketResumesAfterTheLastUpdateApplied() = runTest {
        val socket = socket("tok")
        try {
            socket.start()
            val link = links.receive()
            link.hello()
            link.down(welcome(ann, pts = 40))
            socket.connection.first { it is Connection.Online }
            link.down(Down(updates = Updates(updates = listOf(Event(pts = 41, pts_count = 1)))))
            link.down(Down(ephemeral = Event(typing = Typing(channel_id = "c1", user = ann))))
            socket.events.receive()
            assertEquals(listOf(41L), assertIs<SocketEvent.Updates>(socket.events.receive()).events.map { it.pts })
            assertIs<SocketEvent.Ephemeral>(socket.events.receive())
            socket.pts = 41
            socket.view("c1")

            val lost = async { runCatching { socket.call("gochat.v1.A/One", ByteArray(0)) } }
            link.call()
            link.close(Socket.CLOSE_TOO_SLOW)
            assertIs<ConnectionLost>(lost.await().exceptionOrNull())

            // After a backoff, the next socket says hello from 41, and what
            // the app shows again.
            val next = links.receive()
            assertEquals(41, next.hello().pts)
            next.down(welcome(ann, pts = 41))
            assertEquals("c1", next.next { it.view }.channel_id)
            next.next { it.watch }
        } finally {
            socket.close()
        }
    }

    @Test
    fun closeCodesSayWhatToDo() = runTest {
        val signedOut = socket("old")
        try {
            val waiting = async { runCatching { signedOut.call("gochat.v1.A/One", ByteArray(0)) } }
            val link = links.receive()
            link.hello()
            link.close(Socket.CLOSE_UNAUTHENTICATED)
            assertTrue(assertIs<ApiException>(waiting.await().exceptionOrNull()).isUnauthenticated)
            assertEquals(Connection.SignedOut, signedOut.connection.value)
        } finally {
            signedOut.close()
        }

        val tooOld = socket("tok")
        try {
            val waiting = async { runCatching { tooOld.call("gochat.v1.A/One", ByteArray(0)) } }
            val link = links.receive()
            link.hello()
            link.close(Socket.CLOSE_OLD_LAYER)
            assertTrue(assertIs<ServerRefused>(waiting.await().exceptionOrNull()).message!!.contains("Update the app"))
            assertIs<Connection.Refused>(tooOld.connection.value)
        } finally {
            tooOld.close()
        }
    }

    @Test
    fun aStoppedSocketFailsCallsAtOnce() = runTest {
        val socket = socket("tok")
        try {
            socket.start()
            links.receive().let { link ->
                link.hello()
                link.down(welcome(ann))
            }
            socket.connection.first { it is Connection.Online }
            socket.stop()
            socket.connection.first { it is Connection.Offline }
            assertFailsWith<ConnectionLost> { socket.call("gochat.v1.A/One", ByteArray(0)) }
        } finally {
            socket.close()
        }
    }
}
