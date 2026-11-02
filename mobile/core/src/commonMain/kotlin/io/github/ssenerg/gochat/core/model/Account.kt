package io.github.ssenerg.gochat.core.model

import gochat.v1.GetStorageUsageResponse
import gochat.v1.SSOIdentity
import gochat.v1.SessionInfo
import kotlinx.datetime.TimeZone

// The account's sessions, sign-ins and storage, as the web's settings show them.

/** The browser or device a session's User-Agent names, as the web's session list does. */
fun deviceName(userAgent: String): String {
    // The apps say "GoChat/0.2.0 (Android 16; Pixel 8)" or "GoChat/0.2.0 (iOS 18.4; iPhone)".
    if (userAgent.startsWith("GoChat/")) {
        val about = userAgent.substringAfter('(', "").substringBefore(')')
        val system = about.substringBefore(';').trim().substringBefore(' ')
        val model = if (';' in about) about.substringAfter(';').trim() else ""
        return when {
            model.isNotEmpty() && model.length <= 40 -> "GoChat app on $model"
            system.isNotEmpty() && system.length <= 20 -> "GoChat app on $system"
            else -> "GoChat app"
        }
    }
    val browser = when {
        "Edg" in userAgent -> "Edge"
        "OPR/" in userAgent -> "Opera"
        "Firefox/" in userAgent || "FxiOS/" in userAgent -> "Firefox"
        "Chrome/" in userAgent || "CriOS/" in userAgent -> "Chrome"
        "Safari/" in userAgent -> "Safari"
        else -> ""
    }
    val system = when {
        "iPhone" in userAgent -> "iPhone"
        "iPad" in userAgent -> "iPad"
        "Android" in userAgent -> "Android"
        "Windows" in userAgent -> "Windows"
        "CrOS" in userAgent -> "ChromeOS"
        "Macintosh" in userAgent || "Mac OS X" in userAgent -> "macOS"
        "Linux" in userAgent -> "Linux"
        else -> ""
    }
    return when {
        browser.isNotEmpty() && system.isNotEmpty() -> "$browser on $system"
        browser.isNotEmpty() -> browser
        system.isNotEmpty() -> system
        else -> {
            val name = userAgent.substringBefore('/')
            if (name.isNotEmpty() && name.length <= 40 && name.none { it in " ;(" }) name else "Unknown device"
        }
    }
}

private fun com.squareup.wire.Instant.millis(): Long = getEpochSecond() * 1000 + getNano() / 1_000_000

/** A day in a sentence: today, yesterday, Monday, October 21... */
private fun onDay(ms: Long, nowMs: Long, tz: TimeZone): String = when (val d = Format.day(ms, nowMs, tz)) {
    "Today", "Yesterday" -> d.lowercase()
    else -> d
}

/** The address a session was last seen at. */
val SessionInfo.address: String get() = last_ip.ifEmpty { ip }

/** When the session was last seen: "Active now" for this one. */
fun SessionInfo.seen(nowMs: Long, tz: TimeZone = TimeZone.currentSystemDefault()): String {
    if (current) return "Active now"
    val at = last_seen_at?.millis() ?: return "Not seen since signing in"
    return "Last seen ${onDay(at, nowMs, tz)} at ${Format.clock(at, tz)}"
}

/** When the session signed in, and from where when it has moved since. */
fun SessionInfo.signedIn(nowMs: Long, tz: TimeZone = TimeZone.currentSystemDefault()): String {
    val at = created_at?.millis() ?: return ""
    val from = if (last_ip.isNotEmpty() && last_ip != ip) " from $ip" else ""
    return "Signed in ${onDay(at, nowMs, tz)}$from"
}

/** The account's email at the provider and when it was linked: "ann@corp.test · linked Monday". */
fun SSOIdentity.about(nowMs: Long, tz: TimeZone = TimeZone.currentSystemDefault()): String {
    val linked = linked_at?.let { "linked ${onDay(it.millis(), nowMs, tz)}" }
    return listOfNotNull(email.ifEmpty { null }, linked).joinToString(" · ")
}

/**
 * Whether the person may unlink one of [linked]: the server keeps an
 * account without a password its last one, its only way to sign in.
 */
fun canUnlink(linked: List<SSOIdentity>, hasPassword: Boolean): Boolean = hasPassword || linked.size > 1

/** The space used, against the quota when there is one: "3.0 MB of 10 MB used". */
fun GetStorageUsageResponse.describe(): String =
    if (quota > 0) "${Format.size(used)} of ${Format.size(quota)} used" else "${Format.size(used)} used"

/** How much of the quota is used, from 0 to 1, or null without one. */
val GetStorageUsageResponse.fraction: Float? get() = if (quota > 0) (used.toDouble() / quota).coerceIn(0.0, 1.0).toFloat() else null
