# Building GoChat bots

A bot is a GoChat account that a program signs in to with a token. It
receives the messages meant for it as updates and answers with the same calls
the apps make. This guide takes a bot from nothing to production;
[protocol.md](protocol.md#bots) is the specification it leans on.

- [Making a bot](#making-a-bot)
- [A first call](#a-first-call)
- [Getting updates](#getting-updates)
- [Commands](#commands)
- [Buttons](#buttons)
- [What a bot sees](#what-a-bot-sees)
- [Errors](#errors)
- [Limits](#limits)
- [What a bot may not call](#what-a-bot-may-not-call)
- [Running a bot](#running-a-bot)
- [The examples](#the-examples)

## Making a bot

1. On the web, open **Settings → Bots**. Under **New bot**, give it a
   username ending in `bot`, such as `weather_bot`, and a display name, and
   press **Create bot**.
2. The page shows the bot's token, `b_` and 43 characters, once. Copy it.
   The server keeps only its hash: a lost token cannot be shown again, only
   reset.
3. Under **Manage**, pick a team in **Add to** and press **Add**. You may add
   a bot to the teams you may invite people to.
4. The bot is in no channel yet. In each channel it should work in, open
   **Members → Add people** and find the bot. Anyone may add it to a public
   channel; a private channel's admins only. The bot may also join a public
   channel of its teams itself, with `ChannelService/JoinChannel`, and
   people in its teams may write to it directly.

Whoever holds the token acts as the bot. Keep it out of code and
repositories; hand it to the program in its environment or a secret store,
and never log it. **Manage → Reset token** makes a new one and stops the old
one at once: the bot's sockets close and its HTTP calls answer 401.
**Delete** removes the bot; its messages stay.

Programs may do the same with `BotService`: `CreateBot`, `AddBotToTeam`,
`UpdateBot`, `ResetBotToken` and `DeleteBot`, called by the owner.

## A first call

The HTTP Bot API is the quickest way to try a token:

```sh
export GOCHAT_URL=https://chat.example.com
export GOCHAT_BOT_TOKEN=b_...

curl -sS -X POST "$GOCHAT_URL/api/v2/bot/getMe" \
  -H "Authorization: Bearer $GOCHAT_BOT_TOKEN"
```

```json
{"ok":true,"result":{"user":{"id":"bc3bd805-...","username":"weather_bot","display_name":"Weather","bot":true,"bot_owner_id":"8976d5b8-...",...},"session_id":"9703494b..."}}
```

To post, the bot needs a channel's id. It comes in the bot's `channel_joined`
update when someone adds it, from `listChannels {"team_id": "..."}`, which
lists the channels the bot is in, and in the channel's address on the web,
`/t/<team>/ch/<id>`.

```sh
curl -sS -X POST "$GOCHAT_URL/api/v2/bot/sendMessage" \
  -H "Authorization: Bearer $GOCHAT_BOT_TOKEN" -H "Content-Type: application/json" \
  -d '{"channel_id": "b9cf4aed-...", "content": "Hello from **curl**"}'
```

The answer is `{"ok": true, "result": {"message": {...}}}`, the message as
posted. Every method takes its call's request as JSON with the proto field
names (`channel_id`, `root_id`), and the table in
[protocol.md](protocol.md#the-http-bot-api) maps the short names to calls.

## Getting updates

A bot gets what happens to it in one numbered sequence, its account's: each
update carries `pts`, and the bot keeps the last one it handled. There are
three ways to take them, and a bot uses one:

| | Socket | Long polling | Webhook |
| --- | --- | --- | --- |
| How | `pkg/gochat`, or any client of the protocol | `getUpdates` in a loop | the server POSTs each update |
| The bot needs | a process that stays up | a process that stays up | an https address the server reaches |
| Who keeps `pts` | the bot | the server | the server |

Pick the socket for Go, or for any bot that keeps a process running and
wants updates the moment they happen. Pick long polling for other languages,
or a bot behind a firewall: it needs nothing but HTTPS and JSON. Pick a
webhook when the bot runs where it is called rather than where it waits,
behind a load balancer or as a function.

### The socket, with pkg/gochat

[`pkg/gochat`](../pkg/gochat) connects, reconnects with backoff, and resumes
after the last update it handed out:

```go
c, err := gochat.Dial(ctx, gochat.Options{URL: url, Token: token, Client: "weatherbot/1", Pts: saved})
if err != nil {
	return err
}
defer c.Close()
for u := range c.Updates() {
	if m := u.Event.GetMessagePosted().GetMessage(); m != nil {
		// ...
	}
	save(c.Pts())
}
return c.Err()
```

- `c.Call(ctx, "gochat.v1.MessageService/PostMessage", req, res)` makes a
  call. A failure the server reports is a `*gochat.Error` with the code of
  [protocol.md](protocol.md#calls). `gochat.ErrLost` means the connection
  dropped before the answer came: the server may have made the call, so the
  client does not make it again.
- `u.Reload` is set when the bot missed more than the server replays, or was
  away longer than it keeps updates (a week by default): what came meanwhile
  is gone. `u.Ephemeral` marks updates without a number, such as typing.
- `Updates` closes when the client stops for good: the token is not valid
  (4001), the client broke the protocol (4002), or the server takes its
  layer no more (4011). `c.Err()` says why.

[examples/echobot](../examples/echobot) is a whole bot:

```sh
GOCHAT_URL=https://chat.example.com GOCHAT_BOT_TOKEN=b_... \
  go run ./examples/echobot -state echobot.pts
```

Bots in other languages speak the protocol as
[protocol.md](protocol.md#connecting) says, with code generated from
[`protocol.proto`](../api/protobuf/proto/gochat/v1/protocol.proto).

### Long polling with getUpdates

```sh
curl -sS -X POST "$GOCHAT_URL/api/v2/bot/getUpdates" \
  -H "Authorization: Bearer $GOCHAT_BOT_TOKEN" -H "Content-Type: application/json" \
  -d '{"offset": 0, "timeout": 50}'
```

```json
{"ok":true,"result":{"updates":[
  {"team_joined":{"team":{...}},"pts":"1","pts_count":1},
  {"channel_joined":{"channel":{"id":"b9cf4aed-...","name":"town-square",...}},"pts":"2","pts_count":1}
],"pts":"2"}}
```

- Each update is an `Event` of
  [`gateway.proto`](../api/protobuf/proto/gochat/v1/gateway.proto) in
  protobuf's JSON mapping: one field names what happened (`message_posted`,
  `callback_query`, `channel_joined`, ...). 64-bit numbers such as `pts`
  are strings, enums are names, and fields left at zero are left out, an
  empty `updates` too. Skip fields you do not know: later versions add
  some.
- `offset` is the last `pts` handled plus one. It confirms the updates
  before it, and the server remembers: `0`, or no offset, starts after the
  last update confirmed, after a restart too. An update handled but not yet
  confirmed, because the bot stopped before its next call, comes again.
- `timeout` is how long the call waits for an update when none is waiting,
  at most 50 seconds; give the HTTP client longer. `limit` is at most 100.
- `too_long` is set when the bot was away longer than the server keeps
  updates. The result holds none: take `pts` plus one as the next offset.
- A bot polls once at a time. A second `getUpdates` ends the one waiting
  with 409, so two copies of a bot end each other's polls over and over.
  While a webhook is set, `getUpdates` answers 409 too.
- `getWebhookInfo` tells `delivered_pts`, the last update confirmed, and
  `pending`, how many wait, with a webhook or without.

[examples/dicebot](../examples/dicebot) is a whole bot in Python with
nothing but the standard library:

```sh
GOCHAT_URL=https://chat.example.com GOCHAT_BOT_TOKEN=b_... \
  python3 examples/dicebot/dicebot.py
```

### Webhooks

```sh
export GOCHAT_WEBHOOK_SECRET=$(openssl rand -hex 32)

curl -sS -X POST "$GOCHAT_URL/api/v2/bot/setWebhook" \
  -H "Authorization: Bearer $GOCHAT_BOT_TOKEN" -H "Content-Type: application/json" \
  -d '{"url": "https://bot.example.com/gochat", "secret": "'"$GOCHAT_WEBHOOK_SECRET"'"}'
```

The server then POSTs each update to the URL, as JSON like one of
`getUpdates`' updates, with the secret in `X-GoChat-Bot-Secret`.

- The URL is https, without credentials, and reaches a public address. The
  server refuses loopback, private, link-local and other internal
  addresses, a name that resolves to one at the time of each delivery too,
  and follows no redirect. For development only, a server with
  `services.bots.allow_private_webhooks` set takes plain http and local
  addresses.
- The secret is up to 256 letters, digits, `_` and `-`. Always set one, and
  answer 401 to a request without it, comparing in constant time: the URL
  alone does not prove a request came from the server.
- A 2xx answer takes the update. Anything else, or no answer within 10
  seconds, and the server sends it again, after 1 second, then 2, 4, and so
  on up to 5 minutes, until it is taken.
- Updates go one at a time, in order: the next waits until this one is
  taken. Answer quickly, and answer 2xx to updates the bot ignores or
  cannot handle, or they hold back every update after them. A delivery that
  timed out may have been handled: expect one twice now and then.
- A new webhook gets the updates after it was set. Changing its URL or its
  secret keeps its place, and `deleteWebhook` hands the place to
  `getUpdates`.
- When updates stop coming, `getWebhookInfo` tells the last error, such as
  `the webhook answered 401`, and when it happened.

[examples/webhookbot](../examples/webhookbot) takes deliveries and answers
over the HTTP Bot API:

```sh
GOCHAT_URL=https://chat.example.com GOCHAT_BOT_TOKEN=b_... GOCHAT_WEBHOOK_SECRET=... \
  go run ./examples/webhookbot -addr :8443 -cert cert.pem -key key.pem
```

Without `-cert` it serves plain http, for a TLS-terminating proxy in front.

## Commands

`setCommands` replaces the list of the bot's commands, which people see
suggested when they type `/` in a channel with the bot:

```json
{"commands": [{"name": "roll", "description": "Roll a die: /roll or /roll 20"}]}
```

A bot offers up to 100. A name is 1–32 lowercase letters, digits and
underscores, and a description 1–256 characters.

A command is an ordinary message, `/roll 20`, that everyone in the channel
sees. It reaches every bot in the channel, those in privacy mode too, so
with several bots people address one: `/roll@dice_bot 20`. A bot ignores
commands addressed to another bot. `gochat.Command(text, me.Username)`
reads a command this way, and the Python example has its own `command()`.
The arguments are what follows the first space.

To answer in the thread the command was posted in, pass its `root_id` back
in `sendMessage`.

## Buttons

`markup` in `sendMessage` or `editMessage` puts buttons under a message:

```sh
curl -sS -X POST "$GOCHAT_URL/api/v2/bot/sendMessage" \
  -H "Authorization: Bearer $GOCHAT_BOT_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "channel_id": "b9cf4aed-...",
    "content": "Deploy v1.4 to production?",
    "markup": {"rows": [{"buttons": [
      {"text": "Deploy", "callback_data": "deploy:v1.4"},
      {"text": "Changelog", "url": "https://example.com/changes/v1.4"}
    ]}]}
  }'
```

A button opens its `url`, or sends the bot a `callback_query` update with
its `callback_data`, the message and the person who pressed it, `from`:

```json
{"callback_query":{"id":"d23781af-...","from":{"id":"8976d5b8-...","username":"ada",...},"message":{"id":"4b035592-...",...},"data":"deploy:v1.4"},"pts":"9","pts_count":1}
```

- Answer every press within a minute with `answerCallback`:
  `{"query_id": "d23781af-...", "text": "Deploying."}`. The presser sees the
  text as a passing notice, or as an alert with `"alert": true`; an empty
  text shows nothing. A press answered or older than a minute answers 404.
- The server lets people press only the buttons a message has, so `data`
  is always one the bot put there. Who pressed it is up to the bot to
  check, before it deploys anything.
- `callback_data` holds 64 bytes: keep the state in the bot and a key in the
  data.
- `editMessage` replaces a message's buttons: send them again to keep them,
  and leave `markup` out to remove them. A reply's edit takes its `root_id`.

## What a bot sees

A bot starts in **privacy mode**. From its channels it receives:

- messages that start with `/`, and those that mention it;
- replies in the threads it posted in;
- every message of its direct conversations;
- edits, deletions and reactions of those messages, and changes to its
  channels;
- `callback_query` when its buttons are pressed;
- `team_joined`, `channel_joined` and `channel_left` as it is added and
  removed.

It never receives the posts of its own messages. It reads only what it was
sent: `getMessage` of another message answers 404, and `ListMessages` and
`ListThread` answer 403.

With **Gets every message** checked under **Manage** (`sees_all` in
`UpdateBot`), a bot receives every message of the channels it is added to
from then on, and may read their history. A channel keeps the mode the bot
had when it was added: to change it, remove the bot from the channel and
add it again. A team's `town-square`, which every member stays in, cannot
be left, so choose before adding a bot there. Ask for `sees_all` only when
the bot needs it: it then reads everything people say.

## Errors

The HTTP Bot API answers an error as

```json
{"ok": false, "error_code": 403, "description": "join the channel first"}
```

with `error_code` the HTTP status. Over the socket the same failures carry
the protocol's codes.

| Status | Code | Meaning | What to do |
| --- | --- | --- | --- |
| 400 | 3 | The request is not valid: a field, a size, JSON that does not parse. | Fix the request. |
| 401 | 16 | The token is not valid: reset, or the bot deleted. | Stop. |
| 403 | 7 | Not allowed: not in the channel, someone else's message, history in privacy mode. | Do not retry. |
| 404 | 5, 12 | No such thing, or a method that does not exist or is not open to bots. | Do not retry. |
| 409 | 6, 9 | It exists, or not now: a second `getUpdates`, a webhook set, a channel that cannot be left. | Depends on the call. |
| 429 | 8 | Too many requests, or a limit reached. | Wait, then retry. |
| 500 | 13 | The server failed. | Retry later. |
| 503 | 14 | Unavailable. | Retry with backoff. |

Calls over the [request budget](#limits) are refused before they reach the
Bot API: 429 with `Retry-After`, and an `ApiError` as the body,
`{"code": 8, "message": "too many requests, slow down"}`, rather than
`ok` and `description`.

Retry 429, 5xx and network failures with backoff; other errors come again
the same way. A call whose answer never came may have been made: posting
it again may post twice.

## Limits

These are the defaults; a server's operator may change them.

| What | Limit |
| --- | --- |
| The request budget | 30 calls a second, in bursts of up to 120, per bot, over HTTP and the socket together (`web.rate_limit`); ten times that per address |
| Posting | 5 messages a second in a channel, in bursts of 20 (`services.channels.post_rate`, `post_burst`); more answers 429 `you are posting too fast` |
| A socket's calls | 32 in flight and 256 waiting their turn |
| A socket's frames | 10 a second, in bursts of 40, each up to 64 KiB (`gateway.frame_rate`, `frame_burst`); faster, and the server closes the socket and fails the calls in flight |
| Sockets | 20 per account (`gateway.max_per_user`); more closes with 4008 |
| An HTTP request | 1 MiB |
| A message | 16000 characters (`services.channels.max_message_length`) |
| Buttons | 8 rows of 8; a text of 64 characters; `callback_data` of 64 bytes |
| A press's answer | 200 characters, within a minute |
| Commands | 100; names of 1–32 characters, descriptions of 1–256 |
| `getUpdates` | waits at most 50 seconds; at most 100 updates |
| A webhook | 10 seconds to answer (`services.bots.webhook_timeout`); retries up to 5 minutes apart (`webhook_retry_max`) |
| Updates kept | a week (`services.updates.retention`) |
| Bots | 20 per owner (`services.bots.max_per_owner`) |

A bot that makes calls in bursts over the socket paces them itself:
`pkg/gochat` sends each call as it is made.

## What a bot may not call

Bots make only the calls of
[the table in protocol.md](protocol.md#what-bots-may-call); any other
answers 404, `bots cannot make this call`, over HTTP and code 12 over the
socket. Among those it cannot make: creating channels or teams, adding
people to them, inviting, pinning, saving, searching, pressing buttons,
managing bots, and anything of `AdminService` or `SafetyService`. It deletes
and removes link previews of its own messages only, and `ListMessages` and
`ListThread` need `sees_all`. A bot cannot start a conversation with a
person: people write to it first.

## Running a bot

**Run one copy.** Two copies of a socket bot both get every update and both
answer; two polling ones end each other's polls. Several receivers may share
a webhook's URL, but the server delivers one update at a time anyway.

**Use a service manager or a container** that restarts the bot when it
fails. A systemd unit, with the token in a file only root reads:

```ini
# /etc/systemd/system/dicebot.service
[Unit]
Description=Dice bot for GoChat
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/bin/python3 -u /opt/dicebot/dicebot.py
# GOCHAT_URL=... and GOCHAT_BOT_TOKEN=..., mode 0600
EnvironmentFile=/etc/dicebot.env
DynamicUser=yes
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

A Go bot that keeps its `pts` adds `StateDirectory=echobot` and
`ExecStart=/usr/local/bin/echobot -state /var/lib/echobot/pts`. In a
container:

```dockerfile
FROM python:3.13-alpine
COPY examples/dicebot/dicebot.py /app/dicebot.py
USER nobody
CMD ["python3", "-u", "/app/dicebot.py"]
```

```sh
docker build -t dicebot -f dicebot.Dockerfile .
docker run -d --name dicebot --restart unless-stopped --env-file dicebot.env dicebot
```

A bot whose token was reset exits, and starting it again fails the same
way until it has the new token.

**Reconnect with backoff and jitter.** `pkg/gochat` does it: it waits up
to half a second at first, twice as long after each failure up to 30
seconds, and starts over after a connection that lasted a minute. The server also closes a bot's socket every day, when
its session renews, and the client reconnects. A client of the protocol of
your own follows the close codes of [protocol.md](protocol.md#connecting). A
polling bot backs off on network failures, 5xx and 409, waits out
`Retry-After` on 429, and stops on 401. A webhook's retries are the
server's: the receiver needs only to come back.

**Keep the last `pts`.** With `getUpdates` or a webhook the server keeps it.
Over the socket the bot does: after handling each update with a number,
store `c.Pts()` in a file or a database, and pass it back in
`Options.Pts`. Write the file whole, to a new file renamed over the old,
so a crash never leaves half a number. Started with `0`, the bot gets only
what comes from then on, and misses what came while it was down. Stored
after handling, an update handled just before a crash comes again; stored
before, it is lost: choose by what the bot does twice more safely.

## The examples

| | Updates | Language | Shows |
| --- | --- | --- | --- |
| [echobot](../examples/echobot) | the socket | Go, `pkg/gochat` | commands, a button and its answer, keeping `pts` |
| [dicebot](../examples/dicebot) | `getUpdates` | Python 3, standard library | commands and `@bot` addressing, a button that edits its message |
| [webhookbot](../examples/webhookbot) | a webhook | Go | checking the secret, answering 2xx, calling the HTTP Bot API |
