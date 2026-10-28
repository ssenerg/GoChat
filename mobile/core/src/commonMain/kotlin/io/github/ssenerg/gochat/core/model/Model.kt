package io.github.ssenerg.gochat.core.model

import gochat.v1.Channel
import gochat.v1.ChannelRole
import gochat.v1.ChannelType
import gochat.v1.Message
import gochat.v1.Status
import gochat.v1.Team
import gochat.v1.TeamMember
import gochat.v1.TeamRole
import gochat.v1.User
import gochat.v1.SystemRole

// What the web's render package works out, for the apps.

fun User?.displayName(): String = when {
    this == null -> "Unknown"
    display_name.isNotEmpty() -> display_name
    else -> username
}

fun User?.initials(): String {
    val words = displayName().split(' ', '-', '_', '.').filter { it.isNotEmpty() }
    return when {
        words.isEmpty() -> "?"
        words.size == 1 -> words[0].take(2).uppercase()
        else -> (words[0].take(1) + words[1].take(1)).uppercase()
    }
}

val Channel.isConversation: Boolean
    get() = type == ChannelType.CHANNEL_TYPE_DIRECT || type == ChannelType.CHANNEL_TYPE_GROUP

val Channel.isDirect: Boolean get() = type == ChannelType.CHANNEL_TYPE_DIRECT

val Channel.isVoice: Boolean get() = type == ChannelType.CHANNEL_TYPE_VOICE

val Channel.isPrivate: Boolean get() = type == ChannelType.CHANNEL_TYPE_PRIVATE

val Channel.isArchived: Boolean get() = archived_at != null

/** Whether the caller belongs to the channel. */
val Channel.isMember: Boolean get() = role != ChannelRole.CHANNEL_ROLE_UNSPECIFIED || isConversation

/** The channel's name as the sidebar shows it. */
fun Channel.title(): String = when {
    peer != null -> peer.displayName()
    peers.isNotEmpty() -> peers.joinToString(", ") { it.displayName() }
    else -> display_name
}

/** Whether messages arrived since the caller last read the channel. */
fun Channel.isUnread(): Boolean = when {
    last_message_id.isEmpty() -> false
    last_read_id.isEmpty() -> true
    else -> newer(last_message_id, last_read_id)
}

/** Whether the caller moderates the channel: its admin, or the team's, or the system's. */
fun Channel.moderatedBy(me: User, team: Team?): Boolean =
    role == ChannelRole.CHANNEL_ROLE_ADMIN || team?.role == TeamRole.TEAM_ROLE_ADMIN || me.role == SystemRole.SYSTEM_ROLE_ADMIN

/** Up to two letters for a team without a photo. */
fun Team.initials(): String =
    display_name.split(' ').filter { it.isNotEmpty() }.take(2).joinToString("") { it.take(1) }.uppercase().ifEmpty { "?" }

/** Whether the caller administers the team, as the web's team settings ask. */
val Team.isAdmin: Boolean get() = role == TeamRole.TEAM_ROLE_ADMIN

/**
 * Whether the caller may change [m]'s role or remove them, as the server
 * allows: another admin only the team's owner may, the owner no one else.
 */
fun Team.canChange(me: User, m: TeamMember): Boolean {
    val id = m.user?.id ?: return false
    val system = me.role == SystemRole.SYSTEM_ROLE_ADMIN
    return when {
        !(isAdmin || system) || id == me.id -> false
        system -> true
        id == owner_id -> false
        m.role == TeamRole.TEAM_ROLE_ADMIN -> me.id == owner_id
        else -> true
    }
}

/** The largest picture the server takes for a photo. */
const val MAX_PHOTO_BYTES = 10 * 1024 * 1024

/** Why the server would refuse [picture] as a photo, in its words, or null: it takes JPEG, PNG, GIF and WebP up to 10 MB. */
fun photoProblem(picture: ByteArray): String? {
    fun starts(vararg b: Int, at: Int = 0) = picture.size >= at + b.size && b.indices.all { picture[at + it] == b[it].toByte() }
    fun startsText(t: String, at: Int = 0) = starts(*t.map { it.code }.toIntArray(), at = at)
    val known = starts(0xFF, 0xD8) || starts(0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A) ||
        startsText("GIF87a") || startsText("GIF89a") || (startsText("RIFF") && startsText("WEBPVP8", at = 8))
    return when {
        picture.isEmpty() -> "Choose a picture."
        picture.size > MAX_PHOTO_BYTES -> "The picture is too large, use one under 10 MB and 24 megapixels."
        !known -> "Use a JPEG, PNG, GIF or WebP picture."
        else -> null
    }
}

/** The time of a version 1 (time) UUID, in 100ns since 1582, or 0. */
fun uuidTime(id: String): Long {
    if (id.length != 36) return 0
    val hex = id.substring(15, 18) + id.substring(9, 13) + id.substring(0, 8)
    return hex.toLongOrNull(16) ?: 0
}

/** Whether time UUID [a] is later than [b]; anything is later than an empty [b]. */
fun newer(a: String, b: String): Boolean = b.isEmpty() || uuidTime(a) > uuidTime(b)

/** When a message was sent, in milliseconds since 1970. */
/** Whether the person does not want to be disturbed now: until dnd_until, when set. */
fun User.inDnd(nowMillis: Long): Boolean =
    status == Status.STATUS_DND && dnd_until.let { it == null || it.getEpochSecond() * 1000 + it.getNano() / 1_000_000 > nowMillis }

fun Message.sentAtMillis(): Long =
    created_at?.let { it.getEpochSecond() * 1000 + it.getNano() / 1_000_000 }
        ?: (uuidTime(id) - UUID_EPOCH_OFFSET) / 10_000

// 100ns intervals between the UUID epoch (1582-10-15) and 1970-01-01.
private const val UUID_EPOCH_OFFSET = 0x01B21DD213814000L

/** The status to show for a person: offline when not connected. */
fun shownStatus(online: Boolean, status: Status?): Status = when {
    !online -> Status.STATUS_OFFLINE
    status == null || status == Status.STATUS_UNSPECIFIED -> Status.STATUS_ONLINE
    else -> status
}

private const val EXCERPT_LENGTH = 140

/** A message's text, short and on one line, for a notice or a list. */
fun Message.excerpt(): String {
    var text = content.split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")
    if (text.length > EXCERPT_LENGTH) text = text.take(EXCERPT_LENGTH) + "…"
    if (text.isEmpty()) {
        text = when {
            attachments.size == 1 && attachments[0].voice -> "Sent a voice message"
            attachments.size == 1 -> "Sent a file"
            attachments.size > 1 -> "Sent ${attachments.size} files"
            else -> ""
        }
    }
    return text
}

/** What a message calls out: names, and everyone in the channel or online. */
data class Mentions(val usernames: List<String> = emptyList(), val channel: Boolean = false, val here: Boolean = false) {
    fun of(username: String) = username.lowercase() in usernames
}

private val fenced = Regex("(?s)(```|~~~).*?(```|~~~|$)")
private val inlineCode = Regex("`[^`\n]*`")

private fun isNameChar(c: Char) = c in 'a'..'z' || c in 'A'..'Z' || c in '0'..'9' || c == '.' || c == '_' || c == '-'

/** Finds the mentions of a message's text outside code, as the server does. */
fun parseMentions(input: String): Mentions {
    if ('@' !in input) return Mentions()
    val text = inlineCode.replace(fenced.replace(input, " "), " ")
    val names = LinkedHashSet<String>()
    var channel = false
    var here = false
    var i = 0
    while (i < text.length) {
        if (text[i] != '@' || (i > 0 && (isNameChar(text[i - 1]) || text[i - 1] == '@'))) {
            i++
            continue
        }
        var j = i + 1
        while (j < text.length && isNameChar(text[j])) j++
        val name = text.substring(i + 1, j).trimEnd('.', '_', '-').lowercase()
        when {
            name.isEmpty() -> {}
            name == "channel" || name == "all" -> channel = true
            name == "here" -> here = true
            name.length <= 32 && names.size < 20 -> names += name
        }
        i = j
    }
    return Mentions(names.toList(), channel, here)
}

/** Whether a message calls out [me], by name or as one of everyone. */
fun Message.mentions(me: User): Boolean {
    val found = parseMentions(content)
    return found.channel || found.here || found.of(me.username)
}
