package io.github.ssenerg.gochat.core.state

import gochat.v1.PushPlatform
import kotlinx.coroutines.flow.StateFlow

/** The device's token at its platform's push service. */
data class PushToken(val platform: PushPlatform, val token: String, val sandbox: Boolean = false)

/**
 * The platform's push service, FCM on Android and APNs on iOS, which
 * reaches the app while it is closed.
 */
interface PushService {
    /** The device's token, null until there is one; it may change. */
    val token: StateFlow<PushToken?>

    /** Asks the push service for a token, once someone signed in. */
    fun enable()

    /** Drops the token, so a device signed out hears no more. */
    fun disable()
}

/** What a push the person tapped opens: a channel, or a thread in it. */
data class PushTarget(val channelId: String, val rootId: String = "", val teamId: String = "") {
    companion object {
        /** Reads a push's data, as the server sends it; null for a push of something else. */
        fun from(get: (String) -> String?): PushTarget? {
            val channel = get("channel_id")?.takeIf { it.isNotBlank() } ?: return null
            return PushTarget(channel, get("root_id").orEmpty(), get("team_id").orEmpty())
        }

        /**
         * Reads a tapped notice the app made itself: its [TAP] extra holds
         * [secret]. Other apps may start the app with a channel's keys, but
         * not with the secret.
         */
        fun tapped(secret: String, get: (String) -> String?): PushTarget? =
            if (secret.isNotEmpty() && get(TAP) == secret) from(get) else null

        /** The extra of a notice that holds the app's secret. */
        const val TAP = "gochat_tap"
    }
}
