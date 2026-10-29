package io.github.ssenerg.gochat.ui.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Logout
import androidx.compose.material.icons.filled.Block
import androidx.compose.material.icons.filled.Flag
import androidx.compose.material.icons.filled.Key
import androidx.compose.material.icons.filled.Security
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
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
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gochat.v1.Status
import gochat.v1.SystemRole
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Emojis
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.ui.Avatar
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.PresenceDot
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.Route
import io.github.ssenerg.gochat.ui.main.LocalToasts
import io.github.ssenerg.gochat.ui.rememberFilePicker
import io.github.ssenerg.gochat.ui.statusLabel
import kotlinx.coroutines.launch

private val dndChoices = listOf(30 to "30 minutes", 60 to "1 hour", 4 * 60 to "4 hours", 24 * 60 to "Until tomorrow", 0 to "Until I turn it off")
private val clearChoices = listOf(30 to "30 minutes", 60 to "1 hour", 4 * 60 to "4 hours", 24 * 60 to "Today", 0 to "Don't clear")

@Composable
fun SettingsScreen(session: ChatSession, app: GoChatApp) {
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    val st by session.state.collectAsState()
    val me = st.me
    val status = if (me.status == Status.STATUS_UNSPECIFIED) Status.STATUS_ONLINE else me.status
    var editName by remember { mutableStateOf(false) }
    var pickDnd by remember { mutableStateOf(false) }
    var editCustom by remember { mutableStateOf(false) }
    var changePassword by remember { mutableStateOf(false) }
    var confirmSignOut by remember { mutableStateOf(false) }
    val pickPhoto = rememberFilePicker(
        onPicked = { file -> session.launch({ session.setPhoto(file.bytes) }) { err -> err?.let { toasts.show(it) } } },
        onError = { toasts.show(it) },
        photos = true,
    )

    fun run(block: suspend () -> String?) {
        session.launch(block) { err -> err?.let { toasts.show(it) } }
    }

    Scaffold(topBar = { TopAppBar(title = { Text("Settings") }, navigationIcon = { BackButton { nav.pop() } }) }) { padding ->
        Column(Modifier.padding(padding).fillMaxSize().verticalScroll(rememberScrollState())) {
            Row(Modifier.fillMaxWidth().padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
                Box(Modifier.clip(CircleShape).clickable { pickPhoto() }) { Avatar(me, size = 64.dp) }
                Spacer(Modifier.width(16.dp))
                Column(Modifier.weight(1f)) {
                    Text(me.displayName(), style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                    Text("@" + me.username, color = LocalChatColors.current.muted)
                    Text(session.api.client.baseUrl.removePrefix("https://"), color = LocalChatColors.current.muted, fontSize = 12.sp)
                }
            }
            Row(Modifier.padding(horizontal = 8.dp)) {
                TextButton(onClick = { editName = true }) { Text("Edit name") }
                TextButton(onClick = { pickPhoto() }) { Text("Change photo") }
                if (me.photo.isNotEmpty()) TextButton(onClick = { run { session.removePhoto() } }) { Text("Remove photo") }
            }
            HorizontalDivider()
            SectionTitle("Status")
            listOf(Status.STATUS_ONLINE, Status.STATUS_AWAY, Status.STATUS_DND).forEach { s ->
                ListItem(
                    headlineContent = { Text(statusLabel(s)) },
                    leadingContent = { PresenceDot(s, Modifier.size(12.dp)) },
                    trailingContent = { RadioButton(selected = status == s, onClick = null) },
                    modifier = Modifier.clickable {
                        if (s == Status.STATUS_DND) pickDnd = true else run { session.setStatus(s) }
                    },
                )
            }
            val custom = me.custom_status
            ListItem(
                headlineContent = { Text("Custom status") },
                supportingContent = {
                    Text(
                        if (custom != null && (custom.text.isNotEmpty() || custom.emoji.isNotEmpty())) {
                            listOfNotNull(Emojis.char(custom.emoji), custom.text.ifEmpty { null }).joinToString(" ")
                        } else {
                            "What are you up to?"
                        },
                    )
                },
                modifier = Modifier.clickable { editCustom = true },
            )
            HorizontalDivider()
            SectionTitle("Account")
            ListItem(
                headlineContent = { Text("Change password") },
                leadingContent = { Icon(Icons.Filled.Key, contentDescription = null) },
                modifier = Modifier.clickable { changePassword = true },
            )
            ListItem(
                headlineContent = { Text("Two-factor authentication") },
                supportingContent = { Text(if (me.totp_enabled) "On" else "Off") },
                leadingContent = { Icon(Icons.Filled.Security, contentDescription = null) },
                modifier = Modifier.clickable { nav.open(Route.TwoFactor) },
            )
            ListItem(
                headlineContent = { Text("Blocked people") },
                supportingContent = { Text(if (st.blocked.isEmpty()) "No one" else "${st.blocked.size} blocked") },
                leadingContent = { Icon(Icons.Filled.Block, contentDescription = null) },
                modifier = Modifier.clickable { nav.open(Route.Blocked) },
            )
            if (me.role == SystemRole.SYSTEM_ROLE_ADMIN) {
                ListItem(
                    headlineContent = { Text("All reports") },
                    supportingContent = { Text(if (st.allReports == 0) "None open" else "${st.allReports} open") },
                    leadingContent = { Icon(Icons.Filled.Flag, contentDescription = null) },
                    modifier = Modifier.clickable { nav.open(Route.Reports("")) },
                )
            }
            ListItem(
                headlineContent = { Text("Sign out", color = MaterialTheme.colorScheme.error) },
                leadingContent = { Icon(Icons.AutoMirrored.Filled.Logout, contentDescription = null, tint = MaterialTheme.colorScheme.error) },
                modifier = Modifier.clickable { confirmSignOut = true },
            )
        }
    }

    if (editName) {
        TextDialog("Display name", me.display_name, onDismiss = { editName = false }) { name ->
            editName = false
            run { session.updateProfile(name.trim()) }
        }
    }
    if (pickDnd) {
        ChoiceDialog("Do not disturb for", dndChoices, onDismiss = { pickDnd = false }) { minutes ->
            pickDnd = false
            run { session.setStatus(Status.STATUS_DND, minutes) }
        }
    }
    if (editCustom) CustomStatusDialog(session, onDone = { editCustom = false })
    if (changePassword) PasswordDialog(session, onDone = { changePassword = false })
    if (confirmSignOut) {
        AlertDialog(
            onDismissRequest = { confirmSignOut = false },
            title = { Text("Sign out?") },
            confirmButton = {
                TextButton(onClick = {
                    confirmSignOut = false
                    scope.launch { app.signOut() }
                }) { Text("Sign out", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { confirmSignOut = false }) { Text("Cancel") } },
        )
    }
}

@Composable
internal fun SectionTitle(text: String) {
    Text(
        text.uppercase(),
        color = LocalChatColors.current.muted,
        fontSize = 12.sp,
        fontWeight = FontWeight.SemiBold,
        modifier = Modifier.padding(start = 16.dp, top = 16.dp, bottom = 4.dp),
    )
}

@Composable
private fun TextDialog(title: String, initial: String, onDismiss: () -> Unit, onSave: (String) -> Unit) {
    var value by remember { mutableStateOf(initial) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = { OutlinedTextField(value = value, onValueChange = { value = it }, singleLine = true) },
        confirmButton = { TextButton(onClick = { onSave(value) }) { Text("Save") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun ChoiceDialog(title: String, choices: List<Pair<Int, String>>, onDismiss: () -> Unit, onPick: (Int) -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                choices.forEach { (minutes, label) ->
                    Text(label, modifier = Modifier.fillMaxWidth().clickable { onPick(minutes) }.padding(vertical = 12.dp))
                }
            }
        },
        confirmButton = {},
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun CustomStatusDialog(session: ChatSession, onDone: () -> Unit) {
    val toasts = LocalToasts.current
    val current = session.state.value.me.custom_status
    var emoji by remember { mutableStateOf(current?.emoji?.ifEmpty { null } ?: "speech_balloon") }
    var text by remember { mutableStateOf(current?.text ?: "") }
    var clearIn by remember { mutableStateOf(0) }
    fun save(e: String, t: String, minutes: Int) {
        onDone()
        session.launch({ session.setCustomStatus(e, t, minutes) }) { err -> err?.let { toasts.show(it) } }
    }
    AlertDialog(
        onDismissRequest = onDone,
        title = { Text("Custom status") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                LazyRow(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                    items(Emojis.all, key = { it.name }) { e ->
                        Surface(
                            shape = CircleShape,
                            color = if (e.name == emoji) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surfaceVariant,
                        ) {
                            Box(Modifier.size(40.dp).clickable { emoji = e.name }, contentAlignment = Alignment.Center) {
                                Text(e.char, fontSize = 20.sp)
                            }
                        }
                    }
                }
                OutlinedTextField(value = text, onValueChange = { text = it.take(100) }, label = { Text("Status") }, singleLine = true)
                Text("Clear after", fontWeight = FontWeight.SemiBold)
                clearChoices.forEach { (minutes, label) ->
                    Row(Modifier.fillMaxWidth().clickable { clearIn = minutes }, verticalAlignment = Alignment.CenterVertically) {
                        RadioButton(selected = clearIn == minutes, onClick = { clearIn = minutes })
                        Text(label)
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = { save(emoji, text.trim(), clearIn) }) { Text("Save") } },
        dismissButton = { TextButton(onClick = { save("", "", 0) }) { Text("Clear") } },
    )
}

@Composable
private fun PasswordDialog(session: ChatSession, onDone: () -> Unit) {
    val scope = rememberCoroutineScope()
    val toasts = LocalToasts.current
    var current by remember { mutableStateOf("") }
    var new by remember { mutableStateOf("") }
    var confirm by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    AlertDialog(
        onDismissRequest = onDone,
        title = { Text("Change password") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(current, { current = it }, label = { Text("Current password") }, singleLine = true, visualTransformation = PasswordVisualTransformation())
                OutlinedTextField(new, { new = it }, label = { Text("New password") }, singleLine = true, visualTransformation = PasswordVisualTransformation())
                OutlinedTextField(confirm, { confirm = it }, label = { Text("Confirm new password") }, singleLine = true, visualTransformation = PasswordVisualTransformation())
                if (error != null) Text(error ?: "", color = MaterialTheme.colorScheme.error)
            }
        },
        confirmButton = {
            TextButton(onClick = {
                if (new != confirm) {
                    error = "The new passwords do not match."
                    return@TextButton
                }
                scope.launch {
                    try {
                        session.api.changePassword(current, new)
                        onDone()
                        toasts.show("Password changed.")
                    } catch (e: Exception) {
                        error = e.userMessage()
                    }
                }
            }, enabled = current.isNotEmpty() && new.isNotEmpty()) { Text("Change") }
        },
        dismissButton = { TextButton(onClick = onDone) { Text("Cancel") } },
    )
}
