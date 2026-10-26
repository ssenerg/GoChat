# The GoChat protocol

The GoChat protocol is how native apps and bots talk to a GoChat server. It
plays the part MTProto plays for Telegram: one connection carries calls of
the chat API, their results and the caller's updates, and every update is
numbered, so a client that was away asks for exactly what it missed. People
and bots speak it the same way; bots sign in with a bot token and have
fewer calls open to them.

Browsers do not speak it: the web pages are rendered by the web gateway,
which speaks DRPC to the server. The protocol is served by the web gateway
too, so it sits behind the same TLS terminating proxy.

The frames are protobuf messages in
[`protocol.proto`](../api/protobuf/proto/gochat/v1/protocol.proto); the calls
are those of the chat API's services in the same directory. This document
says what a client does with them.

- [Connecting](#connecting)
- [Calls](#calls)
- [Updates](#updates)
- [Catching up](#catching-up)
- [Bots](#bots)
- [The HTTP Bot API](#the-http-bot-api)
- [Webhooks](#webhooks)
- [Layers](#layers)

## Connecting

Open a WebSocket to `wss://<server>/api/v2/socket` with the subprotocol
`gochat.v2`. Every message either way is binary and holds one frame: `Up`
from the client, `Down` from the server. Text messages close the socket.

The first frame must be `Up.hello`:

| Field | |
| --- | --- |
| `token` | A session token (from `AuthService.Login`, `VerifyMFA` or `SignUp`) or a bot token. |
| `client` | The client and its version, such as `GoChat Android 0.5`; it names the session in the session list. |
| `layer` | The protocol layer the client speaks: `1`. |
| `pts` | The last account update the client applied, to resume after it; `0` to start from now. |

The server answers `Down.welcome` once the token checks out:

| Field | |
| --- | --- |
| `user` | The caller. `user.bot` is set for a bot. |
| `session_id` | The session's public id. |
| `expires_at` | When the session ends, and the socket with it. A bot's renews every day; it reconnects then. |
| `pts` | The account update number the stream continues from. |
| `layer` | The layer the server speaks. |
| `reload` | The client's `pts` is older than the server keeps: reload what it shows, then take `pts` as the state. |

Calls may follow the hello at once. The server sends WebSocket pings; a
client may also send `Up.ping` and gets `Down.pong` with the same id.

A hello without a token opens an anonymous socket, to sign in over: its
welcome names no user, and it may only call `AuthService.GetSignupInfo`,
`SignUp`, `Login` and `VerifyMFA`, and `TeamService.GetInvite`. Once signed
in, the client says hello again on the same socket, with the session's
token.

A socket the server closes with a code of 4000 or more says why:

| Code | Meaning | What to do |
| --- | --- | --- |
| 4000 | The server could not attach the socket. | Reconnect after a backoff. |
| 4001 | The token is not valid. | Sign in again; a bot's token was reset or the bot deleted. |
| 4002 | A frame broke the protocol (no hello first, an unknown frame, a text message). | Fix the client. |
| 4003 | The session was revoked or expired. | Sign in again; a bot reconnects with its token. |
| 4008 | The account holds too many sockets open. | Close another. |
| 4009 | The client fell too far behind reading. | Reconnect with its `pts`. |
| 4011 | The client's layer is older than the server takes. | Update the client. |

Reconnect with backoff and jitter, and always send the last `pts` applied.

## Calls

`Up.call` makes one call of the chat API:

```
Call { id: 7, method: "gochat.v1.MessageService/PostMessage", payload: <PostMessageRequest bytes> }
```

and `Down.result` answers it with the same id, carrying the response's bytes
or an `ApiError`:

| Code | Meaning |
| --- | --- |
| 3 | Invalid argument |
| 5 | Not found |
| 6 | Already exists |
| 7 | Permission denied |
| 8 | Resource exhausted: too many calls, or a limit reached |
| 9 | Failed precondition |
| 12 | Unimplemented: no such call, or not open to the caller |
| 13 | Internal |
| 14 | Unavailable: retry later |
| 16 | Unauthenticated |

Ids are the client's to choose, unique among its calls in flight. Results
come in any order. A socket has at most 32 calls in flight; more wait
their turn. Calls spend the session's request budget, as the HTTP API's do.

People may make the unary calls the apps make over `/api/v1` (see
[`internal/web/api.go`](../internal/web/api.go)), plus `UpdatesService` and
the people's calls of `BotService`. Files and photos still move over HTTP:
`POST /api/v2/files?channel_id=&name=` and `GET /api/v2/files/<id>` take the
same bearer token as `/api/v1/files`. The calls open to bots are
[listed below](#what-bots-may-call).

## Updates

The server sends what happens to the caller as updates, each an `Event` of
[`gateway.proto`](../api/protobuf/proto/gochat/v1/gateway.proto). They come
in two kinds.

**Numbered updates** come in `Down.updates`, oldest first. Each belongs to a
sequence and carries its number:

- **The account's sequence.** Things that happen to the caller: joining or
  leaving a channel or a team, muting a channel, saving a message, the
  voice channels of the caller's teams. A bot's account sequence also holds
  everything it receives from its channels, and the button presses on its
  messages. These carry `pts` and `pts_count` and no `pts_channel_id`.
- **A channel's sequence.** Things that happen in a channel a person
  belongs to: messages posted, edited and deleted, reactions, pins, and
  changes to the channel itself. These carry `pts`, `pts_count` and
  `pts_channel_id`, and each channel numbers its own. `Channel.pts`, in
  `ListChannels`, `GetChannel` and `ChannelJoined`, is the number of the
  channel's latest update.

A sequence goes up by `pts_count` with each update; today `pts_count` is
always 1. Numbers may skip after a server restart: a skipped number holds
nothing. For each sequence the client keeps the number of the last update
it applied, its *local* number, and for each update:

- `local + pts_count == pts`: apply it, and set `local = pts`.
- `local + pts_count > pts`: it is already applied; drop it.
- `local + pts_count < pts`: updates are missing. Hold this one, wait half
  a second for the missing ones to arrive, then ask for the difference
  (below) and apply what it returns.

A channel update for a channel the client does not know is dropped: the
client learns the channel's number with `ChannelJoined` or `ListChannels`.
A `ChannelJoined` may arrive after the first update of that channel, so on
`ChannelJoined` the client takes `channel.pts` as the channel's number and
asks for the channel's difference from it.

**Ephemeral updates** come in `Down.ephemeral` and carry no number: typing,
presence, profile changes, voice states, and the answer to a button the
client pressed. They are never replayed. A client that reconnects asks for
presence again (`UserService.GetPresence`) and voice states
(`VoiceService.ListVoiceStates`) for what it shows. Typing goes only to
sockets that said they show the channel (`Up.view`), presence and profile
changes only to sockets that said they show the person (`Up.watch`).

## Catching up

**The account's sequence catches up by itself.** When the hello carries a
`pts`, the server replays the updates after it before any new one, and the
welcome's `pts` is the hello's: the client applies what follows as usual.
When the server reconnects the socket on its side, it sends a welcome again
and replays what the client missed meanwhile. When the replay would be too
long, the welcome has `reload` set: the client loads what it shows again
(teams, channels, the open channel's messages) and takes the welcome's
`pts` as its number.

A client may also ask itself:

- `UpdatesService.GetState()`: the account's latest number.
- `UpdatesService.GetDifference(pts, limit)`: the account updates after
  `pts`, at most `limit` (default and maximum 100), with the new state. When
  `more` is set, ask again from the returned `pts`. When `too_long` is set,
  reload and take the returned `pts`.

**Channels catch up when the client asks.** A person's client asks for the
channels it shows, when it sees a gap, after a reconnect and after
`ChannelJoined`:

- `UpdatesService.GetChannelDifference(channel_id, pts, limit)`: the
  channel's updates after `pts`, with the same `more` and `too_long`.

A client in many channels need not catch up on all of them: `ListChannels`
gives each channel's `pts` and `last_message_id`, enough for unread marks,
and a channel is caught up when it is opened.

Replayed messages are as they are now: a message deleted since is left out
(its `MessageDeleted` follows), an edited one comes edited. The server keeps
updates for seven days by default (`services.updates.retention`).

## Bots

A bot is an account a person owns. People make bots in the web's settings,
or with `BotService.CreateBot`, which answers the bot and its token once:

```
b_3Hq2...   (b_ and 43 characters)
```

The token is the bot's only credential: keep it secret. Only its hash is
stored. Resetting it (`ResetBotToken`) stops the old one at once and
closes the bot's sockets.

A bot connects as a person does, with its token in the hello, and receives
**everything in its account's sequence**: a bot keeps one number and asks
for nothing else. Its owner adds it to teams they may invite people to;
then people add it to channels as they add anyone (a private channel's
admins only), and anyone in its teams may write to it directly.

### What a bot receives

By default a bot is in **privacy mode** and receives, from its channels:

- messages that start with `/`, its commands;
- messages that mention it (`@its_username`);
- replies in threads it posted in;
- every message of its direct conversations;
- edits, deletions and reactions of those messages;
- `CallbackQuery` when someone presses a button of its messages;
- `ChannelJoined` and `ChannelLeft` as it is added to channels and removed.

With `sees_all` on (`UpdateBot`), a bot added to a channel from then on
receives every message there and may read its history. A channel keeps
what the bot had when it was added; add it again to change it.

### What bots may call

| Service | Calls |
| --- | --- |
| `AuthService` | `Me` |
| `UserService` | `GetUser`, `UpdateProfile` |
| `TeamService` | `ListTeams`, `GetTeam`, `ListTeamMembers` |
| `ChannelService` | `ListChannels`, `GetChannel`, `ListChannelMembers`, `JoinChannel`, `LeaveChannel` |
| `MessageService` | `PostMessage`, `EditMessage`, `DeleteMessage` (its own), `GetMessage`, `ListMessages`, `ListThread` (with `sees_all`), `SendTyping`, `React` |
| `UpdatesService` | `GetState`, `GetDifference` |
| `BotService` | `SetCommands`, `GetCommands`, `AnswerCallback`, `SetWebhook`, `DeleteWebhook`, `GetWebhookInfo`, `ConfirmUpdates` |
| `FileService` | uploads and downloads, at `/api/v2/files` |

Any other call answers code 12.

### Commands

`SetCommands` lists the commands a bot offers, up to 100, each a name of
1–32 lowercase letters, digits and underscores, and a description. People
see them suggested when they type `/` in a channel with the bot. A command
is an ordinary message, `/weather Tehran`, visible to the channel, and
reaches every bot in it, in privacy mode too; with several bots in a
channel people may write `/weather@weather_bot`, and a bot ignores commands
addressed to another.

### Buttons

A bot puts buttons under its messages with `markup` in `PostMessage` or
`EditMessage`: up to 8 rows of up to 8 buttons, each with a text of up to
64 characters and either `callback_data` (up to 64 bytes) or an `url`.
Editing a message replaces its buttons; leaving `markup` out removes them.

Pressing a `callback_data` button (`MessageService.PressButton`) sends the
bot a `CallbackQuery` with the presser, the message and the data. The bot
answers it within a minute with `AnswerCallback(query_id, text, alert)`:
the presser sees `text` as a passing notice, or as an alert with `alert`.
The bot may also edit the message, to change its buttons.

## The HTTP Bot API

For bots that would rather not keep a socket, the web gateway serves the
same calls over HTTP:

```
POST /api/v2/bot/<method>
Authorization: Bearer <bot token>
Content-Type: application/json
```

The body is the call's request in JSON (protobuf's JSON mapping, with the
proto field names), and the answer is always JSON:

```json
{"ok": true, "result": { ... }}
{"ok": false, "error_code": 7, "description": "permission denied"}
```

| Method | Call |
| --- | --- |
| `getMe` | `AuthService/Me` |
| `getUpdates` | `UpdatesService/GetDifference`, see below |
| `sendMessage` | `MessageService/PostMessage` |
| `editMessage` | `MessageService/EditMessage` |
| `deleteMessage` | `MessageService/DeleteMessage` |
| `getMessage` | `MessageService/GetMessage` |
| `sendTyping` | `MessageService/SendTyping` |
| `react` | `MessageService/React` |
| `answerCallback` | `BotService/AnswerCallback` |
| `setCommands` / `getCommands` | `BotService/SetCommands` / `GetCommands` |
| `listTeams` | `TeamService/ListTeams` |
| `listChannels` / `getChannel` | `ChannelService/ListChannels` / `GetChannel` |
| `setWebhook` / `deleteWebhook` / `getWebhookInfo` | `BotService/...` |

Any call of the [table above](#what-bots-may-call) may also be made by its
full name: `POST /api/v2/bot/gochat.v1.TeamService/ListTeamMembers`.

`getUpdates` takes `{"offset": N, "limit": L, "timeout": T}`. `offset` is
the number of the first update wanted, the last `pts` handled plus one;
updates before it are confirmed, and later calls without an offset start
there. When nothing is waiting, the call waits up to `timeout` seconds (at
most 50) for an update. The result is `{"updates": [...], "pts": N}`, each
update with its `pts`. A second `getUpdates` while one waits ends the first
with error 409. While a webhook is set, `getUpdates` answers 409.

## Webhooks

`setWebhook {"url": "https://...", "secret": "..."}` has the server deliver
the bot's updates itself: each update, in order, as one
`POST <url>` with the update's JSON as the body and the header
`X-GoChat-Bot-Secret: <secret>`. A 2xx answer delivers it; anything else,
or no answer within 10 seconds, is retried with a growing delay, and the
next update waits. A new webhook gets the updates after it was set; moving
it to another URL keeps its place. `getWebhookInfo` tells the last
delivered `pts`, how many wait, and the last error.

Webhook URLs must be https and reach a public address: the server refuses
loopback, private, link-local and other internal addresses, at every
delivery, and follows no redirects.

## Layers

The layer is the protocol's version. Within a layer, fields and calls are
only added, and clients ignore what they do not know. A change that would
break a client takes a new layer; the server keeps speaking the older ones
for a while, and closes clients older than that with 4011.

| Layer | |
| --- | --- |
| 1 | This document. |
