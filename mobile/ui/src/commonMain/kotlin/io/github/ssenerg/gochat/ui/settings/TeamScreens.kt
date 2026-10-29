package io.github.ssenerg.gochat.ui.settings

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ExitToApp
import androidx.compose.material.icons.filled.Group
import androidx.compose.material.icons.filled.Link
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
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
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.squareup.wire.Instant
import gochat.v1.InviteSummary
import gochat.v1.Team
import gochat.v1.TeamMember
import gochat.v1.TeamRole
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.model.Format
import io.github.ssenerg.gochat.core.model.canChange
import io.github.ssenerg.gochat.core.model.displayName
import io.github.ssenerg.gochat.core.model.initials
import io.github.ssenerg.gochat.core.model.isAdmin
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.ui.Avatar
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.BotBadge
import io.github.ssenerg.gochat.ui.EmptyState
import io.github.ssenerg.gochat.ui.ErrorState
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.LocalImages
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.ServerImage
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import io.github.ssenerg.gochat.ui.main.Route
import io.github.ssenerg.gochat.ui.rememberFilePicker
import kotlinx.coroutines.launch

@Composable
private fun Screen(title: String, content: @Composable (Modifier) -> Unit) {
    val nav = LocalNavigator.current
    Scaffold(topBar = { TopAppBar(title = { Text(title) }, navigationIcon = { BackButton { nav.pop() } }) }) { padding ->
        content(Modifier.padding(padding).fillMaxSize())
    }
}

/** The team's photo, or its initials. */
@Composable
private fun TeamMark(team: Team) {
    val photo = LocalImages.current?.api?.teamPhotoUrl(team.id, team.photo)
    Box(
        Modifier.size(64.dp).clip(RoundedCornerShape(16.dp)).background(MaterialTheme.colorScheme.surfaceContainerHigh),
        contentAlignment = Alignment.Center,
    ) {
        if (photo != null) {
            ServerImage(photo, contentDescription = null, modifier = Modifier.fillMaxSize())
        } else {
            Text(team.initials(), style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
        }
    }
}

private fun Instant.millis() = getEpochSecond() * 1000 + getNano() / 1_000_000

/**
 * A team's settings, as the web shows them: leaving it, for everyone, and
 * its photo and open invites for its admins.
 */
@Composable
fun TeamSettingsScreen(session: ChatSession, teamId: String) {
    val nav = LocalNavigator.current
    val toasts = LocalToasts.current
    val platform = LocalPlatform.current
    val scope = rememberCoroutineScope()
    val st by session.state.collectAsState()
    val team = st.teams.firstOrNull { it.id == teamId }
    if (team == null) {
        Screen("Team settings") { EmptyState("You are no longer in this team.", it) }
        return
    }
    val admin = team.isAdmin
    var leaving by remember { mutableStateOf(false) }
    var uploading by remember { mutableStateOf(false) }
    var invites by remember { mutableStateOf<List<InviteSummary>?>(null) }
    var next by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableStateOf(0) }
    var revoking by remember { mutableStateOf<InviteSummary?>(null) }
    val pickPhoto = rememberFilePicker(
        onPicked = { file ->
            uploading = true
            session.launch({ session.setTeamPhoto(teamId, file.bytes) }) { err ->
                uploading = false
                toasts.show(err ?: "Saved.")
            }
        },
        onError = { toasts.show(it) },
        photos = true,
    )
    LaunchedEffect(teamId, reload, admin) {
        if (!admin) return@LaunchedEffect
        // So the next page loads again after a reload, even with the same token.
        next = ""
        try {
            val page = session.api.invites(teamId)
            invites = page.invites
            next = page.next
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    fun more() = scope.launch {
        try {
            val page = session.api.invites(teamId, next)
            invites = invites.orEmpty() + page.invites.filter { i -> invites.orEmpty().none { it.id == i.id } }
            next = page.next
        } catch (e: Exception) {
            toasts.show(e.userMessage())
        }
    }

    Screen("Team settings") { modifier ->
        LazyColumn(modifier) {
            item {
                Row(Modifier.fillMaxWidth().padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.clip(RoundedCornerShape(16.dp)).clickable(enabled = admin && !uploading) { pickPhoto() }) { TeamMark(team) }
                    Spacer(Modifier.width(16.dp))
                    Column(Modifier.weight(1f)) {
                        Text(team.display_name, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                        Text("/t/${team.name}", color = LocalChatColors.current.muted)
                    }
                }
                if (team.description.isNotEmpty()) Text(team.description, modifier = Modifier.padding(horizontal = 16.dp))
            }
            if (admin) item {
                Row(Modifier.padding(horizontal = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                    TextButton(onClick = { pickPhoto() }, enabled = !uploading) { Text(if (team.photo.isEmpty()) "Upload a photo" else "Change photo") }
                    if (team.photo.isNotEmpty()) {
                        TextButton(onClick = {
                            session.launch({ session.removeTeamPhoto(teamId) }) { err -> err?.let { toasts.show(it) } }
                        }, enabled = !uploading) { Text("Remove") }
                    }
                    if (uploading) Text("Uploading…", color = LocalChatColors.current.muted)
                }
                Text(
                    "JPEG, PNG, GIF or WebP up to 10 MB, cropped to a square.",
                    color = LocalChatColors.current.muted,
                    fontSize = 12.sp,
                    modifier = Modifier.padding(horizontal = 16.dp),
                )
            }
            item {
                HorizontalDivider(Modifier.padding(top = 12.dp))
                ListItem(
                    headlineContent = { Text("Team members") },
                    supportingContent = { Text(if (admin) "Make admins, or remove people from the team." else "Who is in the team.") },
                    leadingContent = { Icon(Icons.Filled.Group, contentDescription = null) },
                    modifier = Modifier.clickable { nav.open(Route.TeamMembers(teamId)) },
                )
                ListItem(
                    headlineContent = { Text("Leave team", color = MaterialTheme.colorScheme.error) },
                    supportingContent = { Text("You leave its channels and conversations too.") },
                    leadingContent = { Icon(Icons.AutoMirrored.Filled.ExitToApp, contentDescription = null, tint = MaterialTheme.colorScheme.error) },
                    modifier = Modifier.clickable { leaving = true },
                )
                HorizontalDivider()
            }
            if (admin) item {
                SectionTitle("Open invites")
                ListItem(
                    headlineContent = { Text("Create invite link") },
                    supportingContent = { Text("Each link admits one person and expires after a while. Create one per person.") },
                    leadingContent = { Icon(Icons.Filled.Link, contentDescription = null) },
                    modifier = Modifier.clickable {
                        scope.launch {
                            try {
                                val invite = session.api.createInvite(teamId)
                                platform.share("Join ${team.display_name} on GoChat: ${session.api.client.baseUrl}/join/${invite.token}")
                                reload++
                            } catch (e: Exception) {
                                toasts.show(e.userMessage())
                            }
                        }
                    },
                )
            }
            val list = invites
            when {
                !admin -> {}
                error != null && list == null -> item { ErrorState(error ?: "", onRetry = { reload++ }) }
                list == null -> item { Loading(Modifier.fillMaxWidth().padding(16.dp)) }
                list.isEmpty() -> item { Text("No open invites.", color = LocalChatColors.current.muted, modifier = Modifier.padding(16.dp)) }
                else -> {
                    items(list, key = { it.id }) { invite -> InviteRow(invite, onRevoke = { revoking = invite }) }
                    if (next.isNotEmpty()) item { LaunchedEffect(next) { more() } }
                }
            }
        }
    }

    if (leaving) {
        AlertDialog(
            onDismissRequest = { leaving = false },
            title = { Text("Leave ${team.display_name}?") },
            text = { Text("You leave its channels and conversations too, and need a new invite to come back.") },
            confirmButton = {
                TextButton(onClick = {
                    leaving = false
                    session.launch({ session.leaveTeam(teamId) }) { err ->
                        // Out of the team, its screens have nothing to show.
                        if (err == null) nav.home() else toasts.show(err)
                    }
                }) { Text("Leave", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { leaving = false }) { Text("Cancel") } },
        )
    }

    revoking?.let { invite ->
        AlertDialog(
            onDismissRequest = { revoking = null },
            title = { Text("Revoke this invite?") },
            text = { Text("Its link stops working.") },
            confirmButton = {
                TextButton(onClick = {
                    revoking = null
                    scope.launch {
                        try {
                            session.api.revokeInvite(teamId, invite.id)
                        } catch (e: Exception) {
                            toasts.show(e.userMessage())
                        }
                        reload++
                    }
                }) { Text("Revoke", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { revoking = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun InviteRow(invite: InviteSummary, onRevoke: () -> Unit) {
    val now = Format.nowMillis()
    val created = invite.created_at?.let { Format.short(it.millis(), now) }
    val expires = invite.expires_at?.let { Format.short(it.millis(), now) }
    ListItem(
        headlineContent = { Text(invite.created_by?.displayName() ?: "Someone") },
        supportingContent = {
            Column {
                Text(listOfNotNull(created?.let { "Created $it" }, expires?.let { "expires $it" }).joinToString(" · "))
                if (!invite.active) Text("Suspended: its creator can no longer invite.", color = LocalChatColors.current.muted)
            }
        },
        trailingContent = { TextButton(onClick = onRevoke) { Text("Revoke", color = MaterialTheme.colorScheme.error) } },
    )
}

/** The team's members; its admins change roles and remove people, as the server lets them. */
@Composable
fun TeamMembersScreen(session: ChatSession, teamId: String) {
    val toasts = LocalToasts.current
    val scope = rememberCoroutineScope()
    val st by session.state.collectAsState()
    val team = st.teams.firstOrNull { it.id == teamId }
    if (team == null) {
        Screen("Team members") { EmptyState("You are no longer in this team.", it) }
        return
    }
    var members by remember { mutableStateOf<List<TeamMember>?>(null) }
    var next by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableStateOf(0) }
    var removing by remember { mutableStateOf<TeamMember?>(null) }
    // Someone who left goes from the list.
    LaunchedEffect(teamId) { session.membersChanged.collect { if (it == teamId) reload++ } }
    LaunchedEffect(teamId, reload) {
        try {
            val page = session.api.teamMembersPage(teamId)
            members = page.members
            next = page.next
            error = null
        } catch (e: Exception) {
            error = e.userMessage()
        }
    }
    fun more() = scope.launch {
        try {
            val page = session.api.teamMembersPage(teamId, after = next)
            members = members.orEmpty() + page.members.filter { m -> members.orEmpty().none { it.user?.id == m.user?.id } }
            next = page.next
        } catch (e: Exception) {
            toasts.show(e.userMessage())
        }
    }
    fun setRole(m: TeamMember, role: TeamRole) = scope.launch {
        val id = m.user?.id ?: return@launch
        try {
            session.api.setMemberRole(teamId, id, role)
            members = members?.map { if (it.user?.id == id) it.copy(role = role) else it }
        } catch (e: Exception) {
            toasts.show(e.userMessage())
        }
    }

    Screen("Team members") { modifier ->
        val list = members
        when {
            error != null && list == null -> ErrorState(error ?: "", onRetry = { reload++ }, modifier = modifier)
            list == null -> Loading(modifier)
            else -> LazyColumn(modifier) {
                items(list, key = { it.user?.id ?: "" }) { m ->
                    MemberRow(team, m, team.canChange(st.me, m), onRole = { setRole(m, it) }, onRemove = { removing = m })
                }
                if (next.isNotEmpty()) item { LaunchedEffect(next) { more() } }
            }
        }
    }

    removing?.let { m ->
        AlertDialog(
            onDismissRequest = { removing = null },
            title = { Text("Remove ${m.user.displayName()} from the team?") },
            text = { Text("They leave its channels too.") },
            confirmButton = {
                TextButton(onClick = {
                    removing = null
                    scope.launch {
                        val id = m.user?.id ?: return@launch
                        try {
                            session.api.removeMember(teamId, id)
                            members = members?.filter { it.user?.id != id }
                        } catch (e: Exception) {
                            toasts.show(e.userMessage())
                        }
                    }
                }) { Text("Remove", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { removing = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun MemberRow(team: Team, m: TeamMember, canChange: Boolean, onRole: (TeamRole) -> Unit, onRemove: () -> Unit) {
    var menu by remember { mutableStateOf(false) }
    val admin = m.role == TeamRole.TEAM_ROLE_ADMIN
    val role = when {
        m.user?.id == team.owner_id -> " · owner"
        admin -> " · admin"
        else -> ""
    }
    ListItem(
        leadingContent = { Avatar(m.user) },
        headlineContent = {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(m.user.displayName(), fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                if (m.user?.bot == true) BotBadge(Modifier.padding(start = 6.dp))
            }
        },
        supportingContent = { Text("@${m.user?.username.orEmpty()}$role", color = LocalChatColors.current.muted) },
        trailingContent = {
            if (canChange) {
                Box {
                    IconButton(onClick = { menu = true }) { Icon(Icons.Filled.MoreVert, contentDescription = "Change ${m.user.displayName()}") }
                    DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                        DropdownMenuItem(
                            text = { Text(if (admin) "Make member" else "Make admin") },
                            onClick = {
                                menu = false
                                onRole(if (admin) TeamRole.TEAM_ROLE_MEMBER else TeamRole.TEAM_ROLE_ADMIN)
                            },
                        )
                        DropdownMenuItem(
                            text = { Text("Remove from the team", color = MaterialTheme.colorScheme.error) },
                            onClick = {
                                menu = false
                                onRemove()
                            },
                        )
                    }
                }
            }
        },
    )
}
