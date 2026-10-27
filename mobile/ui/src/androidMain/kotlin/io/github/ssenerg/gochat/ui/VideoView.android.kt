package io.github.ssenerg.gochat.ui

import android.content.Context
import android.graphics.SurfaceTexture
import android.view.TextureView
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.viewinterop.AndroidView
import io.github.ssenerg.gochat.core.voice.CallVideo
import io.github.ssenerg.gochat.core.voice.VideoSource
import org.webrtc.EglBase
import org.webrtc.EglRenderer
import org.webrtc.GlRectDrawer
import org.webrtc.VideoFrame
import org.webrtc.VideoSink
import org.webrtc.VideoTrack
import java.util.concurrent.CountDownLatch

@Composable
actual fun VideoView(video: CallVideo, modifier: Modifier, fill: Boolean) {
    val engine = LocalPlatform.current.rtc as? AndroidRtcEngine
    val track = video.track as? VideoTrack
    Box(modifier.background(Color.Black), contentAlignment = Alignment.Center) {
        if (engine == null || track == null) return@Box
        key(track) {
            val context = LocalContext.current
            // The picture's width over its height, once a frame came.
            var aspect by remember { mutableFloatStateOf(0f) }
            val view = remember { VideoTexture(context, engine.egl.eglBaseContext) }
            DisposableEffect(view) {
                view.onAspect = { aspect = it }
                // A track whose call ended is disposed: it has nothing to draw.
                runCatching { track.addSink(view) }
                onDispose {
                    runCatching { track.removeSink(view) }
                    view.release()
                }
            }
            val mirror = video.local && video.source == VideoSource.Camera
            AndroidView(
                factory = { view },
                update = { it.setMirror(mirror) },
                // Filling, the view takes the space and crops the picture to
                // it; fitting, the view takes the picture's shape inside it.
                modifier = if (fill || aspect == 0f) Modifier.fillMaxSize() else Modifier.aspectRatio(aspect),
            )
        }
    }
}

/**
 * Draws a WebRTC video track on a TextureView, which clips, scrolls and
 * rounds its corners as any view does, unlike WebRTC's own SurfaceView.
 */
private class VideoTexture(context: Context, egl: EglBase.Context) :
    TextureView(context),
    TextureView.SurfaceTextureListener,
    VideoSink {
    private val renderer = EglRenderer("video")
    private var aspect = 0f

    /** Hears the picture's width over its height when it changes, on the main thread. */
    var onAspect: ((Float) -> Unit)? = null

    init {
        renderer.init(egl, EglBase.CONFIG_PLAIN, GlRectDrawer())
        surfaceTextureListener = this
    }

    fun setMirror(mirror: Boolean) = renderer.setMirror(mirror)

    fun release() = renderer.release()

    override fun onFrame(frame: VideoFrame) {
        val a = frame.rotatedWidth.toFloat() / frame.rotatedHeight
        if (a != aspect) {
            aspect = a
            post { onAspect?.invoke(a) }
        }
        renderer.onFrame(frame)
    }

    override fun onSizeChanged(w: Int, h: Int, oldw: Int, oldh: Int) {
        super.onSizeChanged(w, h, oldw, oldh)
        // The picture is cropped to the view's shape, never stretched.
        if (w > 0 && h > 0) renderer.setLayoutAspectRatio(w.toFloat() / h)
    }

    override fun onSurfaceTextureAvailable(surface: SurfaceTexture, width: Int, height: Int) {
        renderer.createEglSurface(surface)
    }

    override fun onSurfaceTextureSizeChanged(surface: SurfaceTexture, width: Int, height: Int) {}

    override fun onSurfaceTextureDestroyed(surface: SurfaceTexture): Boolean {
        // The surface goes once the renderer let go of it, as WebRTC's own
        // views wait.
        val released = CountDownLatch(1)
        renderer.releaseEglSurface(released::countDown)
        released.await()
        return true
    }

    override fun onSurfaceTextureUpdated(surface: SurfaceTexture) {}
}
