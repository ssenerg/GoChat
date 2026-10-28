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
import com.google.firebase.FirebaseApp
import com.google.firebase.messaging.FirebaseMessaging
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import gochat.v1.PushPlatform
import io.github.ssenerg.gochat.core.state.PushService
import io.github.ssenerg.gochat.core.state.PushTarget
import io.github.ssenerg.gochat.core.state.PushToken
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * Firebase Cloud Messaging. Firebase starts from the app's resources
 * (google_app_id and the others), which a build gets from the Firebase
 * project's settings; without them the app gets no pushes.
 */
class AndroidPush(private val context: Context) : PushService {
    private val _token = MutableStateFlow<PushToken?>(null)
    override val token: StateFlow<PushToken?> = _token.asStateFlow()

    private val on: Boolean get() = FirebaseApp.getApps(context).isNotEmpty()

    init {
        current = this
        // The channel the server's pushes name.
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

    internal fun tokenChanged(token: String) {
        _token.value = PushToken(PushPlatform.PUSH_PLATFORM_FCM, token)
    }

    /** Shows a push that came while the app ran: the system shows the others. */
    internal fun show(title: String, body: String, data: Map<String, String>) {
        if (Build.VERSION.SDK_INT >= 33 && context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val open = context.packageManager.getLaunchIntentForPackage(context.packageName) ?: return
        data.forEach { (k, v) -> open.putExtra(k, v) }
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

        /** What the push an activity was opened from shows, if it was. */
        fun target(intent: Intent?): PushTarget? {
            val extras = intent?.extras ?: return null
            return PushTarget.from { extras.getString(it) }
        }
    }
}

/** Hears of FCM's tokens, and of pushes that come while the app runs. */
class GoChatMessagingService : FirebaseMessagingService() {
    override fun onNewToken(token: String) {
        AndroidPush.current?.tokenChanged(token)
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val n = message.notification ?: return
        AndroidPush.current?.show(n.title ?: "GoChat", n.body.orEmpty(), message.data)
    }
}
