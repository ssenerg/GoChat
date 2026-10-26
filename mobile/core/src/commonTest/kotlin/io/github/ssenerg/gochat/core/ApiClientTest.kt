package io.github.ssenerg.gochat.core

import gochat.v1.ApiError
import gochat.v1.Attachment
import gochat.v1.GetChannelRequest
import gochat.v1.GetChannelResponse
import gochat.v1.Channel
import gochat.v1.ServerInfo
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.ApiException
import io.github.ssenerg.gochat.core.api.AppInfo
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.content.OutgoingContent
import io.ktor.http.headersOf
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class ApiClientTest {
    private val proto = headersOf(HttpHeaders.ContentType, "application/proto")

    @Test
    fun callsAreProtobufByName() = runTest {
        val api = ApiClient("chat.test", "tok", caller = { method, payload ->
            assertEquals("gochat.v1.ChannelService/GetChannel", method)
            assertEquals("c1", GetChannelRequest.ADAPTER.decode(payload).channel_id)
            GetChannelResponse(channel = Channel(id = "c1", display_name = "Town")).encode()
        })
        val res = api.call("gochat.v1.ChannelService/GetChannel", GetChannelRequest(channel_id = "c1"), GetChannelResponse.ADAPTER)
        assertEquals("Town", res.channel?.display_name)
    }

    @Test
    fun filesTakeTheTokenAtApiV2() = runTest {
        val engine = MockEngine { req ->
            assertEquals("https://chat.test/api/v2/files?channel_id=c1&name=a.txt", req.url.toString())
            assertEquals("Bearer tok", req.headers[HttpHeaders.Authorization])
            assertEquals("hi", (req.body as OutgoingContent.ByteArrayContent).bytes().decodeToString())
            respond(Attachment(id = "f1", name = "a.txt").encode(), HttpStatusCode.OK, proto)
        }
        val api = ApiClient("https://chat.test", "tok", engine)
        assertEquals("f1", api.upload("c1", "a.txt", "hi".encodeToByteArray()).id)
        assertEquals("https://chat.test/api/v2/files/f1", api.fileUrl("f1"))
    }

    @Test
    fun refusalsCarryTheServersReason() = runTest {
        val engine = MockEngine {
            respond(ApiError(code = 7, message = "permission denied").encode(), HttpStatusCode.Forbidden, proto)
        }
        val e = assertFailsWith<ApiException> { ApiClient("https://chat.test", "tok", engine).download("f1") }
        assertEquals(7, e.code)
        assertEquals("permission denied", e.message)
    }

    @Test
    fun aBareLimitStillReads() = runTest {
        val engine = MockEngine { respond("too many requests", HttpStatusCode.TooManyRequests) }
        val e = assertFailsWith<ApiException> { ApiClient("https://chat.test", engine = engine).download("f1") }
        assertEquals(ApiException.RESOURCE_EXHAUSTED, e.code)
        assertTrue(e.message!!.contains("Too many"))
    }

    @Test
    fun theSocketFollowsTheScheme() {
        assertEquals("wss://chat.test/api/v2/socket", ApiClient("https://chat.test").socketUrl)
        assertEquals("ws://localhost:8080/api/v2/socket", ApiClient("http://localhost:8080/").socketUrl)
    }

    @Test
    fun appsNameThemselves() = runTest {
        val seen = mutableListOf<String?>()
        val engine = MockEngine { req ->
            seen += req.headers[HttpHeaders.UserAgent]
            respond(ServerInfo(version = "1.4.0").encode(), HttpStatusCode.OK, proto)
        }
        val api = ApiClient("https://chat.test", engine = engine, app = AppInfo("0.2.0", "Android 16; Pixel (8)\u00e9"))
        assertEquals("1.4.0", api.info().version)
        assertEquals(listOf<String?>("GoChat/0.2.0 (Android 16; Pixel 8)"), seen)
    }

    @Test
    fun versionsCompareByTheirNumbers() {
        val app = AppInfo("0.2.0-debug", "Android 16")
        assertFalse(app.olderThan(""))
        assertFalse(app.olderThan("0.2"))
        assertFalse(app.olderThan("0.1.9"))
        assertTrue(app.olderThan("0.2.1"))
        assertTrue(app.olderThan("0.10"))
        assertTrue(app.olderThan("1"))
    }

    @Test
    fun aServerTooNewForTheAppSaysSoBeforeSigningIn() = runTest {
        val paths = mutableListOf<String>()
        val engine = MockEngine { req ->
            paths += req.url.encodedPath
            respond(ServerInfo(version = "2.0.0", min_app_version = "0.3").encode(), HttpStatusCode.OK, proto)
        }
        val app = GoChatApp(MemoryStore(), backgroundScope, engine, app = AppInfo("0.2.0", "iOS 18.4; iPhone"))
        val why = app.signIn("https://chat.test", "ann", "secret")
        assertTrue(why!!.contains("0.3"), why)
        assertEquals(listOf("/api/v1/info"), paths)

        // A server that takes any app, or cannot say, lets it sign in.
        val older = GoChatApp(MemoryStore(), backgroundScope, MockEngine { respond("", HttpStatusCode.NotFound) }, app = AppInfo("0.2.0", "x"))
        val refused = older.signIn("https://chat.test", "ann", "secret")
        assertFalse(refused.orEmpty().contains("Update the app"), refused)
    }
}
