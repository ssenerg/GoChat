package io.github.ssenerg.gochat.core.state

import io.github.ssenerg.gochat.core.api.ApiException
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Sessions signed out of while the server could not be told. Each ends once
 * the server answers, which forgets its device too: until then the server
 * would push the account's messages to the phone. They wait in the store,
 * so the next launch goes on with them. Use it from one thread, the main one.
 */
internal class SignOuts(
    private val store: KeyValueStore,
    private val scope: CoroutineScope,
    private val logout: suspend (server: String, token: String) -> Unit,
) {
    private var retrying: Job? = null

    /** Ends the session of [token] at [server] once the server answers. */
    fun add(server: String, token: String) {
        save((pending() - (server to token) + (server to token)).takeLast(MAX))
        retry()
    }

    /** Tries those waiting now, and again later until each is settled. */
    fun retry() {
        if (retrying?.isActive == true || pending().isEmpty()) return
        retrying = scope.launch {
            var wait = FIRST_WAIT_MS
            while (true) {
                for (p in pending()) {
                    if (settled { logout(p.first, p.second) }) save(pending() - p)
                }
                if (pending().isEmpty()) return@launch
                delay(wait)
                wait = minOf(wait * 2, LAST_WAIT_MS)
            }
        }
    }

    private fun pending(): List<Pair<String, String>> =
        store.get(KEY)?.lines().orEmpty().mapNotNull { line ->
            line.split('\t').takeIf { it.size == 2 }?.let { it[0] to it[1] }
        }

    private fun save(all: List<Pair<String, String>>) {
        store.put(KEY, all.takeIf { it.isNotEmpty() }?.joinToString("\n") { "${it.first}\t${it.second}" })
    }

    companion object {
        private const val KEY = "signing_out"
        private const val MAX = 4
        private const val FIRST_WAIT_MS = 5_000L
        private const val LAST_WAIT_MS = 5 * 60_000L

        /**
         * Whether [call] settled the session: it ended, it was gone already,
         * or the server answered that it will not end it. A server that
         * cannot be reached, or is busy, may later.
         */
        suspend fun settled(call: suspend () -> Unit): Boolean = try {
            call()
            true
        } catch (e: CancellationException) {
            throw e
        } catch (e: ApiException) {
            e.code != ApiException.UNAVAILABLE && e.code != ApiException.RESOURCE_EXHAUSTED
        } catch (_: Exception) {
            false
        }
    }
}
