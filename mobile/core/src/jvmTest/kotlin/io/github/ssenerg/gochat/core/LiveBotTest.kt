package io.github.ssenerg.gochat.core

import gochat.v1.AddBotToTeamRequest
import gochat.v1.AddBotToTeamResponse
import gochat.v1.AnswerCallbackRequest
import gochat.v1.AnswerCallbackResponse
import gochat.v1.BotCommand
import gochat.v1.Button
import gochat.v1.ButtonRow
import gochat.v1.CreateBotRequest
import gochat.v1.CreateBotResponse
import gochat.v1.DeleteBotRequest
import gochat.v1.DeleteBotResponse
import gochat.v1.EditMessageRequest
import gochat.v1.EditMessageResponse
import gochat.v1.Event
import gochat.v1.Markup
import gochat.v1.PostMessageRequest
import gochat.v1.PostMessageResponse
import gochat.v1.SetCommandsRequest
import gochat.v1.SetCommandsResponse
import io.github.ssenerg.gochat.core.api.ApiClient
import io.github.ssenerg.gochat.core.api.GoChatApi
import io.github.ssenerg.gochat.core.socket.SocketEvent
import io.github.ssenerg.gochat.core.state.AppState
import io.github.ssenerg.gochat.core.state.BotAnswer
import io.github.ssenerg.gochat.core.state.GoChatApp
import io.github.ssenerg.gochat.core.state.MemoryStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlin.random.Random
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

/**
 * A bot, written with the app's own core, and a person's app: commands,
 * buttons and the bot's answers. Runs against a stack, as LiveServerTest.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class LiveBotTest {
    private val url = System.getenv("GOCHAT_E2E_URL")
    private val admin = System.getenv("GOCHAT_E2E_ADMIN")
    private val password = System.getenv("GOCHAT_E2E_ADMIN_PASSWORD")

    @Test
    fun botsButtonsReachTheirBotAndAnswerThePresser() {
        if (url == null || admin == null || password == null) {
            println("GOCHAT_E2E_URL is not set: skipping")
            return
        }
        runBlocking {
            val main = Dispatchers.Default.limitedParallelism(1)
            val scope = CoroutineScope(SupervisorJob() + main)
            try {
                withTimeout(60_000) { run(scope) }
            } finally {
                scope.cancel()
            }
        }
    }

    private suspend fun run(scope: CoroutineScope) {
        val app = GoChatApp(MemoryStore(), scope)
        assertNull(withContext(scope.coroutineContext) { app.signIn(url!!, admin!!, password!!) })
        val session = (app.state.value as AppState.SignedIn).session
        val st = session.state.first { !it.loading && it.channels.isNotEmpty() }
        val town = st.textChannels.first { it.name == "town-square" }

        // The person makes a bot and adds it to the team and the channel.
        val name = "wx" + Random.nextInt(1_000_000) + "_bot"
        val made = session.api.client.call(
            "gochat.v1.BotService/CreateBot",
            CreateBotRequest(username = name, display_name = "Weather"),
            CreateBotResponse.ADAPTER,
        )
        val botUser = made.bot!!.user!!
        session.api.client.call("gochat.v1.BotService/AddBotToTeam", AddBotToTeamRequest(bot_id = botUser.id, team_id = st.teamId!!), AddBotToTeamResponse.ADAPTER)
        session.api.addChannelMember(town.id, botUser.id)

        // The bot connects with its token, over the same socket the apps use.
        val bot = GoChatApi(ApiClient(url!!, made.token))
        val heard = MutableSharedFlow<Event>(replay = 64)
        scope.launch { for (e in bot.client.socket.events) if (e is SocketEvent.Updates) e.events.forEach { heard.emit(it) } }
        bot.client.socket.start()
        bot.client.call(
            "gochat.v1.BotService/SetCommands",
            SetCommandsRequest(commands = listOf(BotCommand(name = "weather", description = "The weather somewhere"))),
            SetCommandsResponse.ADAPTER,
        )
        try {
            // The composer suggests the bot's commands.
            val suggested = withContext(scope.coroutineContext) { session.commands(town.id, "wea") }
            assertTrue(suggested.any { it.command?.name == "weather" && it.bot?.id == botUser.id }, suggested.toString())

            // A command reaches the bot, which answers with buttons.
            val timeline = withContext(scope.coroutineContext) { session.timeline(town.id) }
            timeline.state.first { !it.loading }
            withContext(scope.coroutineContext) { timeline.send("/weather Tehran") }
            val command = heard.first { it.message_posted?.message?.content == "/weather Tehran" }.message_posted!!.message!!
            val markup = Markup(
                rows = listOf(
                    ButtonRow(buttons = listOf(Button(text = "Refresh", callback_data = "refresh"), Button(text = "Forecast", url = "https://example.com/forecast"))),
                ),
            )
            val reply = bot.client.call(
                "gochat.v1.MessageService/PostMessage",
                PostMessageRequest(channel_id = command.channel_id, content = "Sunny in Tehran", markup = markup),
                PostMessageResponse.ADAPTER,
            ).message!!
            val shown = timeline.state.first { s -> s.messages.any { it.id == reply.id } }.messages.first { it.id == reply.id }
            assertTrue(shown.author!!.bot, "the author is a bot")
            assertEquals(listOf("Refresh", "Forecast"), shown.markup!!.rows[0].buttons.map { it.text })
            assertEquals("https://example.com/forecast", shown.markup!!.rows[0].buttons[1].url)

            // A press reaches the bot; its answer reaches the presser, and its
            // edit changes the buttons.
            val answers = MutableSharedFlow<BotAnswer>(replay = 4)
            scope.launch { session.answers.collect { answers.emit(it) } }
            assertNull(withContext(scope.coroutineContext) { session.press(shown, "refresh") })
            val query = heard.first { it.callback_query != null }.callback_query!!
            assertEquals("refresh", query.data_)
            assertEquals(session.me.id, query.from?.id)
            bot.client.call("gochat.v1.BotService/AnswerCallback", AnswerCallbackRequest(query_id = query.id, text = "Still sunny"), AnswerCallbackResponse.ADAPTER)
            assertEquals(BotAnswer("Weather", "Still sunny", alert = false), answers.first())
            bot.client.call(
                "gochat.v1.MessageService/EditMessage",
                EditMessageRequest(
                    channel_id = reply.channel_id, message_id = reply.id, content = "Sunny in Tehran, 31°",
                    markup = Markup(rows = listOf(ButtonRow(buttons = listOf(Button(text = "Done", callback_data = "done"))))),
                ),
                EditMessageResponse.ADAPTER,
            )
            timeline.state.first { s -> s.messages.first { it.id == reply.id }.markup?.rows?.firstOrNull()?.buttons?.map { it.text } == listOf("Done") }

            // An alert is an alert.
            assertNull(withContext(scope.coroutineContext) { session.press(shown, "done") })
            val second = heard.first { it.callback_query?.data_ == "done" }.callback_query!!
            bot.client.call("gochat.v1.BotService/AnswerCallback", AnswerCallbackRequest(query_id = second.id, text = "Bye", alert = true), AnswerCallbackResponse.ADAPTER)
            assertEquals(BotAnswer("Weather", "Bye", alert = true), answers.first { it.text == "Bye" })
        } finally {
            bot.client.close()
            session.api.client.call("gochat.v1.BotService/DeleteBot", DeleteBotRequest(bot_id = botUser.id), DeleteBotResponse.ADAPTER)
            withContext(scope.coroutineContext) { app.signOut() }
        }
    }
}
