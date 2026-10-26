package io.github.ssenerg.gochat.core.api

import com.squareup.wire.Message
import com.squareup.wire.ProtoAdapter
import gochat.v1.ApiError
import gochat.v1.Attachment
import gochat.v1.ServerInfo
import gochat.v1.User
import io.github.ssenerg.gochat.core.socket.KtorDialer
import io.github.ssenerg.gochat.core.socket.Socket
import io.ktor.client.HttpClient
import io.ktor.client.HttpClientConfig
import io.ktor.client.call.body
import io.ktor.client.engine.HttpClientEngine
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.plugins.UserAgent
import io.ktor.client.plugins.timeout
import io.ktor.client.plugins.websocket.WebSockets
import io.ktor.client.request.HttpRequestBuilder
import io.ktor.client.request.accept
import io.ktor.client.request.get
import io.ktor.client.request.header
import io.ktor.client.request.parameter
import io.ktor.client.request.post
import io.ktor.client.request.setBody
import io.ktor.client.statement.HttpResponse
import io.ktor.http.ContentType
import io.ktor.http.HttpHeaders
import io.ktor.http.contentType
import io.ktor.http.isSuccess
import kotlinx.coroutines.CancellationException

/** A call the server refused: the chat API's error code and its reason. */
class ApiException(val code: Int, message: String, val status: Int = 0) : Exception(message) {
    val isUnauthenticated: Boolean get() = code == UNAUTHENTICATED

    companion object {
        const val INVALID_ARGUMENT = 3
        const val NOT_FOUND = 5
        const val ALREADY_EXISTS = 6
        const val PERMISSION_DENIED = 7
        const val RESOURCE_EXHAUSTED = 8
        const val FAILED_PRECONDITION = 9
        const val UNAVAILABLE = 14
        const val UNAUTHENTICATED = 16
    }
}

/**
 * The socket dropped, or never came up, before the server answered: whether
 * it made the call is unknown.
 */
class ConnectionLost(message: String) : Exception(message)

/** The server does not take this app: it is too old for the server, or the server for it. */
class ServerRefused(message: String) : Exception(message)

/** What to tell a person about a failure. */
fun Throwable.userMessage(): String = when (this) {
    is ApiException -> message?.replaceFirstChar { it.uppercase() } ?: "Something went wrong."
    is ServerRefused -> message ?: "The server does not take this app."
    is CancellationException -> "Cancelled."
    else -> "Cannot reach the server. Check your connection."
}

private val PROTO = ContentType("application", "proto")

/**
 * The app that talks to the server: its version, and the system and device
 * it runs on, such as "Android 16; Pixel 8". It goes in the User-Agent, and
 * the server names the session after the device in its session list.
 */
class AppInfo(val version: String, device: String) {
    // A header carries printable ASCII; the parentheses would end it early.
    val device: String = device.filter { it in ' '..'~' && it != '(' && it != ')' }.trim()

    val userAgent: String get() = "GoChat/$version ($device)"

    /** Whether this app is older than [min], such as "0.3"; an empty one takes any. */
    fun olderThan(min: String): Boolean {
        if (min.isBlank()) return false
        val have = parts(version)
        val need = parts(min)
        for (i in 0 until maxOf(have.size, need.size)) {
            val a = have.getOrElse(i) { 0 }
            val b = need.getOrElse(i) { 0 }
            if (a != b) return a < b
        }
        return false
    }

    // "0.2.0-debug" counts as 0.2.0.
    private fun parts(v: String) = v.trim().takeWhile { it.isDigit() || it == '.' }.split('.').map { it.toIntOrNull() ?: 0 }
}

/** Makes the chat API's calls: the socket in the apps, a stub in tests. */
fun interface Caller {
    suspend fun call(method: String, payload: ByteArray): ByteArray
}

/**
 * A GoChat server: its chat API over the GoChat protocol's socket
 * (docs/protocol.md in the server's repository), and files over HTTP with
 * the session token in the Authorization header.
 */
class ApiClient(
    baseUrl: String,
    token: String? = null,
    engine: HttpClientEngine? = null,
    app: AppInfo? = null,
    caller: Caller? = null,
) {
    val baseUrl: String = normalizeServerUrl(baseUrl)

    /** The session token, once signed in; setting it signs the socket in. */
    var token: String? = token
        set(value) {
            field = value
            if (value != null) socket.useToken(value)
        }

    val http: HttpClient = if (engine != null) HttpClient(engine) { configure(app) } else HttpClient { configure(app) }

    private val opened = lazy { Socket(socketUrl, app?.userAgent ?: "GoChat", token, KtorDialer(http)) }

    /** The socket calls and updates come over; it connects when first used. */
    val socket: Socket by opened

    private val caller: Caller = caller ?: Caller { method, payload -> socket.call(method, payload) }

    private fun HttpClientConfig<*>.configure(app: AppInfo?) {
        expectSuccess = false
        if (app != null) install(UserAgent) { agent = app.userAgent }
        install(WebSockets)
        install(HttpTimeout) {
            connectTimeoutMillis = 15_000
        }
    }

    internal fun HttpRequestBuilder.authorize() {
        token?.let { header(HttpHeaders.Authorization, "Bearer $it") }
    }

    /** Makes one call of the chat API, such as "gochat.v1.ChannelService/GetChannel". */
    suspend fun <Res : Any> call(method: String, request: Message<*, *>, response: ProtoAdapter<Res>): Res =
        response.decode(caller.call(method, request.encode()))

    /**
     * What the server says of itself: its version and the oldest app it
     * takes. It needs no token. The protocol has no such call yet, so it
     * stays at /api/v1.
     */
    suspend fun info(): ServerInfo {
        val res = http.get("$baseUrl/api/v1/info") {
            timeout { requestTimeoutMillis = 15_000 }
            accept(PROTO)
        }
        return decode(res, ServerInfo.ADAPTER)
    }

    /** Uploads one attachment of a channel, a voice message when [voice]. */
    suspend fun upload(
        channelId: String,
        name: String,
        bytes: ByteArray,
        voice: Boolean = false,
        durationMs: Long = 0,
    ): Attachment {
        val res = http.post("$baseUrl/api/v2/files") {
            timeout { requestTimeoutMillis = 10 * 60_000 }
            authorize()
            accept(PROTO)
            parameter("channel_id", channelId)
            parameter("name", name)
            if (voice) {
                parameter("voice", "1")
                parameter("duration_ms", durationMs.toString())
            }
            contentType(ContentType.Application.OctetStream)
            setBody(bytes)
        }
        return decode(res, Attachment.ADAPTER)
    }

    /** Downloads an attachment. */
    suspend fun download(attachmentId: String): ByteArray {
        val res = http.get(fileUrl(attachmentId)) {
            timeout { requestTimeoutMillis = 10 * 60_000 }
            authorize()
            accept(PROTO)
        }
        if (!res.status.isSuccess()) throw failure(res)
        return res.body()
    }

    /** Sets the caller's profile photo from any picture; /api/v2 has no photos yet. */
    suspend fun setPhoto(picture: ByteArray): User {
        val res = http.post("$baseUrl/api/v1/photo") {
            timeout { requestTimeoutMillis = 2 * 60_000 }
            authorize()
            accept(PROTO)
            contentType(ContentType.Application.OctetStream)
            setBody(picture)
        }
        return decode(res, User.ADAPTER)
    }

    fun fileUrl(attachmentId: String) = "$baseUrl/api/v2/files/$attachmentId"

    /** Where a person's photo is, or null when they have none. */
    fun userPhotoUrl(user: User?): String? =
        user?.takeIf { it.photo.isNotEmpty() }?.let { "$baseUrl/api/v1/photos/u/${it.id}/${it.photo}" }

    fun teamPhotoUrl(teamId: String, photo: String): String? =
        photo.takeIf { it.isNotEmpty() }?.let { "$baseUrl/api/v1/photos/t/$teamId/$it" }

    /** The socket's address. */
    val socketUrl: String
        get() = baseUrl.replaceFirst("https://", "wss://").replaceFirst("http://", "ws://") + "/api/v2/socket"

    fun close() {
        if (opened.isInitialized()) socket.close()
        http.close()
    }

    private suspend fun <T : Any> decode(res: HttpResponse, adapter: ProtoAdapter<T>): T {
        if (!res.status.isSuccess()) throw failure(res)
        return adapter.decode(res.body<ByteArray>())
    }

    private suspend fun failure(res: HttpResponse): ApiException {
        val status = res.status.value
        val bytes = runCatching { res.body<ByteArray>() }.getOrNull()
        val isProto = res.headers[HttpHeaders.ContentType]?.startsWith("application/proto") == true
        if (bytes != null && isProto) {
            runCatching { ApiError.ADAPTER.decode(bytes) }.getOrNull()?.let {
                return ApiException(it.code, it.message.ifEmpty { "The server refused the request." }, status)
            }
        }
        return when (status) {
            401 -> ApiException(ApiException.UNAUTHENTICATED, "Sign in again.", status)
            429 -> ApiException(ApiException.RESOURCE_EXHAUSTED, "Too many requests, try again in a moment.", status)
            in 500..599 -> ApiException(ApiException.UNAVAILABLE, "The server is unavailable, try again later.", status)
            else -> ApiException(0, "The server refused the request ($status).", status)
        }
    }
}

/**
 * Turns what a person typed into a server address: https:// unless they
 * named the scheme, and no trailing slash.
 */
fun normalizeServerUrl(input: String): String {
    var url = input.trim().trimEnd('/')
    if (!url.startsWith("http://") && !url.startsWith("https://")) url = "https://$url"
    return url
}
