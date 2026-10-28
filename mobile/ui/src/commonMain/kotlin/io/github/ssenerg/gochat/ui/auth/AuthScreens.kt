package io.github.ssenerg.gochat.ui.auth

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Forum
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.ui.PlatformBackHandler
import kotlinx.coroutines.launch

private enum class Mode { SignIn, SignUp }

/** Signing in, the second factor, and creating an account with an invite. */
@Composable
fun AuthScreens(app: GoChatApp, state: AppState) {
    var mode by remember { mutableStateOf(Mode.SignIn) }
    val initialServer = (state as? AppState.SignedOut)?.server?.ifEmpty { null } ?: app.lastServer
    var server by remember { mutableStateOf(initialServer) }
    Box(Modifier.fillMaxSize().imePadding(), contentAlignment = Alignment.Center) {
        Column(
            Modifier.widthIn(max = 420.dp).fillMaxWidth().verticalScroll(rememberScrollState()).padding(24.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Brand()
            Spacer(Modifier.height(24.dp))
            when {
                state is AppState.Verifying -> MfaForm(app)
                mode == Mode.SignUp -> {
                    PlatformBackHandler(true) { mode = Mode.SignIn }
                    SignUpForm(app, server, { server = it }) { mode = Mode.SignIn }
                }
                else -> SignInForm(app, (state as? AppState.SignedOut)?.notice, server, { server = it }) { mode = Mode.SignUp }
            }
        }
    }
}

@Composable
private fun Brand() {
    Surface(color = MaterialTheme.colorScheme.primaryContainer, shape = RoundedCornerShape(16.dp)) {
        Icon(
            Icons.Filled.Forum,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onPrimaryContainer,
            modifier = Modifier.padding(14.dp).size(36.dp),
        )
    }
    Spacer(Modifier.height(12.dp))
    Text("GoChat", style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.SemiBold)
}

@Composable
private fun ServerField(server: String, onChange: (String) -> Unit) {
    OutlinedTextField(
        value = server,
        onValueChange = onChange,
        label = { Text("Server") },
        placeholder = { Text("chat.example.com") },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
        modifier = Modifier.fillMaxWidth(),
    )
}

@Composable
private fun FormError(error: String?) {
    if (error != null) {
        Text(
            error,
            color = MaterialTheme.colorScheme.error,
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.fillMaxWidth().padding(vertical = 8.dp),
        )
    }
}

@Composable
private fun SubmitButton(text: String, busy: Boolean, enabled: Boolean, onClick: () -> Unit) {
    Button(onClick = onClick, enabled = enabled && !busy, modifier = Modifier.fillMaxWidth().height(48.dp)) {
        if (busy) CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp) else Text(text)
    }
}

@Composable
private fun SignInForm(app: GoChatApp, notice: String?, server: String, onServer: (String) -> Unit, onSignUp: () -> Unit) {
    val scope = rememberCoroutineScope()
    var login by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember(notice) { mutableStateOf(notice) }
    val ready = server.isNotBlank() && login.isNotBlank() && password.isNotEmpty()
    fun submit() {
        if (!ready || busy) return
        busy = true
        error = null
        scope.launch {
            error = app.signIn(server, login, password)
            busy = false
        }
    }
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("Sign in to your team's server", style = MaterialTheme.typography.titleMedium)
        ServerField(server, onServer)
        OutlinedTextField(
            value = login,
            onValueChange = { login = it },
            label = { Text("Username or email") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, imeAction = ImeAction.Next),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = password,
            onValueChange = { password = it },
            label = { Text("Password") },
            singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Done),
            keyboardActions = KeyboardActions(onDone = { submit() }),
            modifier = Modifier.fillMaxWidth(),
        )
        FormError(error)
        SubmitButton("Sign in", busy, ready) { submit() }
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center, verticalAlignment = Alignment.CenterVertically) {
            Text("Got an invite?", color = MaterialTheme.colorScheme.onSurfaceVariant)
            TextButton(onClick = onSignUp) { Text("Create an account") }
        }
    }
}

@Composable
private fun MfaForm(app: GoChatApp) {
    val scope = rememberCoroutineScope()
    var code by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    PlatformBackHandler(true) { app.cancelVerify() }
    fun submit() {
        if (code.isBlank() || busy) return
        busy = true
        error = null
        scope.launch {
            error = app.verify(code)
            busy = false
        }
    }
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("Two-factor authentication", style = MaterialTheme.typography.titleMedium)
        Text(
            "Enter the code from your authenticator app, or one of your recovery codes.",
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        OutlinedTextField(
            value = code,
            onValueChange = { code = it },
            label = { Text("Code") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(
                capitalization = KeyboardCapitalization.None,
                autoCorrectEnabled = false,
                keyboardType = KeyboardType.Ascii,
                imeAction = ImeAction.Done,
            ),
            keyboardActions = KeyboardActions(onDone = { submit() }),
            modifier = Modifier.fillMaxWidth(),
        )
        FormError(error)
        SubmitButton("Verify", busy, code.isNotBlank()) { submit() }
        TextButton(onClick = { app.cancelVerify() }, modifier = Modifier.fillMaxWidth()) { Text("Back to sign in") }
    }
}

@Composable
private fun SignUpForm(app: GoChatApp, server: String, onServer: (String) -> Unit, onBack: () -> Unit) {
    val scope = rememberCoroutineScope()
    var invite by remember { mutableStateOf("") }
    var username by remember { mutableStateOf("") }
    var displayName by remember { mutableStateOf("") }
    var email by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var confirm by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val ready = server.isNotBlank() && username.isNotBlank() && email.isNotBlank() && password.isNotEmpty()
    fun submit() {
        if (!ready || busy) return
        if (password != confirm) {
            error = "The passwords do not match."
            return
        }
        busy = true
        error = null
        scope.launch {
            error = app.signUp(server, invite, username, email, displayName, password)
            busy = false
        }
    }
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("Create an account", style = MaterialTheme.typography.titleMedium)
        ServerField(server, onServer)
        OutlinedTextField(
            value = invite,
            onValueChange = { invite = it },
            label = { Text("Invite link") },
            supportingText = { Text("Paste the link a team admin sent you.") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = username, onValueChange = { username = it }, label = { Text("Username") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, imeAction = ImeAction.Next),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = displayName, onValueChange = { displayName = it }, label = { Text("Display name (optional)") }, singleLine = true,
            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = email, onValueChange = { email = it }, label = { Text("Email") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, imeAction = ImeAction.Next),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = password, onValueChange = { password = it }, label = { Text("Password") }, singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Next),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = confirm, onValueChange = { confirm = it }, label = { Text("Confirm password") }, singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Done),
            keyboardActions = KeyboardActions(onDone = { submit() }),
            modifier = Modifier.fillMaxWidth(),
        )
        FormError(error)
        SubmitButton("Create account", busy, ready) { submit() }
        TextButton(onClick = onBack, modifier = Modifier.fillMaxWidth()) { Text("I have an account") }
    }
}
