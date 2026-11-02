package io.github.ssenerg.gochat.ui.settings

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.SSOIdentity
import gochat.v1.SSOProvider
import gochat.v1.SessionInfo
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.about
import io.github.ssenerg.gochat.core.model.address
import io.github.ssenerg.gochat.core.model.canUnlink
import io.github.ssenerg.gochat.core.model.deviceName
import io.github.ssenerg.gochat.core.model.seen
import io.github.ssenerg.gochat.core.model.signedIn
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.SignIns
import io.github.ssenerg.gochat.core.state.SsoLink
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import kotlinx.coroutines.launch

@Composable
private fun Screen(title: String, content: @Composable (Modifier) -> Unit) {
    val nav = LocalNavigator.current
    Scaffold(topBar = { TopAppBar(title = { Text(title) }, navigationIcon = { BackButton { nav.pop() } }) }) { padding ->
        content(Modifier.padding(padding).fillMaxSize())
    }
}

@Composable
private fun Intro(text: String) {
    Text(text, color = LocalChatColors.current.muted, fontSize = 13.sp, modifier = Modifier.padding(16.dp))
}

/** A small label beside a name, as the web's badges. */
@Composable
private fun Tag(text: String) {
    Box(
        Modifier.clip(RoundedCornerShape(6.dp)).background(MaterialTheme.colorScheme.primaryContainer).padding(horizontal = 6.dp, vertical = 1.dp),
    ) {
        Text(text, color = MaterialTheme.colorScheme.onPrimaryContainer, fontSize = 11.sp, fontWeight = FontWeight.SemiBold)
    }
}

@Composable
private fun Confirm(title: String, action: String, onConfirm: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        confirmButton = { TextButton(onClick = onConfirm) { Text(action, color = MaterialTheme.colorScheme.error) } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

/** Settings → Sessions: where the person is signed in, to sign out of one or every other. */
@Composable
fun SessionsScreen(session: ChatSession) {
    val toasts = LocalToasts.current
    var list by remember { mutableStateOf<List<SessionInfo>?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableStateOf(0) }
    var revoke by remember { mutableStateOf<SessionInfo?>(null) }
    var revokeOthers by remember { mutableStateOf(false) }
    LaunchedEffect(reload) {
        try {
            list = session.sessions()
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    fun run(done: String, block: suspend () -> String?) {
        session.launch(block) { err ->
            toasts.show(err ?: done)
            reload++
        }
    }
    Screen("Sessions") { modifier ->
        val shown = list
        when {
            error != null && shown == null -> ErrorState(error ?: "", onRetry = { reload++ }, modifier = modifier)
            shown == null -> Loading(modifier)
            else -> LazyColumn(modifier) {
                item { Intro("Where you are signed in. Sign out any you do not recognize, then change your password.") }
                val now = Format.nowMillis()
                items(shown, key = { it.id }) { s ->
                    ListItem(
                        headlineContent = {
                            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                                Text(deviceName(s.user_agent), modifier = Modifier.weight(1f, fill = false))
                                if (s.current) Tag("This device")
                            }
                        },
                        supportingContent = {
                            Column {
                                Text(listOf(s.address, s.seen(now)).filter { it.isNotEmpty() }.joinToString(" · "))
                                s.signedIn(now).takeIf { it.isNotEmpty() }?.let { Text(it, fontSize = 12.sp) }
                            }
                        },
                        trailingContent = if (s.current) null else {
                            { TextButton(onClick = { revoke = s }) { Text("Sign out", color = MaterialTheme.colorScheme.error) } }
                        },
                    )
                }
                if (shown.any { !it.current }) {
                    item {
                        OutlinedButton(onClick = { revokeOthers = true }, modifier = Modifier.fillMaxWidth().padding(16.dp)) {
                            Text("Sign out all other sessions", color = MaterialTheme.colorScheme.error)
                        }
                    }
                }
            }
        }
    }
    revoke?.let { s ->
        Confirm("Sign this session out?", "Sign out", onDismiss = { revoke = null }, onConfirm = {
            revoke = null
            run("Signed out.") { session.revokeSession(s.id) }
        })
    }
    if (revokeOthers) {
        Confirm("Sign out every other session?", "Sign out", onDismiss = { revokeOthers = false }, onConfirm = {
            revokeOthers = false
            run("Every other session was signed out.") { session.revokeOtherSessions() }
        })
    }
}

/**
 * Settings → Single sign-on: the accounts at providers linked to the
 * person's, to unlink, and the providers to link another at, in the
 * system browser as a sign-in does.
 */
@Composable
fun SignInsScreen(session: ChatSession, app: GoChatApp) {
    val toasts = LocalToasts.current
    val platform = LocalPlatform.current
    val scope = rememberCoroutineScope()
    val link by app.link.collectAsState()
    var signIns by remember { mutableStateOf<SignIns?>(null) }
    var providers by remember { mutableStateOf(emptyList<SSOProvider>()) }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableStateOf(0) }
    var unlink by remember { mutableStateOf<SSOIdentity?>(null) }
    LaunchedEffect(reload) {
        try {
            signIns = session.signIns()
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    LaunchedEffect(Unit) {
        providers = session.ssoProviders()
    }
    LaunchedEffect(link) {
        when (val l = link) {
            is SsoLink.Linked -> toasts.show("Linked your ${l.provider} account${if (l.email.isEmpty()) "" else " " + l.email}.")
            is SsoLink.Failed -> toasts.show(l.reason)
            null -> return@LaunchedEffect
        }
        app.linkSeen()
        reload++
    }
    Screen("Single sign-on") { modifier ->
        val shown = signIns
        when {
            error != null && shown == null -> ErrorState(error ?: "", onRetry = { reload++ }, modifier = modifier)
            shown == null -> Loading(modifier)
            else -> LazyColumn(modifier) {
                item {
                    Intro(
                        (if (shown.hasPassword) "Sign in with these accounts as well as your password." else "Your account has no password: you sign in with these, and their own second factor guards it.") +
                            " Two-factor codes, when on, are still asked for.",
                    )
                }
                if (shown.linked.isEmpty()) {
                    item { Text("No accounts are linked.", modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) }
                }
                val now = Format.nowMillis()
                val unlinkable = canUnlink(shown.linked, shown.hasPassword)
                items(shown.linked, key = { it.issuer + "\n" + it.subject }) { id ->
                    ListItem(
                        headlineContent = { Text(id.provider_name.ifEmpty { id.provider_id }) },
                        supportingContent = { id.about(now).takeIf { it.isNotEmpty() }?.let { Text(it) } },
                        trailingContent = if (!unlinkable) null else {
                            { TextButton(onClick = { unlink = id }) { Text("Unlink", color = MaterialTheme.colorScheme.error) } }
                        },
                    )
                }
                if (!unlinkable && shown.linked.isNotEmpty()) {
                    item { Intro("This is the only way you sign in, so it stays linked. Link another account to unlink this one.") }
                }
                if (providers.isNotEmpty()) {
                    item {
                        FlowRow(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                            providers.forEach { p ->
                                OutlinedButton(onClick = {
                                    scope.launch {
                                        try {
                                            val url = app.linkUrl(p.id)
                                            platform.signInInBrowser(url) { app.ssoReturned(it) }
                                        } catch (e: Exception) {
                                            toasts.show(e.userMessage())
                                        }
                                    }
                                }) { Text("Link ${p.name}") }
                            }
                        }
                    }
                }
            }
        }
    }
    unlink?.let { id ->
        Confirm("Unlink this ${id.provider_name} account?", "Unlink", onDismiss = { unlink = null }, onConfirm = {
            unlink = null
            session.launch({ session.unlinkSso(id) }) { err ->
                toasts.show(err ?: "Unlinked.")
                reload++
            }
        })
    }
}
