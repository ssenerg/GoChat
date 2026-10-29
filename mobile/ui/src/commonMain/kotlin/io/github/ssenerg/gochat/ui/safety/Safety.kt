package io.github.ssenerg.gochat.ui.safety

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Chat
import androidx.compose.material.icons.filled.Block
import androidx.compose.material.icons.filled.Flag
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.rememberModalBottomSheetState
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
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.squareup.wire.Instant
import gochat.v1.BlockedUser
import gochat.v1.Report
import gochat.v1.ReportAction
import gochat.v1.ReportReason
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.ui.Avatar
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.BotBadge
import io.github.ssenerg.gochat.ui.EmptyState
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import kotlinx.coroutines.launch

/** The reasons a report gives, as the web words them. */
private val reasons = listOf(
    ReportReason.REPORT_REASON_SPAM to "Spam",
    ReportReason.REPORT_REASON_HARASSMENT to "Harassment or bullying",
    ReportReason.REPORT_REASON_HATE to "Hate speech",
    ReportReason.REPORT_REASON_SEXUAL to "Sexual content",
    ReportReason.REPORT_REASON_VIOLENCE to "Violence or threats",
    ReportReason.REPORT_REASON_OTHER to "Something else",
)

private fun reasonLabel(r: ReportReason) = reasons.firstOrNull { it.first == r }?.second ?: "Reported"

const val REPORT_SENT = "Thank you. The admins will look at your report."

/** Asks why a message or a person is reported; [onSend] gets the reason and what the person added. */
@Composable
fun ReportDialog(title: String, onDismiss: () -> Unit, onSend: (ReportReason, String) -> Unit) {
    var reason by remember { mutableStateOf<ReportReason?>(null) }
    var details by remember { mutableStateOf("") }
    val ready = reason != null && (reason != ReportReason.REPORT_REASON_OTHER || details.isNotBlank())
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                Text(
                    "Your report goes to the team's admins. The person you report is not told who sent it.",
                    color = LocalChatColors.current.muted,
                    fontSize = 13.sp,
                )
                reasons.forEach { (r, label) ->
                    Row(
                        Modifier.fillMaxWidth().selectable(selected = reason == r, role = Role.RadioButton) { reason = r }.padding(vertical = 2.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        RadioButton(selected = reason == r, onClick = null)
                        Spacer(Modifier.width(8.dp))
                        Text(label)
                    }
                }
                OutlinedTextField(
                    value = details,
                    onValueChange = { details = it.take(MAX_DETAILS) },
                    label = { Text(if (reason == ReportReason.REPORT_REASON_OTHER) "What happened?" else "Anything to add?") },
                    minLines = 2,
                    modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
                )
            }
        },
        confirmButton = {
            TextButton(onClick = { reason?.let { onSend(it, details.trim()) } }, enabled = ready) { Text("Send report") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

private const val MAX_DETAILS = 500

/**
 * A person's profile, from a message's author or a member list: message
 * them, block or unblock them, or report them to the team's admins.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PersonSheet(session: ChatSession, user: User, onDismiss: () -> Unit) {
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val st by session.state.collectAsState()
    val blocked = user.id in st.blocked
    val me = user.id == st.me.id
    val teamId = st.teamId
    var confirmBlock by remember { mutableStateOf(false) }
    var reporting by remember { mutableStateOf(false) }

    if (!confirmBlock && !reporting) {
        ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheet) {
            Column(Modifier.navigationBarsPadding().padding(bottom = 8.dp)) {
                Row(Modifier.fillMaxWidth().padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
                    Avatar(user, size = 56.dp)
                    Spacer(Modifier.width(14.dp))
                    Column(Modifier.weight(1f)) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(user.displayName(), style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
                            if (user.bot) {
                                Spacer(Modifier.width(6.dp))
                                BotBadge()
                            }
                        }
                        Text("@" + user.username, color = LocalChatColors.current.muted)
                        if (blocked) Text("You blocked them.", color = LocalChatColors.current.muted, fontSize = 13.sp)
                    }
                }
                if (!me) {
                    HorizontalDivider()
                    if (!blocked && !user.deactivated && teamId != null) {
                        SheetAction(Icons.AutoMirrored.Filled.Chat, "Message") {
                            onDismiss()
                            session.launch({ runCatching { session.openConversation(listOf(user.id)) } }) { r ->
                                r.onSuccess { nav.openChannel(it.id) }.onFailure { toasts.show(it.userMessage()) }
                            }
                        }
                    }
                    if (blocked) {
                        SheetAction(Icons.Filled.Block, "Unblock") {
                            onDismiss()
                            session.launch({ session.block(user.id, false) }) { err -> toasts.show(err ?: "Unblocked.") }
                        }
                    } else {
                        SheetAction(Icons.Filled.Block, "Block", danger = true) { confirmBlock = true }
                    }
                    if (teamId != null) SheetAction(Icons.Filled.Flag, "Report", danger = true) { reporting = true }
                }
            }
        }
    }
    if (confirmBlock) {
        AlertDialog(
            onDismissRequest = onDismiss,
            title = { Text("Block ${user.displayName()}?") },
            text = { Text("They can no longer message you or start a conversation with you, and their messages show folded. They are not told.") },
            confirmButton = {
                TextButton(onClick = {
                    onDismiss()
                    session.launch({ session.block(user.id, true) }) { err -> toasts.show(err ?: "Blocked.") }
                }) { Text("Block", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
        )
    }
    if (reporting && teamId != null) {
        ReportDialog("Report ${user.displayName()}", onDismiss = onDismiss) { reason, details ->
            onDismiss()
            session.launch({ session.reportPerson(teamId, user.id, reason, details) }) { err -> toasts.show(err ?: REPORT_SENT) }
        }
    }
}

@Composable
private fun SheetAction(icon: androidx.compose.ui.graphics.vector.ImageVector, text: String, danger: Boolean = false, onClick: () -> Unit) {
    val color = if (danger) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface
    ListItem(
        headlineContent = { Text(text, color = color) },
        leadingContent = { Icon(icon, contentDescription = null, tint = color) },
        colors = ListItemDefaults.colors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
        modifier = Modifier.clickable(onClick = onClick),
    )
}

/** A message by someone the person blocked, folded until tapped. */
@Composable
fun FoldedMessage(onShow: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onShow).padding(horizontal = 12.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(Icons.Filled.Block, contentDescription = null, tint = LocalChatColors.current.muted, modifier = Modifier.size(16.dp))
        Spacer(Modifier.width(8.dp))
        Text("Message from someone you blocked", color = LocalChatColors.current.muted, fontSize = 13.sp, modifier = Modifier.weight(1f))
        Text("Show", color = MaterialTheme.colorScheme.primary, fontSize = 13.sp, fontWeight = FontWeight.SemiBold)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun Screen(title: String, content: @Composable (Modifier) -> Unit) {
    val nav = LocalNavigator.current
    Scaffold(topBar = { TopAppBar(title = { Text(title) }, navigationIcon = { BackButton { nav.pop() } }) }) { padding ->
        content(Modifier.padding(padding).fillMaxSize())
    }
}

/** Settings → Blocked people: whom the person blocked, to unblock. */
@Composable
fun BlockedPeopleScreen(session: ChatSession) {
    val toasts = LocalToasts.current
    val st by session.state.collectAsState()
    var people by remember { mutableStateOf<List<BlockedUser>?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableStateOf(0) }
    // Unblocking elsewhere, or here, lists them again.
    LaunchedEffect(reload, st.blocked) {
        try {
            people = session.blockedPeople()
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    Screen("Blocked people") { modifier ->
        val list = people
        when {
            error != null && list == null -> ErrorState(error ?: "", onRetry = { reload++ }, modifier = modifier)
            list == null -> Loading(modifier)
            list.isEmpty() -> EmptyState("You have not blocked anyone.", modifier)
            else -> LazyColumn(modifier) {
                item {
                    Text(
                        "People you blocked cannot message you or start a conversation with you, and their messages show folded. They are not told.",
                        color = LocalChatColors.current.muted,
                        fontSize = 13.sp,
                        modifier = Modifier.padding(16.dp),
                    )
                }
                items(list, key = { it.user?.id.orEmpty() }) { b ->
                    val u = b.user ?: return@items
                    ListItem(
                        headlineContent = { Text(u.displayName()) },
                        supportingContent = { Text("@" + u.username) },
                        leadingContent = { Avatar(u) },
                        trailingContent = {
                            TextButton(onClick = {
                                session.launch({ session.block(u.id, false) }) { err -> err?.let { toasts.show(it) } }
                            }) { Text("Unblock") }
                        },
                    )
                }
            }
        }
    }
}

/**
 * The reports waiting for admins: a team's for its admins, every team's for
 * a system admin when [teamId] is empty, open or resolved.
 */
@Composable
fun ReportsScreen(session: ChatSession, teamId: String) {
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    val st by session.state.collectAsState()
    val system = teamId.isEmpty()
    var resolved by remember { mutableStateOf(false) }
    var reports by remember { mutableStateOf<List<Report>?>(null) }
    var next by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    // New reports bring the list again.
    val open = if (system) st.allReports else st.reports[teamId] ?: 0
    LaunchedEffect(teamId, resolved, open) {
        try {
            val page = session.api.reports(teamId, resolved)
            reports = page.reports
            next = page.next
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    fun more() = scope.launch {
        try {
            val page = session.api.reports(teamId, resolved, next)
            reports = reports.orEmpty() + page.reports.filter { r -> reports.orEmpty().none { it.id == r.id } }
            next = page.next
        } catch (e: Exception) {
            toasts.show(e.userMessage())
        }
    }
    fun resolve(r: Report, action: ReportAction) {
        session.launch({ session.resolveReport(r.id, action) }) { err ->
            if (err != null) toasts.show(err) else reports = reports.orEmpty().filter { it.id != r.id }
        }
    }

    Screen(if (system) "All reports" else "Reports") { modifier ->
        Column(modifier) {
            Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                FilterChip(selected = !resolved, onClick = { resolved = false }, label = { Text("Open") })
                FilterChip(selected = resolved, onClick = { resolved = true }, label = { Text("Resolved") })
            }
            val list = reports
            when {
                error != null && list == null -> ErrorState(error ?: "", modifier = Modifier.weight(1f))
                list == null -> Loading(Modifier.weight(1f))
                list.isEmpty() -> EmptyState(if (resolved) "No resolved reports." else "No open reports.", Modifier.weight(1f))
                else -> LazyColumn(Modifier.weight(1f)) {
                    items(list, key = { it.id }) { r ->
                        ReportRow(r, system) { action -> resolve(r, action) }
                        HorizontalDivider()
                    }
                    if (next.isNotEmpty()) {
                        item(key = "more") {
                            LaunchedEffect(next) { more() }
                            Loading(Modifier.fillMaxWidth().padding(16.dp))
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun ReportRow(r: Report, system: Boolean, onAct: (ReportAction) -> Unit) {
    val muted = LocalChatColors.current.muted
    var confirm by remember { mutableStateOf<Pair<ReportAction, String>?>(null) }
    Column(Modifier.fillMaxWidth().padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(reasonLabel(r.reason), fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
            r.created_at?.let { Text(Format.short(it.millis(), Format.nowMillis()), color = muted, fontSize = 12.sp) }
        }
        val about = if (r.message_id.isNotEmpty()) "A message by ${r.user.displayName()}" else r.user.displayName()
        Text("$about · @${r.user?.username.orEmpty()}")
        val where = listOfNotNull(r.team_name.ifEmpty { null }, r.channel_name.ifEmpty { null }?.let { "#$it" })
        Text("From ${r.reporter.displayName()}" + where.joinToString("") { " · $it" }, color = muted, fontSize = 13.sp)
        if (r.excerpt.isNotEmpty()) {
            Text(r.excerpt, modifier = Modifier.padding(vertical = 4.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (r.details.isNotEmpty()) Text("“${r.details}”", fontSize = 13.sp)
        if (r.action != ReportAction.REPORT_ACTION_UNSPECIFIED) {
            Text("${doneLabel(r.action)} by ${r.resolved_by.displayName()}", color = muted, fontSize = 13.sp)
        } else {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(top = 4.dp)) {
                OutlinedButton(onClick = { onAct(ReportAction.REPORT_ACTION_DISMISS) }) { Text("Dismiss") }
                if (r.message_id.isNotEmpty()) {
                    OutlinedButton(onClick = { confirm = ReportAction.REPORT_ACTION_DELETE_MESSAGE to "Delete this message for everyone?" }) { Text("Delete") }
                }
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                if (r.team_id.isNotEmpty()) {
                    TextButton(onClick = { confirm = ReportAction.REPORT_ACTION_REMOVE_MEMBER to "Remove ${r.user.displayName()} from ${r.team_name}?" }) {
                        Text("Remove from team", color = MaterialTheme.colorScheme.error)
                    }
                }
                if (system) {
                    TextButton(onClick = {
                        confirm = ReportAction.REPORT_ACTION_DEACTIVATE to "Deactivate ${r.user.displayName()}? They are signed out everywhere at once and cannot sign in."
                    }) { Text("Deactivate", color = MaterialTheme.colorScheme.error) }
                }
            }
        }
    }
    confirm?.let { (action, question) ->
        AlertDialog(
            onDismissRequest = { confirm = null },
            text = { Text(question) },
            confirmButton = {
                TextButton(onClick = {
                    confirm = null
                    onAct(action)
                }) { Text("Yes", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { confirm = null }) { Text("Cancel") } },
        )
    }
}

private fun Instant.millis() = getEpochSecond() * 1000 + getNano() / 1_000_000

private fun doneLabel(a: ReportAction) = when (a) {
    ReportAction.REPORT_ACTION_DISMISS -> "Dismissed"
    ReportAction.REPORT_ACTION_DELETE_MESSAGE -> "Message deleted"
    ReportAction.REPORT_ACTION_REMOVE_MEMBER -> "Removed from the team"
    ReportAction.REPORT_ACTION_DEACTIVATE -> "Account deactivated"
    else -> "Resolved"
}
