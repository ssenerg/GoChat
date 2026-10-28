package io.github.ssenerg.gochat.android

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
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

    /** Opens what a tapped push shows. */
    private fun opened(intent: Intent?) {
        AndroidPush.target(intent)?.let { (application as GoChatApplication).app.openPush(it) }
    }
}
