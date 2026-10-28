package io.github.ssenerg.gochat.core.api

import io.ktor.http.parseQueryString
import okio.ByteString.Companion.encodeUtf8
import kotlin.io.encoding.Base64

private val base64Url = Base64.UrlSafe.withPadding(Base64.PaddingOption.ABSENT)

/** Random bytes from the platform's secure generator. */
internal expect fun secureRandomBytes(size: Int): ByteArray

/**
 * A PKCE verifier and its S256 challenge (RFC 7636): a sign-in the browser
 * finishes hands its code only to whoever holds the verifier.
 */
class Pkce(val verifier: String) {
    val challenge: String get() = base64Url.encode(verifier.encodeUtf8().sha256().toByteArray())

    companion object {
        fun create() = Pkce(base64Url.encode(secureRandomBytes(32)))
    }
}

/** What a sign-in in the browser came back to the app with: a code to redeem, or why not. */
data class SsoReturn(val code: String?, val error: String?)

/** Reads the redirect a sign-in came back to, or null when it is not one. */
fun parseSsoReturn(callback: String, redirect: String): SsoReturn? {
    if (!callback.startsWith(redirect) || callback.getOrNull(redirect.length).let { it != null && it != '?' }) return null
    val q = parseQueryString(callback.substringAfter('?', ""))
    val code = q["code"]?.takeIf { it.isNotEmpty() }
    val error = q["error"]?.takeIf { it.isNotEmpty() }
    return if (code == null && error == null) null else SsoReturn(code, error)
}

