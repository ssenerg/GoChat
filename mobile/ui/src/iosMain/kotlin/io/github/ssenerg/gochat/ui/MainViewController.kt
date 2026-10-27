package io.github.ssenerg.gochat.ui

import androidx.compose.ui.window.ComposeUIViewController
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.voice.VideoSource
import kotlinx.coroutines.MainScope
import kotlinx.coroutines.launch
import platform.UIKit.UIViewController

private val scope = MainScope()

/** The app and its platform, made once for the process, as Android's Application does. */
private var root: Pair<GoChatApp, IosPlatform>? = null

/**
 * The app's screen, for the Swift side to show, with the WebRTC it supplies
 * for voice channels.
 */
fun MainViewController(rtc: IosRtcEngine?): UIViewController {
    val (app, platform) = root ?: run {
        val platform = IosPlatform(scope, rtc?.let(::SwiftRtc))
        val app = GoChatApp(platform.store, scope, rtc = platform.rtc, app = platform.app).also { it.start() }
        // A broadcast the person started, from the app or the Control
        // Center, shares the screen in the call.
        rtc?.setScreenEvents(
            object : IosScreenEvents {
                override fun started() {
                    scope.launch { (app.state.value as? AppState.SignedIn)?.session?.voice?.setVideo(VideoSource.Screen, true) }
                }
            },
        )
        (app to platform).also { root = it }
    }
    return ComposeUIViewController { GoChatRoot(app, platform) }
}
