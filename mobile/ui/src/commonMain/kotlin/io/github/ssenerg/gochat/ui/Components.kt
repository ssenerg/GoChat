package io.github.ssenerg.gochat.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.LinkAnnotation
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextLinkStyles
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.withLink
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import coil3.ImageLoader
import coil3.compose.AsyncImage
import coil3.compose.LocalPlatformContext
import coil3.network.NetworkHeaders
import coil3.network.httpHeaders
import coil3.request.ImageRequest
import gochat.v1.Status
import gochat.v1.User
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.model.Markup
import io.github.ssenerg.gochat.core.model.initials

/** Loads pictures from the server with the session's token. */
class Images(val loader: ImageLoader, val api: ApiClient)

val LocalImages = staticCompositionLocalOf<Images?> { null }

/** A picture the server serves only to the signed in. */
@Composable
fun ServerImage(url: String, contentDescription: String?, modifier: Modifier = Modifier, contentScale: ContentScale = ContentScale.Crop) {
    val images = LocalImages.current ?: return
    val context = LocalPlatformContext.current
    val request = remember(url, images) {
        ImageRequest.Builder(context)
            .data(url)
            .httpHeaders(NetworkHeaders.Builder().set("Authorization", "Bearer ${images.api.token ?: ""}").build())
            .build()
    }
    AsyncImage(model = request, contentDescription = contentDescription, imageLoader = images.loader, modifier = modifier, contentScale = contentScale)
}

private val avatarColors = listOf(
    Color(0xFF8B95F0), Color(0xFF6CC79E), Color(0xFFE8BB6C), Color(0xFFE48394),
    Color(0xFF7EB6E8), Color(0xFFB98BE8), Color(0xFFE89B6C), Color(0xFF6CC7C2),
)

/** A person's photo, or their initials on a color of their own. */
@Composable
fun Avatar(user: User?, size: Dp = 36.dp, modifier: Modifier = Modifier) {
    val url = LocalImages.current?.api?.userPhotoUrl(user)
    Box(modifier.size(size).clip(CircleShape), contentAlignment = Alignment.Center) {
        if (url != null) {
            ServerImage(url, contentDescription = null, modifier = Modifier.fillMaxSize())
        } else {
            val color = avatarColors[((user?.id?.hashCode() ?: 0) and 0x7fffffff) % avatarColors.size]
            Box(Modifier.fillMaxSize().background(color), contentAlignment = Alignment.Center) {
                Text(
                    user.initials(),
                    color = Color(0xFF1B1E3D),
                    fontWeight = FontWeight.SemiBold,
                    fontSize = (size.value * 0.38f).sp,
                )
            }
        }
    }
}

/** An avatar with the person's presence dot. */
@Composable
fun AvatarWithStatus(user: User?, status: Status, size: Dp = 36.dp) {
    Box {
        Avatar(user, size)
        PresenceDot(
            status,
            Modifier.align(Alignment.BottomEnd).size(size * 0.32f),
            ring = MaterialTheme.colorScheme.surface,
        )
    }
}

@Composable
fun PresenceDot(status: Status, modifier: Modifier = Modifier, ring: Color? = null) {
    val ringed = if (ring != null) modifier.border(2.dp, ring, CircleShape) else modifier
    Box(ringed.clip(CircleShape).background(statusColor(status)))
}

/** A count of unread mentions. */
@Composable
fun MentionBadge(count: Int) {
    val c = LocalChatColors.current
    Box(
        Modifier.clip(RoundedCornerShape(10.dp)).background(c.unread).padding(horizontal = 6.dp, vertical = 1.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(if (count > 99) "99+" else count.toString(), color = c.unreadInk, fontSize = 11.sp, fontWeight = FontWeight.Bold)
    }
}

/** Marks a bot's name, as the web does. */
@Composable
fun BotBadge(modifier: Modifier = Modifier) {
    Box(
        modifier.clip(RoundedCornerShape(6.dp)).background(MaterialTheme.colorScheme.primaryContainer)
            .padding(horizontal = 5.dp, vertical = 1.dp)
            .semantics(mergeDescendants = true) { contentDescription = "Bot" },
        contentAlignment = Alignment.Center,
    ) {
        Text("BOT", color = MaterialTheme.colorScheme.onPrimaryContainer, fontSize = 10.sp, fontWeight = FontWeight.Bold, letterSpacing = 0.4.sp)
    }
}

@Composable
fun BackButton(onBack: () -> Unit) {
    IconButton(onClick = onBack) {
        Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
    }
}

@Composable
fun Loading(modifier: Modifier = Modifier) {
    Box(modifier.fillMaxSize(), contentAlignment = Alignment.Center) { CircularProgressIndicator() }
}

/** A failure with a way to try again. */
@Composable
fun ErrorState(message: String, onRetry: (() -> Unit)? = null, modifier: Modifier = Modifier) {
    Column(
        modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(message, textAlign = TextAlign.Center, color = MaterialTheme.colorScheme.onSurfaceVariant)
        if (onRetry != null) TextButton(onClick = onRetry) { Text("Try again") }
    }
}

@Composable
fun EmptyState(text: String, modifier: Modifier = Modifier) {
    Box(modifier.fillMaxSize().padding(24.dp), contentAlignment = Alignment.Center) {
        Text(text, textAlign = TextAlign.Center, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/** A message's text, with its Markdown. */
@Composable
fun MessageText(content: String, modifier: Modifier = Modifier, style: TextStyle = MaterialTheme.typography.bodyMedium) {
    val blocks = remember(content) { Markup.parse(content) }
    val colors = LocalChatColors.current
    val single = blocks.singleOrNull()
    if (single is Markup.Block.Paragraph) {
        Text(inlineText(single.inlines, colors), modifier = modifier, style = style)
        return
    }
    Column(modifier, verticalArrangement = Arrangement.spacedBy(4.dp)) {
        for (block in blocks) {
            when (block) {
                is Markup.Block.Paragraph -> Text(inlineText(block.inlines, colors), style = style)
                is Markup.Block.Item -> Row {
                    Text(block.marker, style = style, modifier = Modifier.width(22.dp))
                    Text(inlineText(block.inlines, colors), style = style)
                }
                is Markup.Block.Quote -> Row(Modifier.height(IntrinsicSize.Min)) {
                    Box(Modifier.width(3.dp).fillMaxHeight().background(MaterialTheme.colorScheme.outline))
                    Text(
                        inlineText(block.inlines, colors),
                        style = style,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(start = 8.dp),
                    )
                }
                is Markup.Block.Code -> Surface(
                    color = colors.codeBackground,
                    shape = RoundedCornerShape(6.dp),
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Text(
                        block.text,
                        style = style.copy(fontFamily = FontFamily.Monospace, fontSize = 13.sp),
                        modifier = Modifier.horizontalScroll(rememberScrollState()).padding(8.dp),
                        softWrap = false,
                    )
                }
            }
        }
    }
}

private fun inlineText(inlines: List<Markup.Inline>, colors: ChatColors): AnnotatedString = buildAnnotatedString {
    fun add(list: List<Markup.Inline>) {
        for (n in list) {
            when (n) {
                is Markup.Inline.Text -> append(n.text)
                is Markup.Inline.Bold -> withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { add(n.children) }
                is Markup.Inline.Italic -> withStyle(SpanStyle(fontStyle = FontStyle.Italic)) { add(n.children) }
                is Markup.Inline.Strike -> withStyle(SpanStyle(textDecoration = TextDecoration.LineThrough)) { add(n.children) }
                is Markup.Inline.Code -> withStyle(SpanStyle(fontFamily = FontFamily.Monospace, background = colors.codeBackground)) {
                    append(n.text)
                }
                is Markup.Inline.Link -> withLink(
                    LinkAnnotation.Url(n.url, TextLinkStyles(style = SpanStyle(color = colors.link, textDecoration = TextDecoration.Underline))),
                ) { add(n.children) }
                is Markup.Inline.Mention -> withStyle(
                    SpanStyle(color = colors.mentionInk, background = colors.mention, fontWeight = FontWeight.SemiBold),
                ) { append("@" + n.name) }
            }
        }
    }
    add(inlines)
}
