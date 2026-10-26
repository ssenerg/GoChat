package io.github.ssenerg.gochat.ui.main

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SnackbarDuration
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.SnackbarResult
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.VerticalDivider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import io.github.ssenerg.gochat.core.state.BotAnswer
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.ui.EmptyState
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.PlatformBackHandler
import io.github.ssenerg.gochat.ui.channel.ChannelScreen
import io.github.ssenerg.gochat.ui.lists.PinsScreen
import io.github.ssenerg.gochat.ui.lists.SavedScreen
import io.github.ssenerg.gochat.ui.lists.SearchScreen
import io.github.ssenerg.gochat.ui.lists.ThreadsScreen
import io.github.ssenerg.gochat.ui.people.BrowseChannelsScreen
import io.github.ssenerg.gochat.ui.people.ChannelInfoScreen
import io.github.ssenerg.gochat.ui.people.NewChannelScreen
import io.github.ssenerg.gochat.ui.people.NewConversationScreen
import io.github.ssenerg.gochat.ui.settings.SettingsScreen
import io.github.ssenerg.gochat.ui.thread.ThreadScreen
import io.github.ssenerg.gochat.ui.voice.VoiceRoomScreen
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch

/** Where in the app the person is. */
sealed interface Route {
    data object Home : Route

    data class Channel(val id: String) : Route

    data class Thread(val channelId: String, val rootId: String) : Route

    data object Threads : Route

    data object Saved : Route

    data class Pins(val channelId: String) : Route

    data object Search : Route

    data object NewConversation : Route

    data object Browse : Route

    data object NewChannel : Route

    data class ChannelInfo(val id: String) : Route

    data class Voice(val id: String) : Route

    data object Settings : Route
}

/** The screens open, the sidebar at the bottom. */
class Navigator {
    val stack = mutableStateListOf<Route>(Route.Home)

    val top: Route get() = stack.last()

    val canPop: Boolean get() = stack.size > 1

    fun open(route: Route) {
        if (stack.last() != route) stack.add(route)
    }

    /** Opens a channel from the sidebar: the screens before it close. */
    fun openChannel(id: String) {
        stack.clear()
        stack.add(Route.Home)
        stack.add(Route.Channel(id))
    }

    fun pop() {
        if (stack.size > 1) stack.removeAt(stack.lastIndex)
    }

    /** Closes every screen showing [channelId], when the person leaves it. */
    fun closeChannel(channelId: String) {
        stack.removeAll { (it is Route.Channel && it.id == channelId) || (it is Route.Thread && it.channelId == channelId) || (it is Route.ChannelInfo && it.id == channelId) }
        if (stack.isEmpty()) stack.add(Route.Home)
    }
}

/** Short messages at the bottom of the screen. */
class Toasts(private val host: SnackbarHostState, private val scope: CoroutineScope) {
    fun show(text: String) {
        scope.launch { host.showSnackbar(text, duration = SnackbarDuration.Short) }
    }
}

val LocalNavigator = staticCompositionLocalOf<Navigator> { error("no navigator") }
val LocalToasts = staticCompositionLocalOf<Toasts> { error("no toasts") }

@Composable
fun MainScreen(app: GoChatApp, session: ChatSession) {
    val nav = remember(session) { Navigator() }
    val snackbar = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()
    val toasts = remember(snackbar) { Toasts(snackbar, scope) }

    // Messages elsewhere that call for the person.
    LaunchedEffect(session) {
        session.notices.collect { n ->
            val result = snackbar.showSnackbar("${n.title}: ${n.body}", actionLabel = "Open", duration = SnackbarDuration.Short)
            if (result == SnackbarResult.ActionPerformed) {
                if (n.rootId.isNotEmpty()) nav.open(Route.Thread(n.channelId, n.rootId)) else nav.openChannel(n.channelId)
            }
        }
    }

    // Why a call ended or could not start.
    LaunchedEffect(session) {
        session.voice.errors.collect { toasts.show(it) }
    }

    // A bot's answer to a button pressed here: a passing notice, or an alert.
    var alert by remember(session) { mutableStateOf<BotAnswer?>(null) }
    LaunchedEffect(session) {
        session.answers.collect { a -> if (a.alert) alert = a else toasts.show("${a.bot}: ${a.text}") }
    }
    alert?.let { a ->
        AlertDialog(
            onDismissRequest = { alert = null },
            title = { Text(a.bot) },
            text = { Text(a.text) },
            confirmButton = { TextButton(onClick = { alert = null }) { Text("OK") } },
        )
    }

    // What is on screen is what gets read; in the background nothing is.
    val top = nav.top
    val foreground by LocalPlatform.current.foreground.collectAsState()
    LaunchedEffect(foreground) { session.setForeground(foreground) }
    LaunchedEffect(top, foreground) {
        when {
            !foreground -> session.setVisible(null)
            top is Route.Channel -> session.setVisible(top.id)
            top is Route.Thread -> session.setVisible(top.channelId, top.rootId)
            else -> session.setVisible(null)
        }
    }

    PlatformBackHandler(nav.canPop) { nav.pop() }

    CompositionLocalProvider(LocalNavigator provides nav, LocalToasts provides toasts) {
        BoxWithConstraints(Modifier.fillMaxSize()) {
            if (maxWidth >= 720.dp) {
                Row(Modifier.fillMaxSize()) {
                    Sidebar(session, app, Modifier.width(320.dp).fillMaxHeight())
                    VerticalDivider()
                    Box(Modifier.weight(1f).fillMaxHeight().background(MaterialTheme.colorScheme.background)) {
                        if (top == Route.Home) EmptyState("Pick a channel or a conversation.") else key(top) { Screen(top, session, app) }
                    }
                }
            } else if (top == Route.Home) {
                Sidebar(session, app, Modifier.fillMaxSize())
            } else {
                // Each screen keeps its own state: a draft never moves to
                // another conversation.
                key(top) { Screen(top, session, app) }
            }
            SnackbarHost(snackbar, Modifier.align(Alignment.BottomCenter).navigationBarsPadding().padding(bottom = 72.dp))
        }
    }
}

@Composable
private fun Screen(route: Route, session: ChatSession, app: GoChatApp) {
    when (route) {
        Route.Home -> {}
        is Route.Channel -> ChannelScreen(session, route.id)
        is Route.Thread -> ThreadScreen(session, route.channelId, route.rootId)
        Route.Threads -> ThreadsScreen(session)
        Route.Saved -> SavedScreen(session)
        is Route.Pins -> PinsScreen(session, route.channelId)
        Route.Search -> SearchScreen(session)
        Route.NewConversation -> NewConversationScreen(session)
        Route.Browse -> BrowseChannelsScreen(session)
        Route.NewChannel -> NewChannelScreen(session)
        is Route.ChannelInfo -> ChannelInfoScreen(session, route.id)
        is Route.Voice -> VoiceRoomScreen(session, route.id)
        Route.Settings -> SettingsScreen(session, app)
    }
}
