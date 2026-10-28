package io.github.ssenerg.gochat.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import gochat.v1.PushPlatform
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.PushService
import io.github.ssenerg.gochat.core.state.PushTarget
import io.github.ssenerg.gochat.core.state.PushToken
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.suspendCancellableCoroutine
import platform.UIKit.UIApplication
import platform.UIKit.registerForRemoteNotifications
import platform.UIKit.unregisterForRemoteNotifications
import platform.UserNotifications.UNAuthorizationOptionAlert
import platform.UserNotifications.UNAuthorizationOptionBadge
import platform.UserNotifications.UNAuthorizationOptionSound
import platform.UserNotifications.UNUserNotificationCenter
import kotlin.coroutines.resume

/**
 * APNs. The Swift side's app delegate hands it the device's token and the
 * pushes the person taps; use it from the main thread.
 */
object IosPush : PushService {
    private val _token = MutableStateFlow<PushToken?>(null)
    override val token: StateFlow<PushToken?> = _token.asStateFlow()

    /**
     * Whether someone is signed in: a push that comes after signing out,
     * before APNs forgot the token, is someone else's.
     */
    var wanted: Boolean = false
        private set

    /** A push tapped before the app was made, as at a cold start. */
    private var pending: PushTarget? = null

    internal var app: GoChatApp? = null
        set(value) {
            field = value
            pending?.let { t ->
                pending = null
                value?.openPush(t)
            }
        }

    override fun enable() {
        wanted = true
        UIApplication.sharedApplication.registerForRemoteNotifications()
    }

    override fun disable() {
        wanted = false
        _token.value = null
        UIApplication.sharedApplication.unregisterForRemoteNotifications()
    }

    /** APNs gave the device [token], in hexadecimal; [sandbox] when it is of Apple's sandbox. */
    fun registered(token: String, sandbox: Boolean) {
        _token.value = PushToken(PushPlatform.PUSH_PLATFORM_APNS, token, sandbox)
    }

    /** The person tapped a push, whose keys are these. */
    fun opened(channelId: String, rootId: String, teamId: String) {
        val data = mapOf("channel_id" to channelId, "root_id" to rootId, "team_id" to teamId)
        val t = PushTarget.from { data[it] } ?: return
        app?.openPush(t) ?: run { pending = t }
    }
}

@Composable
actual fun rememberNotificationAccess(): suspend () -> Boolean = remember {
    suspend {
        suspendCancellableCoroutine { c ->
            UNUserNotificationCenter.currentNotificationCenter().requestAuthorizationWithOptions(
                UNAuthorizationOptionAlert or UNAuthorizationOptionSound or UNAuthorizationOptionBadge,
            ) { granted, _ -> c.resume(granted) }
        }
    }
}
