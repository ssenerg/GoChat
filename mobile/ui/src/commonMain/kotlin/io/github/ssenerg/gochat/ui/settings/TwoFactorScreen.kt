package io.github.ssenerg.gochat.ui.settings

import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
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
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import gochat.v1.BeginTOTPSetupResponse
import io.github.alexzhirkevich.qrose.rememberQrCodePainter
import io.github.ssenerg.gochat.core.api.userMessage
import io.github.ssenerg.gochat.core.state.ChatSession
import io.github.ssenerg.gochat.ui.BackButton
import io.github.ssenerg.gochat.ui.LocalChatColors
import io.github.ssenerg.gochat.ui.LocalPlatform
import io.github.ssenerg.gochat.ui.Loading
import io.github.ssenerg.gochat.ui.main.LocalNavigator
import io.github.ssenerg.gochat.ui.main.LocalToasts
import kotlinx.coroutines.launch

/** Two-factor authentication with an authenticator app: setting it up, its recovery codes, and turning it off, as on the web. */
@Composable
fun TwoFactorScreen(session: ChatSession) {
    val nav = LocalNavigator.current
    val st by session.state.collectAsState()
    var loaded by remember { mutableStateOf(false) }
    var setup by remember { mutableStateOf<BeginTOTPSetupResponse?>(null) }
    var codes by remember { mutableStateOf<List<String>?>(null) }
    // Events show the person as others do, so the account says whether it is on.
    LaunchedEffect(Unit) {
        session.refreshMe()
        loaded = true
    }

    Scaffold(topBar = { TopAppBar(title = { Text("Two-factor authentication") }, navigationIcon = { BackButton { nav.pop() } }) }) { padding ->
        Column(
            Modifier.padding(padding).fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            val shown = codes
            val started = setup
            when {
                shown != null -> RecoveryCodes(shown) { nav.pop() }
                !loaded -> Loading(Modifier.fillMaxWidth().padding(32.dp))
                st.me.totp_enabled -> TurnOff(session)
                started != null -> Confirm(session, started, onCodes = { codes = it })
                else -> Begin(session, onStarted = { setup = it })
            }
        }
    }
}

@Composable
private fun Begin(session: ChatSession, onStarted: (BeginTOTPSetupResponse) -> Unit) {
    val scope = rememberCoroutineScope()
    var password by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    Text("Add a second step to signing in with an authenticator app.", color = LocalChatColors.current.muted)
    Problem(error)
    PasswordField(password) { password = it }
    Button(
        onClick = {
            busy = true
            scope.launch {
                try {
                    onStarted(session.api.beginTotp(password))
                } catch (e: Exception) {
                    error = e.userMessage()
                }
                busy = false
            }
        },
        enabled = password.isNotEmpty() && !busy,
    ) { Text("Set up") }
}

@Composable
private fun Confirm(session: ChatSession, setup: BeginTOTPSetupResponse, onCodes: (List<String>) -> Unit) {
    val scope = rememberCoroutineScope()
    val platform = LocalPlatform.current
    val toasts = LocalToasts.current
    var code by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    Text("Scan the code", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
    Text("Scan it with an authenticator app, or type the key, then enter the code it shows.", color = LocalChatColors.current.muted)
    // Dark on light whatever the theme, as scanners expect.
    Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) {
        Image(
            rememberQrCodePainter(setup.url),
            contentDescription = "QR code for your authenticator app",
            modifier = Modifier.size(216.dp).clip(RoundedCornerShape(8.dp)).background(Color.White).padding(12.dp),
        )
    }
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        SelectionContainer(Modifier.weight(1f)) {
            Text(setup.secret.chunked(4).joinToString(" "), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyLarge)
        }
        OutlinedButton(onClick = {
            platform.copy(setup.secret)
            toasts.show("Key copied.")
        }) { Text("Copy") }
    }
    Problem(error)
    OutlinedTextField(
        value = code,
        onValueChange = { v -> code = v.filter { it.isDigit() }.take(6) },
        label = { Text("Code") },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
        modifier = Modifier.fillMaxWidth(),
    )
    Button(
        onClick = {
            busy = true
            scope.launch {
                try {
                    onCodes(session.confirmTotp(code))
                } catch (e: Exception) {
                    error = e.userMessage()
                    code = ""
                }
                busy = false
            }
        },
        enabled = code.length == 6 && !busy,
    ) { Text("Turn on") }
}

@Composable
private fun RecoveryCodes(codes: List<String>, onDone: () -> Unit) {
    val platform = LocalPlatform.current
    val toasts = LocalToasts.current
    Text("Save your recovery codes", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
    Text("Two-factor authentication is on. Each code below signs you in once if you lose your device. They are shown only now.")
    SelectionContainer {
        Column(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).background(MaterialTheme.colorScheme.surfaceContainerHigh).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            codes.forEach { Text(it, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyLarge) }
        }
    }
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedButton(onClick = {
            platform.copy(codes.joinToString("\n"))
            toasts.show("Recovery codes copied.")
        }) { Text("Copy") }
        OutlinedButton(onClick = { platform.share(codes.joinToString("\n")) }) { Text("Share") }
        Button(onClick = onDone) { Text("Done") }
    }
}

@Composable
private fun TurnOff(session: ChatSession) {
    val scope = rememberCoroutineScope()
    val toasts = LocalToasts.current
    var password by remember { mutableStateOf("") }
    var code by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    Text("On", color = MaterialTheme.colorScheme.primary, fontWeight = FontWeight.SemiBold)
    Text("Signing in asks for a code from your authenticator app.")
    Problem(error)
    PasswordField(password) { password = it }
    OutlinedTextField(
        value = code,
        onValueChange = { code = it.trim().take(16) },
        label = { Text("Code or recovery code") },
        singleLine = true,
        // Recovery codes are no words to correct.
        keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.None, autoCorrectEnabled = false, keyboardType = KeyboardType.Ascii),
        modifier = Modifier.fillMaxWidth(),
    )
    Button(
        onClick = {
            busy = true
            scope.launch {
                try {
                    session.disableTotp(password, code)
                    password = ""
                    code = ""
                    error = null
                    toasts.show("Two-factor authentication is off.")
                } catch (e: Exception) {
                    error = e.userMessage()
                }
                busy = false
            }
        },
        enabled = password.isNotEmpty() && code.isNotEmpty() && !busy,
        colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error, contentColor = MaterialTheme.colorScheme.onError),
    ) { Text("Turn off") }
}

@Composable
private fun PasswordField(value: String, onChange: (String) -> Unit) {
    OutlinedTextField(
        value = value,
        onValueChange = { onChange(it.take(256)) },
        label = { Text("Password") },
        singleLine = true,
        visualTransformation = PasswordVisualTransformation(),
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
        modifier = Modifier.fillMaxWidth(),
    )
}

@Composable
private fun Problem(error: String?) {
    if (error != null) Text(error, color = MaterialTheme.colorScheme.error)
}
