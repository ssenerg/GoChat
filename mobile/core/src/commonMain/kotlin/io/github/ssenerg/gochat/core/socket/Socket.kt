package io.github.ssenerg.gochat.core.socket

import gochat.v1.AppView
import gochat.v1.AppWatch
import gochat.v1.Down
import gochat.v1.Event
import gochat.v1.Ping
import gochat.v1.SocketHello
import gochat.v1.SocketWelcome
import gochat.v1.Up
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.ConnectionLost
import io.github.ssenerg.gochat.core.api.ServerRefused
import io.ktor.client.HttpClient
import io.ktor.client.plugins.timeout
import io.ktor.client.plugins.websocket.webSocket
import io.ktor.client.request.get
import io.ktor.client.request.header
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.websocket.Frame
import io.ktor.websocket.readBytes
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.ReceiveChannel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import okio.ByteString.Companion.toByteString
import kotlin.concurrent.Volatile
import kotlin.coroutines.CoroutineContext
import kotlin.random.Random
import kotlin.time.Duration.Companion.minutes
import kotlin.time.TimeSource

/** Where the socket stands. */
sealed interface Connection {
    data object Connecting : Connection

    /** Calls and updates flow. */
    data class Online(val welcome: SocketWelcome) : Connection

    /** The socket is down; another attempt follows. */
    data class Offline(val reason: String) : Connection

    /** The session is no longer valid: the person signs in again. */
    data object SignedOut : Connection

    /** The server does not take this app, which stops trying. */
    data class Refused(val reason: String) : Connection
}

/** What the socket hands the session, in the order it came. */
sealed interface SocketEvent {
    /**
     * The socket serves the session, again after each reconnect. With
     * `reload` set, updates were lost: what the app shows loads again.
     */
    data class Welcome(val welcome: SocketWelcome) : SocketEvent

    /** Numbered updates, oldest first. */
    data class Updates(val events: List<Event>) : SocketEvent

    /** Typing, presence, profiles, button answers: never numbered, never replayed. */
    data class Ephemeral(val event: Event) : SocketEvent
}

/** An open WebSocket, as the socket uses it. */
internal interface Link {
    /** The next binary message, or null once the socket closed. */
    suspend fun receive(): ByteArray?

    suspend fun send(bytes: ByteArray)

    /** The code the server closed the socket with, once [receive] gave null. */
    suspend fun closeCode(): Int?
}

/** Opens WebSockets: Ktor's in the apps, a fake in tests. */
internal fun interface Dialer {
    suspend fun dial(url: String, protocol: String, use: suspend (Link) -> Unit)
}

/** Thrown when a server answers the socket's address with 404: it predates the protocol. */
internal class Unsupported : Exception("This server is older than the app: ask its admin to update it.")

internal class KtorDialer(private val http: HttpClient) : Dialer {
    override suspend fun dial(url: String, protocol: String, use: suspend (Link) -> Unit) {
        var opened = false
        try {
            http.webSocket(urlString = url, request = { header(HttpHeaders.SecWebSocketProtocol, protocol) }) {
                opened = true
                val ws = this
                use(object : Link {
                    override suspend fun receive(): ByteArray? {
                        while (true) {
                            val frame = ws.incoming.receiveCatching().getOrNull() ?: return null
                            if (frame is Frame.Binary) return frame.readBytes()
                        }
                    }

                    override suspend fun send(bytes: ByteArray) = ws.outgoing.send(Frame.Binary(true, bytes))

                    override suspend fun closeCode(): Int? = ws.closeReason.await()?.code?.toInt()
                })
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // A refused handshake reads differently on each platform: ask
            // plainly whether the address exists.
            if (!opened && notFound(url)) throw Unsupported()
            throw e
        }
    }

    private suspend fun notFound(url: String): Boolean = try {
        val plain = url.replaceFirst("wss://", "https://").replaceFirst("ws://", "http://")
        http.get(plain) { timeout { requestTimeoutMillis = 10_000 } }.status == HttpStatusCode.NotFound
    } catch (e: CancellationException) {
        throw e
    } catch (_: Exception) {
        false
    }
}

/**
 * The socket of the GoChat protocol (docs/protocol.md): one WebSocket for
 * the chat API's calls and the session's updates. It connects when first
 * used, reconnects with backoff, and resumes the account's updates after
 * [pts]. Without a token it is anonymous, to sign in over; [authenticate]
 * then says hello again on the same socket.
 */
class Socket internal constructor(
    val url: String,
    /** Names the app in the session list, such as "GoChat/0.3.0 (Android 16; Pixel 8)". */
    private val client: String,
    token: String?,
    private val dialer: Dialer,
    // Single-threaded: the socket's state is touched there only.
    context: CoroutineContext = Dispatchers.Default.limitedParallelism(1),
) {
    private val serial = context
    private val scope = CoroutineScope(SupervisorJob() + context)

    private val _connection = MutableStateFlow<Connection>(Connection.Offline("Not connected"))
    val connection: StateFlow<Connection> = _connection.asStateFlow()

    private val _events = Channel<SocketEvent>(Channel.UNLIMITED)

    /** Welcomes and updates, kept until the session reads them; it alone does. */
    val events: ReceiveChannel<SocketEvent> get() = _events

    /** The last account update the app applied: each hello resumes after it. */
    @Volatile
    var pts: Long = 0

    @Volatile
    private var closed = false

    private var token: String? = token
    private var job: Job? = null
    private var link: Link? = null
    private var anonymous = false
    private var welcomed = CompletableDeferred<SocketWelcome>()
    private var stopped = false
    private var nextId = 0L
    private val pending = HashMap<Long, CompletableDeferred<gochat.v1.Result>>()
    private var viewing = ""
    private var watching = emptyList<String>()
    private var delayMs = MIN_DELAY_MS
    private var pingSeq = 0L
    private var awaitingPong = 0L

    /** Connects unless it is already, and stays connected until [stop]. */
    fun start() {
        scope.launch {
            stopped = false
            launchIfIdle()
        }
    }

    /** Closes the socket until [start]; calls fail meanwhile. */
    fun stop() {
        scope.launch {
            stopped = true
            job?.cancel()
            job = null
            fail(ConnectionLost("Not connected."))
            _connection.value = Connection.Offline("Not connected")
        }
    }

    /** Tries again now rather than after the wait, as when the app comes back to the foreground. */
    fun retryNow() {
        scope.launch {
            if (stopped || job?.isActive != true || _connection.value !is Connection.Offline) return@launch
            job?.cancel()
            job = null
            launchIfIdle()
        }
    }

    /** Closes the socket for good. */
    fun close() {
        closed = true
        _events.close()
        _connection.value = Connection.Offline("Not connected")
        scope.launch { fail(ConnectionLost("Not connected.")) }.invokeOnCompletion { scope.cancel() }
    }

    /** Connects and waits for the first outcome: online, or why not. */
    suspend fun connect(): Connection {
        withContext(serial) {
            stopped = false
            launchIfIdle()
        }
        return connection.first { it !is Connection.Connecting }
    }

    /** Signs the socket in with [token]: a hello on the open socket, or the next one's. */
    suspend fun authenticate(token: String): SocketWelcome {
        val ready = withContext(serial) { useTokenNow(token) }
        return withTimeoutOrNull(CALL_TIMEOUT_MS) { ready.await() } ?: throw ConnectionLost("The server did not answer in time.")
    }

    /** Uses [token] from now on, as [authenticate] does without waiting. */
    fun useToken(token: String) {
        scope.launch { useTokenNow(token) }
    }

    private suspend fun useTokenNow(token: String): CompletableDeferred<SocketWelcome> {
        if (this.token == token && !anonymous && link != null) return welcomed
        this.token = token
        stopped = false
        val l = link
        if (l != null && anonymous) {
            if (welcomed.isCompleted) welcomed = CompletableDeferred()
            try {
                hello(l)
            } catch (e: CancellationException) {
                throw e
            } catch (_: Exception) {
                // The socket dropped: the next one says hello with the token.
            }
        }
        launchIfIdle()
        return welcomed
    }

    /** Makes one call of the chat API, such as "gochat.v1.MessageService/PostMessage". */
    suspend fun call(method: String, payload: ByteArray): ByteArray {
        val result = withTimeoutOrNull(CALL_TIMEOUT_MS) { exchange(method, payload) }
            ?: throw ConnectionLost("The server did not answer in time.")
        result.error?.let { throw ApiException(it.code, it.message.ifEmpty { "The server refused the request." }) }
        return result.payload?.toByteArray() ?: ByteArray(0)
    }

    private suspend fun exchange(method: String, payload: ByteArray): gochat.v1.Result {
        while (true) {
            val ready = withContext(serial) {
                if (closed || stopped) throw ConnectionLost("Not connected.")
                launchIfIdle()
                welcomed
            }
            ready.await()
            val sent = withContext(serial) {
                val l = link
                if (l == null || !welcomed.isCompleted) return@withContext null
                val id = ++nextId
                val answer = CompletableDeferred<gochat.v1.Result>()
                pending[id] = answer
                try {
                    l.send(Up.ADAPTER.encode(Up(call = gochat.v1.Call(id = id, method = method, payload = payload.toByteString()))))
                } catch (e: CancellationException) {
                    pending.remove(id)
                    throw e
                } catch (_: Exception) {
                    pending.remove(id)
                    throw ConnectionLost("The connection dropped.")
                }
                id to answer
            } ?: continue // dropped since the welcome: wait for the next
            try {
                return sent.second.await()
            } catch (e: Throwable) {
                // An answer that comes takes its entry out itself.
                withContext(NonCancellable + serial) { pending.remove(sent.first) }
                throw e
            }
        }
    }

    /** Says which channel the app shows, null for none: typing comes for it alone. */
    fun view(channelId: String?) {
        scope.launch {
            viewing = channelId.orEmpty()
            tell(Up(view = AppView(channel_id = viewing)))
        }
    }

    /** Says whose presence and profile the app shows. */
    fun watch(userIds: List<String>) {
        scope.launch {
            watching = userIds.take(MAX_WATCHED)
            tell(Up(watch = AppWatch(user_ids = watching)))
        }
    }

    private suspend fun tell(up: Up) {
        val l = link ?: return
        if (anonymous || !welcomed.isCompleted) return
        try {
            l.send(Up.ADAPTER.encode(up))
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            // The next welcome says it again.
        }
    }

    private fun launchIfIdle() {
        if (job?.isActive == true || closed || stopped) return
        _connection.value = Connection.Connecting
        job = scope.launch { run() }
    }

    private suspend fun run() {
        while (true) {
            _connection.value = Connection.Connecting
            val started = TimeSource.Monotonic.markNow()
            val outcome = try {
                session()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Unsupported) {
                Outcome.Refused(e.message.orEmpty())
            } catch (_: Exception) {
                Outcome.Retry("Cannot reach the server")
            } finally {
                down()
            }
            when (outcome) {
                Outcome.SignedOut -> {
                    fail(ApiException(ApiException.UNAUTHENTICATED, "Sign in again."))
                    _connection.value = Connection.SignedOut
                    return
                }
                is Outcome.Refused -> {
                    fail(ServerRefused(outcome.reason))
                    _connection.value = Connection.Refused(outcome.reason)
                    return
                }
                is Outcome.Retry -> {
                    _connection.value = Connection.Offline(outcome.reason)
                    if (started.elapsedNow() > 1.minutes) delayMs = MIN_DELAY_MS
                    val wait = if (outcome.slow) SLOW_DELAY_MS else delayMs
                    delay(wait / 2 + Random.nextLong(wait / 2 + 1))
                    delayMs = minOf(delayMs * 2, MAX_DELAY_MS)
                }
            }
        }
    }

    /** One connection, from the hello until it closes. */
    private suspend fun session(): Outcome {
        var code: Int? = null
        dialer.dial(url, PROTOCOL) { l ->
            link = l
            hello(l)
            coroutineScope {
                val pinger = launch { keepAlive(l) }
                while (true) {
                    val bytes = l.receive() ?: break
                    val down = runCatching { Down.ADAPTER.decode(bytes) }.getOrNull() ?: continue
                    receive(down)
                }
                pinger.cancel()
            }
            code = l.closeCode()
        }
        return Outcome.of(code)
    }

    private suspend fun hello(l: Link) {
        val t = token
        anonymous = t == null
        val hello = SocketHello(token = t.orEmpty(), client = client, layer = LAYER, pts = if (t == null) 0 else pts)
        l.send(Up.ADAPTER.encode(Up(hello = hello)))
    }

    private suspend fun receive(d: Down) {
        d.welcome?.let { welcome(it) }
        d.result?.let { pending.remove(it.id)?.complete(it) }
        d.updates?.let { if (it.updates.isNotEmpty()) _events.trySend(SocketEvent.Updates(it.updates)) }
        d.ephemeral?.let { _events.trySend(SocketEvent.Ephemeral(it)) }
        d.pong?.let { if (it.id == awaitingPong) awaitingPong = 0 }
    }

    private suspend fun welcome(w: SocketWelcome) {
        // An anonymous welcome that a signed-in hello has overtaken.
        if (w.user == null && !anonymous) return
        welcomed.complete(w)
        _connection.value = Connection.Online(w)
        if (w.user == null) return
        delayMs = MIN_DELAY_MS
        // What the app shows holds per socket: say it on each.
        tell(Up(view = AppView(channel_id = viewing)))
        tell(Up(watch = AppWatch(user_ids = watching)))
        _events.trySend(SocketEvent.Welcome(w))
    }

    /** Pings the server, giving up on a socket that stopped answering. */
    private suspend fun keepAlive(l: Link) {
        while (true) {
            delay(PING_EVERY_MS)
            val id = ++pingSeq
            awaitingPong = id
            l.send(Up.ADAPTER.encode(Up(ping = Ping(id = id))))
            delay(PONG_WAIT_MS)
            if (awaitingPong == id) throw ConnectionLost("The server stopped answering.")
        }
    }

    /** The socket closed: the calls in flight fail, and new ones wait for the next welcome. */
    private fun down() {
        link = null
        awaitingPong = 0
        if (welcomed.isCompleted) welcomed = CompletableDeferred()
        val lost = ConnectionLost("The connection dropped.")
        pending.values.forEach { it.completeExceptionally(lost) }
        pending.clear()
    }

    /** Fails the calls waiting for a welcome too. */
    private fun fail(e: Exception) {
        down()
        welcomed.completeExceptionally(e)
        welcomed = CompletableDeferred()
    }

    private sealed interface Outcome {
        data object SignedOut : Outcome

        data class Refused(val reason: String) : Outcome

        data class Retry(val reason: String, val slow: Boolean = false) : Outcome

        companion object {
            fun of(code: Int?): Outcome = when (code) {
                CLOSE_UNAUTHENTICATED, CLOSE_REVOKED -> SignedOut
                CLOSE_OLD_LAYER -> Refused("This server needs a newer version of the app. Update the app.")
                CLOSE_TOO_MANY -> Retry("Too many devices connected", slow = true)
                CLOSE_PROTOCOL -> Retry("The server did not understand the app", slow = true)
                CLOSE_TOO_SLOW -> Retry("Catching up")
                else -> Retry("Reconnecting")
            }
        }
    }

    companion object {
        const val PROTOCOL = "gochat.v2"

        /** The protocol layer this app speaks. */
        const val LAYER = 1

        // The close codes of docs/protocol.md.
        const val CLOSE_RETRY = 4000
        const val CLOSE_UNAUTHENTICATED = 4001
        const val CLOSE_PROTOCOL = 4002
        const val CLOSE_REVOKED = 4003
        const val CLOSE_TOO_MANY = 4008
        const val CLOSE_TOO_SLOW = 4009
        const val CLOSE_OLD_LAYER = 4011

        /** The most people the server takes in one watch. */
        const val MAX_WATCHED = 1000

        internal const val CALL_TIMEOUT_MS = 30_000L
        internal const val PING_EVERY_MS = 30_000L
        internal const val PONG_WAIT_MS = 15_000L
        private const val MIN_DELAY_MS = 1_000L
        private const val MAX_DELAY_MS = 30_000L
        private const val SLOW_DELAY_MS = 60_000L
    }
}
