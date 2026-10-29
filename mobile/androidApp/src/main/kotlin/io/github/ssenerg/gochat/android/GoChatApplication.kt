package io.github.ssenerg.gochat.android

import android.app.Application
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.ui.AndroidPlatform
import io.github.ssenerg.gochat.ui.AndroidPush
import io.github.ssenerg.gochat.ui.VoiceCallService
import kotlinx.coroutines.MainScope

/** Holds the app's state for as long as the process lives, across activities. */
class GoChatApplication : Application() {
    private val scope = MainScope()
    lateinit var platform: AndroidPlatform
        private set
    lateinit var app: GoChatApp
        private set

    override fun onCreate() {
        super.onCreate()
        platform = AndroidPlatform(this, scope)
        val push = AndroidPush(this)
        app = GoChatApp(platform.store, scope, rtc = platform.rtc, app = platform.app, push = push)
        push.app = app
        app.start()
        VoiceCallService.follow(this, app, scope)
    }
}
