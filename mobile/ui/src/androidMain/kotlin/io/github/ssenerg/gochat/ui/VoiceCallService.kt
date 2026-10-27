package io.github.ssenerg.gochat.ui

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.os.Build
import android.os.IBinder
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.ChatSession
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import kotlin.time.Duration.Companion.seconds

/**
 * Keeps a voice call going while the app is in the background, with the
 * screen off or another app open: Android lets only a foreground service,
 * which shows a notice, use the microphone then, and the camera and the
 * screen while they go out.
 */
class VoiceCallService : Service() {
    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_LEAVE) {
            onLeave?.invoke()
            return START_NOT_STICKY
        }
        val name = intent?.getStringExtra(EXTRA_NAME).orEmpty()
        val camera = intent?.getBooleanExtra(EXTRA_CAMERA, false) == true
        val screen = intent?.getBooleanExtra(EXTRA_SCREEN, false) == true
        val notices = getSystemService(NotificationManager::class.java)
        notices.createNotificationChannel(
            NotificationChannel(NOTICE_CHANNEL, "Voice channels", NotificationManager.IMPORTANCE_LOW).apply { setShowBadge(false) },
        )
        val open = packageManager.getLaunchIntentForPackage(packageName)?.let {
            PendingIntent.getActivity(this, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        }
        val leave = PendingIntent.getService(
            this, 1, Intent(this, VoiceCallService::class.java).setAction(ACTION_LEAVE), PendingIntent.FLAG_IMMUTABLE,
        )
        val notice = Notification.Builder(this, NOTICE_CHANNEL)
            .setSmallIcon(R.drawable.gochat_voice)
            .setContentTitle(if (name.isEmpty()) "In a voice channel" else "In $name")
            .setContentText("Talking in GoChat")
            .setContentIntent(open)
            .setOngoing(true)
            .setCategory(Notification.CATEGORY_CALL)
            .addAction(Notification.Action.Builder(null as Icon?, "Leave", leave).build())
            .build()
        if (Build.VERSION.SDK_INT >= 29) {
            var types = ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
            if (camera && Build.VERSION.SDK_INT >= 30 && checkSelfPermission(Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) {
                types = types or ServiceInfo.FOREGROUND_SERVICE_TYPE_CAMERA
            }
            if (screen) types = types or ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION
            try {
                startForeground(NOTICE_ID, notice, types)
            } catch (_: Exception) {
                // Android refuses the camera or the screen from the
                // background: the voice goes on without them.
                types = ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
                startForeground(NOTICE_ID, notice, types)
            }
            shown.value = types
        } else {
            startForeground(NOTICE_ID, notice)
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        shown.value = 0
        super.onDestroy()
    }

    /** What the service runs for: the call's name, and whether its camera and screen go out. */
    private data class Call(val session: ChatSession, val name: String, val camera: Boolean, val screen: Boolean)

    companion object {
        private const val NOTICE_CHANNEL = "voice"
        private const val NOTICE_ID = 1
        private const val EXTRA_NAME = "name"
        private const val EXTRA_CAMERA = "camera"
        private const val EXTRA_SCREEN = "screen"
        private const val ACTION_LEAVE = "io.github.ssenerg.gochat.LEAVE_VOICE"

        /** Leaves the call the service keeps, from its notice. */
        @Volatile
        private var onLeave: (() -> Unit)? = null

        /** The call the service runs for now. */
        @Volatile
        private var current: Call? = null

        /** The foreground service types Android took, once it took them; 0 while it does not run. */
        private val shown = MutableStateFlow(0)

        private fun intent(context: Context, call: Call, screen: Boolean = call.screen) =
            Intent(context, VoiceCallService::class.java)
                .putExtra(EXTRA_NAME, call.name)
                .putExtra(EXTRA_CAMERA, call.camera)
                .putExtra(EXTRA_SCREEN, screen)

        /**
         * Runs the service for the screen too, before a capture starts, as
         * Android asks from version 14: the person consented already. Says
         * whether it does.
         */
        suspend fun allowScreen(context: Context): Boolean {
            if (Build.VERSION.SDK_INT < 29) return true
            val call = current ?: return false
            val ctx = context.applicationContext
            if (runCatching { ctx.startForegroundService(intent(ctx, call, screen = true)) }.isFailure) return false
            return withTimeoutOrNull(5.seconds) { shown.first { it and ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION != 0 } } != null
        }

        /** Runs the service for as long as [app]'s person talks in a voice channel. */
        @OptIn(ExperimentalCoroutinesApi::class)
        fun follow(context: Context, app: GoChatApp, scope: CoroutineScope) {
            val ctx = context.applicationContext
            val service = Intent(ctx, VoiceCallService::class.java)
            scope.launch {
                app.state
                    .flatMapLatest { st ->
                        val session = (st as? AppState.SignedIn)?.session ?: return@flatMapLatest flowOf(null)
                        session.voice.state.map { call -> call?.let { Call(session, it.name, it.camera, it.screen) } }
                    }
                    .distinctUntilChanged()
                    .collect { call ->
                        current = call
                        if (call == null) {
                            onLeave = null
                            ctx.stopService(service)
                            return@collect
                        }
                        onLeave = { call.session.voice.leave() }
                        // A call starts once the microphone is allowed, which a
                        // microphone service needs; starting it from the
                        // background, Android refuses, and the call goes on.
                        if (ctx.checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) {
                            runCatching { ctx.startForegroundService(intent(ctx, call)) }
                        }
                    }
            }
        }
    }
}
