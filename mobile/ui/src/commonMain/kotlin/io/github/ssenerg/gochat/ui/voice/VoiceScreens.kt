package io.github.ssenerg.gochat.ui.voice

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ScreenShare
import androidx.compose.material.icons.automirrored.filled.StopScreenShare
import androidx.compose.material.icons.filled.CallEnd
import androidx.compose.material.icons.filled.Fullscreen
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.Headset
import androidx.compose.material.icons.filled.HeadsetOff
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material.icons.filled.VideocamOff
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FilledTonalIconButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.IconButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import gochat.v1.Channel
import gochat.v1.VoiceState
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.moderatedBy
import io.github.ssenerg.gochat.core.model.title
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.voice.CallVideo
import io.github.ssenerg.gochat.core.voice.VideoSource
import io.github.ssenerg.gochat.core.voice.VoiceCallState
import io.github.ssenerg.gochat.ui.Avatar
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.EmptyState
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.VideoView
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import io.github.ssenerg.gochat.ui.main.Route
import io.github.ssenerg.gochat.ui.rememberCallAccess
import io.github.ssenerg.gochat.ui.rememberCameraAccess
import io.github.ssenerg.gochat.ui.rememberScreenShare
import kotlinx.coroutines.launch

/** Returns what joins a voice channel, once the microphone is allowed. */
@Composable
fun rememberJoinVoice(session: ChatSession): (Channel) -> Unit {
    val microphone = rememberCallAccess()
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    return remember(session, microphone) {
        { ch ->
            if (!session.voice.available) {
                toasts.show("Talking in voice channels comes to this app next; join from the web for now.")
            } else {
                scope.launch {
                    if (microphone()) session.voice.join(ch) else toasts.show("Allow GoChat to use the microphone to talk in voice channels.")
                }
            }
        }
    }
}

/** What the call says of itself. */
private fun VoiceCallState.status(): String = if (connected) "Voice connected" else "Connecting…"

/** A voice channel: who talks in it, their cameras and screens, and the call's controls. */
@Composable
fun VoiceRoomScreen(session: ChatSession, channelId: String) {
    val st by session.state.collectAsState()
    val call by session.voice.state.collectAsState()
    val allVideos by session.voice.videos.collectAsState()
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val join = rememberJoinVoice(session)
    val ch = st.channel(channelId)
    val here = call?.takeIf { it.channelId == channelId }
    val members = st.voice[channelId]?.states.orEmpty()
    val moderator = ch?.moderatedBy(st.me, st.team) == true
    // The call's cameras and screens show on its own channel only.
    val videos = if (here != null) allVideos else emptyList()
    val cameras = videos.filter { it.source == VideoSource.Camera }.associateBy { it.userId }
    val screens = videos.filter { it.source == VideoSource.Screen }.sortedBy { !it.local }
    // Whose screen shows full screen, while it is shared.
    var expanded by remember { mutableStateOf<String?>(null) }
    screens.firstOrNull { it.userId == expanded }?.let { FullScreen(it) { expanded = null } }
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(ch?.title() ?: here?.name ?: "Voice channel", maxLines = 1, overflow = TextOverflow.Ellipsis) },
                navigationIcon = { BackButton { nav.pop() } },
            )
        },
        bottomBar = {
            Surface(tonalElevation = 3.dp) {
                Box(Modifier.fillMaxWidth().navigationBarsPadding().padding(16.dp), contentAlignment = Alignment.Center) {
                    when {
                        here != null -> CallControls(session, here)
                        !session.voice.available -> Text(
                            "Talking in voice channels comes to this app next; join from the web for now.",
                            textAlign = TextAlign.Center,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        ch != null -> Button(onClick = { join(ch) }, modifier = Modifier.fillMaxWidth().height(48.dp)) {
                            Icon(Icons.Filled.Mic, contentDescription = null)
                            Spacer(Modifier.width(8.dp))
                            Text(if (call != null) "Move here" else "Join voice")
                        }
                    }
                }
            }
        },
    ) { padding ->
        if (members.isEmpty()) {
            EmptyState(if (here != null) "Connecting…" else "No one is here yet.", Modifier.padding(padding))
        } else {
            LazyVerticalGrid(
                GridCells.Adaptive(140.dp),
                Modifier.padding(padding).fillMaxSize(),
                contentPadding = PaddingValues(16.dp),
                horizontalArrangement = Arrangement.spacedBy(12.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                // Screens go first, the grid's full width, as the web shows them.
                items(screens, key = { "screen:" + it.userId }, span = { GridItemSpan(maxLineSpan) }) { video ->
                    val owner = members.firstOrNull { it.user?.id == video.userId }?.user
                    ScreenTile(video, if (video.local) "Your screen" else "${owner.displayName()}'s screen") { expanded = video.userId }
                }
                items(members.filter { it.user != null }.distinctBy { it.user?.id }, key = { it.user?.id.orEmpty() }) { vs ->
                    val id = vs.user?.id.orEmpty()
                    VoiceTile(
                        vs,
                        camera = cameras[id],
                        speaking = here != null && id in here.speaking,
                        onDisconnect = if (moderator && id != st.me.id) {
                            {
                                session.launch({ session.disconnectVoice(channelId, id) }) { err -> err?.let(toasts::show) }
                            }
                        } else {
                            null
                        },
                    )
                }
            }
        }
    }
}

@Composable
private fun VoiceTile(vs: VoiceState, camera: CallVideo?, speaking: Boolean, onDisconnect: (() -> Unit)?) {
    val colors = LocalChatColors.current
    var menu by remember { mutableStateOf(false) }
    Box {
        Column(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(16.dp))
                .background(MaterialTheme.colorScheme.surfaceContainerHigh)
                .then(if (camera != null && speaking) Modifier.border(3.dp, colors.online, RoundedCornerShape(16.dp)) else Modifier)
                .then(if (onDisconnect != null) Modifier.clickable { menu = true } else Modifier)
                .padding(vertical = if (camera != null) 0.dp else 20.dp, horizontal = if (camera != null) 0.dp else 12.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            if (camera != null) {
                // A camera takes the avatar's place, the person's own mirrored.
                VideoView(camera, Modifier.fillMaxWidth().aspectRatio(4f / 3f), fill = true)
            } else {
                // A ring around whoever talks, as on the web.
                Box(
                    Modifier.size(76.dp).border(3.dp, if (speaking) colors.online else Color.Transparent, CircleShape).padding(6.dp),
                    contentAlignment = Alignment.Center,
                ) {
                    Avatar(vs.user, size = 64.dp)
                }
            }
            Spacer(Modifier.height(10.dp))
            Row(Modifier.padding(horizontal = 12.dp).padding(bottom = if (camera != null) 10.dp else 0.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(vs.user.displayName(), fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                if (vs.self_stream) {
                    Spacer(Modifier.width(6.dp))
                    LiveBadge()
                }
                if (vs.self_video) {
                    Spacer(Modifier.width(4.dp))
                    Icon(Icons.Filled.Videocam, contentDescription = "Camera on", tint = colors.muted, modifier = Modifier.size(16.dp))
                }
                if (vs.self_deaf) {
                    Spacer(Modifier.width(4.dp))
                    Icon(Icons.Filled.HeadsetOff, contentDescription = "Deafened", tint = colors.muted, modifier = Modifier.size(16.dp))
                } else if (vs.self_mute) {
                    Spacer(Modifier.width(4.dp))
                    Icon(Icons.Filled.MicOff, contentDescription = "Muted", tint = colors.muted, modifier = Modifier.size(16.dp))
                }
            }
        }
        if (onDisconnect != null) {
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                DropdownMenuItem(text = { Text("Disconnect from voice") }, onClick = {
                    menu = false
                    onDisconnect()
                })
            }
        }
    }
}

/** Someone's screen, its whole picture, with a way to see it full screen. */
@Composable
private fun ScreenTile(video: CallVideo, name: String, onExpand: () -> Unit) {
    Box(Modifier.fillMaxWidth().aspectRatio(16f / 9f).clip(RoundedCornerShape(16.dp)).background(Color.Black)) {
        VideoView(video, Modifier.fillMaxSize(), fill = false)
        Text(
            name,
            color = Color.White,
            fontWeight = FontWeight.SemiBold,
            fontSize = 13.sp,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.align(Alignment.BottomStart).padding(10.dp)
                .background(Color.Black.copy(alpha = 0.55f), RoundedCornerShape(6.dp)).padding(horizontal = 8.dp, vertical = 2.dp),
        )
        IconButton(
            onClick = onExpand,
            modifier = Modifier.align(Alignment.TopEnd).padding(6.dp).background(Color.Black.copy(alpha = 0.45f), CircleShape),
        ) {
            Icon(Icons.Filled.Fullscreen, contentDescription = "Full screen", tint = Color.White)
        }
    }
}

/** A screen over everything else, until the person goes back. */
@Composable
private fun FullScreen(video: CallVideo, onClose: () -> Unit) {
    Dialog(onDismissRequest = onClose, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Box(Modifier.fillMaxSize().background(Color.Black).clickable(onClick = onClose)) {
            VideoView(video, Modifier.fillMaxSize(), fill = false)
        }
    }
}

/** Marks someone who shares their screen, as Discord does. */
@Composable
internal fun LiveBadge() {
    Text(
        "LIVE",
        color = Color.White,
        fontSize = 10.sp,
        fontWeight = FontWeight.Bold,
        letterSpacing = 0.5.sp,
        modifier = Modifier.background(MaterialTheme.colorScheme.error, RoundedCornerShape(4.dp)).padding(horizontal = 5.dp, vertical = 1.dp),
    )
}

/** Mute, deafen, the camera, the screen and leave, under the voice room. */
@Composable
private fun CallControls(session: ChatSession, call: VoiceCallState) {
    val colors = LocalChatColors.current
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    val muted by session.voice.muted.collectAsState()
    val deafened by session.voice.deafened.collectAsState()
    val cameraAccess = rememberCameraAccess()
    val screenShare = rememberScreenShare()
    Column(horizontalAlignment = Alignment.CenterHorizontally) {
        Text(call.status(), color = if (call.connected) colors.online else colors.away, fontWeight = FontWeight.SemiBold)
        Spacer(Modifier.height(12.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
            FilledTonalIconButton(onClick = { session.voice.toggleMute() }, modifier = Modifier.size(52.dp)) {
                Icon(if (muted || deafened) Icons.Filled.MicOff else Icons.Filled.Mic, contentDescription = if (muted || deafened) "Unmute" else "Mute")
            }
            FilledTonalIconButton(onClick = { session.voice.toggleDeafen() }, modifier = Modifier.size(52.dp)) {
                Icon(if (deafened) Icons.Filled.HeadsetOff else Icons.Filled.Headset, contentDescription = if (deafened) "Undeafen" else "Deafen")
            }
            if (session.voice.video) {
                VideoToggle(
                    on = call.camera,
                    icon = if (call.camera) Icons.Filled.VideocamOff else Icons.Filled.Videocam,
                    label = if (call.camera) "Turn the camera off" else "Turn the camera on",
                ) {
                    if (call.camera) {
                        session.voice.setVideo(VideoSource.Camera, false)
                    } else {
                        scope.launch {
                            if (cameraAccess()) session.voice.setVideo(VideoSource.Camera, true) else toasts.show("Allow GoChat to use the camera to turn it on.")
                        }
                    }
                }
                VideoToggle(
                    on = call.screen,
                    icon = if (call.screen) Icons.AutoMirrored.Filled.StopScreenShare else Icons.AutoMirrored.Filled.ScreenShare,
                    label = if (call.screen) "Stop sharing the screen" else "Share the screen",
                ) {
                    if (call.screen) {
                        session.voice.setVideo(VideoSource.Screen, false)
                    } else {
                        scope.launch { if (screenShare()) session.voice.setVideo(VideoSource.Screen, true) }
                    }
                }
            }
            FilledIconButton(
                onClick = { session.voice.leave() },
                modifier = Modifier.size(52.dp),
                colors = IconButtonDefaults.filledIconButtonColors(
                    containerColor = MaterialTheme.colorScheme.error,
                    contentColor = MaterialTheme.colorScheme.onError,
                ),
            ) {
                Icon(Icons.Filled.CallEnd, contentDescription = "Leave voice")
            }
        }
    }
}

/** A camera or screen switch, filled while it sends. */
@Composable
private fun VideoToggle(on: Boolean, icon: ImageVector, label: String, onClick: () -> Unit) {
    if (on) {
        FilledIconButton(onClick = onClick, modifier = Modifier.size(52.dp)) { Icon(icon, contentDescription = label) }
    } else {
        FilledTonalIconButton(onClick = onClick, modifier = Modifier.size(52.dp)) { Icon(icon, contentDescription = label) }
    }
}

/** The call, at the foot of the sidebar wherever the person is, as in Discord. */
@Composable
fun VoicePanel(session: ChatSession) {
    val call = session.voice.state.collectAsState().value ?: return
    val muted by session.voice.muted.collectAsState()
    val deafened by session.voice.deafened.collectAsState()
    val colors = LocalChatColors.current
    val nav = LocalNavigator.current
    val tint = if (call.connected) colors.online else colors.away
    Row(
        Modifier.fillMaxWidth().background(colors.rail.copy(alpha = 0.6f))
            .clickable { nav.open(Route.Voice(call.channelId)) }
            .padding(start = 12.dp, end = 4.dp, top = 4.dp, bottom = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(Icons.Filled.GraphicEq, contentDescription = null, tint = tint, modifier = Modifier.size(22.dp))
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f)) {
            Text(call.status(), color = tint, fontWeight = FontWeight.SemiBold, fontSize = 13.sp)
            Text(call.name, color = colors.sidebarMuted, fontSize = 12.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        IconButton(onClick = { session.voice.toggleMute() }) {
            Icon(if (muted || deafened) Icons.Filled.MicOff else Icons.Filled.Mic, contentDescription = if (muted || deafened) "Unmute" else "Mute", tint = colors.sidebarText)
        }
        IconButton(onClick = { session.voice.toggleDeafen() }) {
            Icon(if (deafened) Icons.Filled.HeadsetOff else Icons.Filled.Headset, contentDescription = if (deafened) "Undeafen" else "Deafen", tint = colors.sidebarText)
        }
        IconButton(onClick = { session.voice.leave() }) {
            Icon(Icons.Filled.CallEnd, contentDescription = "Leave voice", tint = MaterialTheme.colorScheme.error)
        }
    }
}
