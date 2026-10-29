package io.github.ssenerg.gochat.ui

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.util.Base64
import com.google.firebase.FirebaseApp
import com.google.firebase.messaging.FirebaseMessaging
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import gochat.v1.PushPlatform
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.PushService
import io.github.ssenerg.gochat.core.state.PushTarget
import io.github.ssenerg.gochat.core.state.PushToken
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.runBlocking
import java.security.SecureRandom

/**
 * Firebase Cloud Messaging. Firebase starts from the app's resources
 * (google_app_id and the others), which a build gets from the Firebase
 * project's settings; without them the app gets no pushes. The server
 * sends data alone, so the app shows each push itself, open or closed.
 */
class AndroidPush(private val context: Context) : PushService {
    private val _token = MutableStateFlow<PushToken?>(null)
    override val token: StateFlow<PushToken?> = _token.asStateFlow()

    private val on: Boolean get() = FirebaseApp.getApps(context).isNotEmpty()

    /** The app, which says whether a push is to show. */
    var app: GoChatApp? = null

    init {
        current = this
        // The channel of messages' notices.
        context.getSystemService(NotificationManager::class.java)
            .createNotificationChannel(NotificationChannel(CHANNEL, "Messages", NotificationManager.IMPORTANCE_HIGH))
    }

    override fun enable() {
        if (!on) return
        val messaging = FirebaseMessaging.getInstance()
        messaging.isAutoInitEnabled = true
        messaging.token.addOnSuccessListener { tokenChanged(it) }
    }

    override fun disable() {
        _token.value = null
        if (!on) return
        val messaging = FirebaseMessaging.getInstance()
        messaging.isAutoInitEnabled = false
        messaging.deleteToken()
    }

    /**
     * Shows a push, unless no one is signed in: then FCM is asked again to
     * drop the token, which fails while signing out offline.
     */
    internal fun received(title: String, body: String, data: Map<String, String>) {
        if (app?.pushReceived() == true) {
            show(title, body, data)
        } else if (on) {
            FirebaseMessaging.getInstance().deleteToken()
        }
    }

    internal fun tokenChanged(token: String) {
        _token.value = PushToken(PushPlatform.PUSH_PLATFORM_FCM, token)
    }

    private fun show(title: String, body: String, data: Map<String, String>) {
        if (Build.VERSION.SDK_INT >= 33 && context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val open = context.packageManager.getLaunchIntentForPackage(context.packageName) ?: return
        data.forEach { (k, v) -> open.putExtra(k, v) }
        open.putExtra(PushTarget.TAP, tapSecret(context))
        val id = (data["message_id"] ?: (title + body)).hashCode()
        val tap = PendingIntent.getActivity(context, id, open, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val notice = Notification.Builder(context, CHANNEL)
            .setSmallIcon(R.drawable.gochat_message)
            .setContentTitle(title)
            .setContentText(body)
            .setStyle(Notification.BigTextStyle().bigText(body))
            .setCategory(Notification.CATEGORY_MESSAGE)
            .setContentIntent(tap)
            .setAutoCancel(true)
            .build()
        context.getSystemService(NotificationManager::class.java).notify(data["channel_id"], id, notice)
    }

    companion object {
        private const val CHANNEL = "messages"

        internal var current: AndroidPush? = null

        private const val PREFS = "gochat_push"
        private const val TAP_SECRET = "tap_secret"

        /** What the push an activity was opened from shows, if the app made its notice. */
        fun target(context: Context, intent: Intent?): PushTarget? {
            val extras = intent?.extras ?: return null
            return PushTarget.tapped(tapSecret(context)) { extras.getString(it) }
        }

        // The install's secret, which its notices carry: MainActivity is
        // exported, so any app may start it with a channel's keys.
        private fun tapSecret(context: Context): String {
            val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            prefs.getString(TAP_SECRET, null)?.let { return it }
            val secret = Base64.encodeToString(ByteArray(16).also { SecureRandom().nextBytes(it) }, Base64.NO_WRAP)
            prefs.edit().putString(TAP_SECRET, secret).apply()
            return secret
        }
    }
}

/**
 * Hears of FCM's tokens, and of pushes: FCM starts the app's process for
 * one, if it is not running, and GoChatApplication makes the app first.
 */
class GoChatMessagingService : FirebaseMessagingService() {
    override fun onNewToken(token: String) {
        AndroidPush.current?.tokenChanged(token)
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val push = AndroidPush.current ?: return
        val data = message.data
        // A server from before sends a notification, which the system
        // shows while the app is closed.
        val title = data["title"] ?: message.notification?.title ?: "GoChat"
        val body = data["body"] ?: message.notification?.body.orEmpty()
        // FCM calls on a thread of its own; the app keeps to the main one.
        runBlocking(Dispatchers.Main.immediate) { push.received(title, body, data - "title" - "body") }
    }
}
