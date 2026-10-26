package io.github.ssenerg.gochat.core

import gochat.v1.Channel
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.socket.Connection
import io.github.ssenerg.gochat.core.model.isConversation
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlin.random.Random
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/**
 * Drives a running stack as the apps do, when GOCHAT_E2E_URL and the admin's
 * account are set, as for the Go end-to-end suite.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class LiveServerTest {
    private val url = System.getenv("GOCHAT_E2E_URL")
    private val admin = System.getenv("GOCHAT_E2E_ADMIN")
    private val password = System.getenv("GOCHAT_E2E_ADMIN_PASSWORD")
    private val teamName = System.getenv("GOCHAT_E2E_TEAM")

    @Test
    fun aSessionFollowsTheServer() {
        if (url == null || admin == null || password == null || teamName == null) {
            println("GOCHAT_E2E_URL is not set: skipping")
            return
        }
        runBlocking {
            // The app's own thread: the session is used from one.
            val main = Dispatchers.Default.limitedParallelism(1)
            val scope = CoroutineScope(SupervisorJob() + main)
            try {
                withTimeout(60_000) { run(scope) }
            } finally {
                scope.cancel()
            }
        }
    }

    private suspend fun run(scope: CoroutineScope) {
        val store = MemoryStore()
        val app = GoChatApp(store, scope)
        val error = withContext(scope.coroutineContext) { app.signIn(url!!, admin!!, password!!) }
        assertEquals(null, error)
        val session = (app.state.value as AppState.SignedIn).session
        assertNotNull(store.get("token"), "the token is kept")

        val st = session.state.first { !it.loading && it.channels.isNotEmpty() }
        assertEquals(teamName, st.team?.name)
        val town = st.textChannels.first { it.name == "town-square" }
        session.socket.connection.first { it is Connection.Online }

        val timeline = withContext(scope.coroutineContext) { session.timeline(town.id) }
        timeline.state.first { !it.loading }

        // Another device of the same person posts: this one hears of it.
        val other = GoChatApi(ApiClient(url!!))
        other.client.token = other.login(admin!!, password!!).session!!.token
        val text = "from another device " + Random.nextInt(1_000_000)
        val posted = other.post(town.id, text)!!
        val seen = timeline.state.first { s -> s.messages.any { it.id == posted.id } }
        assertEquals(text, seen.messages.first { it.id == posted.id }.content)

        // This one posts: the message replaces its pending copy.
        withContext(scope.coroutineContext) { timeline.send("from the app's core") }
        val sent = timeline.state.first { s -> s.pending.isEmpty() && s.messages.any { it.content == "from the app's core" } }
        val mine = sent.messages.first { it.content == "from the app's core" }

        // Reactions come back as events and as the call's answer.
        withContext(scope.coroutineContext) { session.react(mine, "tada") }
        timeline.state.first { s -> s.messages.first { it.id == mine.id }.reactions.any { it.emoji == "tada" } }

        // A thread reply counts on its root.
        other.post(town.id, "a reply", rootId = mine.id)
        timeline.state.first { s -> s.messages.first { it.id == mine.id }.reply_count == 1 }
        val thread = withContext(scope.coroutineContext) { session.thread(town.id, mine.id) }
        val t = thread.state.first { !it.loading }
        assertEquals(mine.id, t.root?.id)
        assertTrue(t.replies.any { it.content == "a reply" })

        // An upload, a voice message among them, posts as an attachment.
        val webm = byteArrayOf(0x1a, 0x45, 0xdf.toByte(), 0xa3.toByte()) + ByteArray(64)
        val voice = session.upload(town.id, "voice.webm", webm, voice = true, durationMs = 1200)
        assertTrue(voice.voice)
        assertEquals(1200, voice.duration_ms)
        withContext(scope.coroutineContext) { timeline.send("", listOf(voice.id)) }
        timeline.state.first { s -> s.messages.any { m -> m.attachments.any { it.id == voice.id } } }
        assertEquals(webm.toList(), session.api.client.download(voice.id).toList())

        // Edits and deletes reach the timeline.
        assertEquals(null, session.edit(mine, "edited by the app"))
        timeline.state.first { s -> s.messages.first { it.id == mine.id }.content == "edited by the app" }
        other.delete(posted)
        timeline.state.first { s -> s.messages.none { it.id == posted.id } }

        // A conversation opens and joins the sidebar.
        val peer = session.api.teamMembers(st.teamId!!).first { it.user?.id != session.me.id }.user!!
        val dm: Channel = session.openConversation(listOf(peer.id))
        assertTrue(dm.isConversation)
        session.state.first { s -> s.channels.any { it.id == dm.id } }
        assertEquals(st.teamId, dm.team_id, "conversations belong to the team on screen")

        // Deleting it for oneself takes it off the sidebar.
        assertEquals(null, withContext(scope.coroutineContext) { session.deleteConversation(dm.id, everyone = false) })
        session.state.first { s -> s.channels.none { it.id == dm.id } }

        withContext(scope.coroutineContext) { app.signOut() }
        assertTrue(app.state.value is AppState.SignedOut)
        assertEquals(null, store.get("token"))
        other.logout()
        checkClosed(session)
    }

    private fun checkClosed(session: ChatSession) {
        assertTrue(session.socket.connection.value !is Connection.Online, "the socket closes with the session")
    }

    @Test
    fun anAppCatchesUpAfterBeingAway() {
        if (url == null || admin == null || password == null || teamName == null) {
            println("GOCHAT_E2E_URL is not set: skipping")
            return
        }
        runBlocking {
            val main = Dispatchers.Default.limitedParallelism(1)
            val scope = CoroutineScope(SupervisorJob() + main)
            try {
                withTimeout(60_000) { away(scope) }
            } finally {
                scope.cancel()
            }
        }
    }

    private suspend fun away(scope: CoroutineScope) {
        val store = MemoryStore()
        val app = GoChatApp(store, scope)
        assertEquals(null, withContext(scope.coroutineContext) { app.signIn(url!!, admin!!, password!!) })
        val session = (app.state.value as AppState.SignedIn).session
        val town = session.state.first { !it.loading && it.channels.isNotEmpty() }.textChannels.first { it.name == "town-square" }
        val timeline = withContext(scope.coroutineContext) {
            session.setVisible(town.id)
            session.timeline(town.id)
        }
        timeline.state.first { !it.loading }

        val other = GoChatApi(ApiClient(url!!))
        other.client.token = other.login(admin!!, password!!).session!!.token
        val before = other.post(town.id, "before going away")!!
        timeline.state.first { s -> s.messages.any { it.id == before.id } }

        // The app goes away: the phone is offline, or the app in the background.
        withContext(scope.coroutineContext) { session.socket.stop() }
        session.socket.connection.first { it !is Connection.Online }
        val missed = (1..3).map { other.post(town.id, "while away $it")!! }
        other.edit(before, "edited while away")
        other.react(missed[0], "tada", remove = false)
        other.delete(missed[1])
        // An update of the account: only the replay brings it, as catching up
        // does not ask for mutes again.
        other.setMuted(town.id, true)
        try {
            withContext(scope.coroutineContext) { session.socket.start() }
            val caught = timeline.state.first { s ->
                s.messages.any { it.id == missed[2].id } && s.messages.none { it.id == missed[1].id } &&
                    s.messages.first { it.id == before.id }.content == "edited while away" &&
                    s.messages.first { it.id == missed[0].id }.reactions.any { it.emoji == "tada" }
            }
            assertEquals(1, caught.messages.count { it.id == missed[0].id }, "each message once")
            session.state.first { town.id in it.muted }

            // The account's number is kept: a restarted app resumes after it.
            val kept = withTimeout(10_000) {
                while (store.get("pts") == null) kotlinx.coroutines.delay(100)
                store.get("pts")!!.toLong()
            }
            assertTrue(kept > 0)
            val restarted = GoChatApp(store, scope)
            withContext(scope.coroutineContext) { restarted.start() }
            val again = (restarted.state.first { it is AppState.SignedIn } as AppState.SignedIn).session
            val welcome = (again.socket.connection.first { it is Connection.Online } as Connection.Online).welcome
            assertTrue(welcome.pts >= kept && !welcome.reload, "resumed after $kept: ${welcome.pts}, reload ${welcome.reload}")
            again.state.first { !it.loading && town.id in it.muted }
            withContext(scope.coroutineContext) { again.socket.stop() }
        } finally {
            other.setMuted(town.id, false)
            withContext(scope.coroutineContext) { app.signOut() }
            other.logout()
        }
    }
}
