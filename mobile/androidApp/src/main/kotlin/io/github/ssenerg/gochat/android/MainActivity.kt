package io.github.ssenerg.gochat.android

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.ui.AndroidPush
import io.github.ssenerg.gochat.ui.GoChatRoot

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        val application = application as GoChatApplication
        setContent { GoChatRoot(application.app, application.platform) }
        if (savedInstanceState == null) opened(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        opened(intent)
    }

    /** Opens what a tapped push shows, or finishes a sign-in the browser came back from. */
    private fun opened(intent: Intent?) {
        val app = (application as GoChatApplication).app
        val data = intent?.data
        if (data?.scheme == GoChatApp.SSO_SCHEME) {
            app.ssoReturned(data.toString())
            return
        }
        AndroidPush.target(this, intent)?.let { app.openPush(it) }
    }
}
