package io.github.ssenerg.gochat.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.key
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.viewinterop.UIKitInteropProperties
import androidx.compose.ui.viewinterop.UIKitView
import io.github.ssenerg.gochat.core.voice.CallVideo
import io.github.ssenerg.gochat.core.voice.VideoSource
import kotlinx.cinterop.ExperimentalForeignApi

@OptIn(ExperimentalForeignApi::class)
@Composable
actual fun VideoView(video: CallVideo, modifier: Modifier, fill: Boolean) {
    val engine = (LocalPlatform.current.rtc as? SwiftRtc)?.native
    Box(modifier.background(Color.Black)) {
        if (engine == null) return@Box
        val mirror = video.local && video.source == VideoSource.Camera
        key(video.track, fill) {
            // WebRTC's Metal view, which Swift makes and feeds; it takes no
            // touches, so the screens' buttons over it work.
            UIKitView(
                factory = { engine.videoView(video.track, mirror, fill) },
                modifier = Modifier.fillMaxSize(),
                onRelease = { engine.releaseVideoView(it, video.track) },
                properties = UIKitInteropProperties(interactionMode = null),
            )
        }
    }
}
