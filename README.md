# GoChat

A self-hosted team chat in the spirit of Mattermost: teams, public and private
channels, voice channels to talk in as in Discord, direct messages, one-time
invite links, attachments, voice messages, presence and typing indicators,
all delivered in real time. The whole application runs on
the [hollywood](https://github.com/anthdm/hollywood) actor engine, on both the
server and the web gateway, and the browser UI is server-rendered HTML driven
by [htmx](https://htmx.org) over a WebSocket. Native apps for Android and iOS,
in Kotlin, live in [mobile](mobile). Bots sign in with a token and speak the
same protocol as apps, with commands, buttons on their messages, an HTTP Bot
API and webhooks.

## Architecture

```
 browser ──HTTPS/WS──▶ gochat web ──DRPC (protobuf)──▶ gochat server ──▶ ScyllaDB
  (htmx)               gateway actors                    service actors    MinIO
 apps, bots ──/api───▶    ▲                                   ▲
 (Android, iOS)                                               │
    │ signaling WS + WebRTC (Opus over UDP/TCP)               │ DRPC stream
    └──────────────────────────────────────────▶ gochat voice ┘
                                                  SFU actors
```

**`gochat server`** owns all state and rules.

| Actor | Role |
| --- | --- |
| `accounts` (pool) | sign-up, sign-in, argon2id hashing, sessions, TOTP, recovery codes |
| `directory` (pool) | teams, invites, channel listings, direct messages, profiles |
| `channels` → one actor per channel | membership, authorization, posting, edits, history, read state, typing |
| `files` (pool) | upload grants, downloads, orphan cleanup |
| `hub` | live connections of every web gateway, event fan-out, presence |
| `updates` → one actor per account | numbers the account's updates and keeps their log |
| `bots` (pool) | bot accounts and tokens, commands, button answers, webhooks |
| `voice` | who talks in which voice channel, join tokens, the voice servers |

A channel actor is the single writer for its channel, so ordering and
membership checks never race. It loads lazily on first use and passivates when
idle. Blocking work (password hashing, Scylla reads) runs in worker pools so it
never stalls a mailbox.

**`gochat web`** is a stateless gateway. Each page gets one WebSocket served by
a session actor; a router actor multiplexes every session over a single
bidirectional DRPC stream to the server and reattaches them after a reconnect.
Events arrive as protobuf and leave as htmx out-of-band HTML fragments. You
can run as many gateways as you like behind a load balancer.

**`gochat voice`** is a voice server, an SFU in the manner of Discord's. It
keeps one actor per voice channel with talk, which receives each person's
Opus track over WebRTC ([Pion](https://github.com/pion/webrtc)) and forwards
it to the others, neither mixed nor decoded.

The server is designed as a single node: channel actors and the hub live in
one process. Scylla and MinIO scale independently, and so do voice servers:
each channel's talk runs on one of them, the least loaded when it starts.

### Voice channels

As in Discord, the chat server keeps the voice states and voice servers
carry the voice:

1. A page or an app asks to join (`JoinVoice`). The chat server checks the person may,
   picks the channel's voice server and hands back its signaling endpoint and
   a single-use token valid for 30 seconds.
2. The page opens the voice server's websocket and identifies with the token,
   which the voice server claims from the chat server over its DRPC stream.
   Only then does the person count as talking, and the team's pages see them
   under the channel.
3. The voice server offers, the page answers, and the page's microphone and
   everyone else's voices flow over WebRTC. The voice server only ever offers,
   renegotiating as people come and go. The page tells it when the microphone
   hears speech, and it tells the others.

Each person talks in one channel at a time: joining elsewhere, or from another
tab, disconnects the earlier connection. Mute and deafen are the person's own
and show to the others. A channel's admins and the team's can disconnect
anyone. Leaving the team, signing out or the sign-in expiring ends the talk.
A channel takes at most 25 people.

Each browser picks its noise suppression in its account settings: off, the
browser's own filter (the default), or strong, which runs
[RNNoise](third_party/rnnoise-wasm) over the microphone in an audio worklet
before sending it, adding 20 ms. A browser that cannot run it uses its own
filter.

### The GoChat protocol and bots

Apps and bots speak one protocol, in the manner of Telegram's MTProto, over
one socket at `/api/v2/socket`: binary protobuf frames carrying calls of the
chat API, their results, and updates. [docs/protocol.md](docs/protocol.md)
specifies it.

- Updates are numbered: each account's in one sequence (`pts`), each text
  channel's in another, and kept in a log for a week. A client that comes
  back says the last number it applied and gets what it missed, in order;
  `UpdatesService` fills any gap.
- A bot is an account its owner creates in **Settings → Bots**, with a token
  that only the owner sees, once. The owner adds it to a team, and channel
  members add it to channels. Unless it is set to see every message, a bot
  gets commands (`/echo hi`), mentions of it, replies in its threads and its
  direct messages.
- A bot's commands show in the composer as people type `/`, and its messages
  may carry buttons: a press reaches the bot as a `CallbackQuery`, and its
  answer shows to the person who pressed.
- Bots that would rather not keep a socket use the HTTP Bot API:
  `POST /api/v2/bot/<method>` with JSON, such as `sendMessage`, `getUpdates`
  (long polling) or `setWebhook`, which has the server POST each update to
  the bot's https address.

[pkg/gochat](pkg/gochat) is a Go client of the protocol that reconnects and
catches up by itself, and [examples/echobot](examples/echobot) a bot built on
it:

```bash
GOCHAT_URL=http://localhost:8080 GOCHAT_BOT_TOKEN=b_... go run ./examples/echobot -state echobot.pts
```

### Client API for native apps

The web gateway also serves the [mobile apps](mobile) at `/api/v1`, the way
Discord's apps use its REST API and gateway. It forwards calls of the same
chat API the pages use, so the server checks every one as it does for the
web:

- `POST /api/v1/rpc/<service>/<method>` makes one call, such as
  `/api/v1/rpc/gochat.v1.ChannelService/GetChannel`, with the request as
  protobuf (`application/proto`) or JSON (`application/json`) and the
  answer in the same encoding, or an `ApiError`.
- `GET /api/v1/gateway` is the event socket: binary `ClientFrame`s carrying
  the person's events as the server sends them. The app answers with
  `AppFrame`s naming the channel it shows and the people whose presence it
  shows, as pages do, since typing and presence go only to those. The chat
  server keeps, of those people, the ones the person shares a team with, as
  `GetPresence` does.
- `POST /api/v1/files`, `GET /api/v1/files/<id>`, `GET /api/v1/photos/...`
  and `POST /api/v1/photo` move attachments and photos.
- `GET /api/v1/info` answers, without a token, the server's version and the
  oldest app it takes, `min_app_version` in the web's configuration; an
  older app asks its person to update it.

Apps sign in with `AuthService/Login` (and `VerifyMFA` for two-factor
accounts) and send the session token as `Authorization: Bearer <token>`.
Their sessions show in the web's session list by the device the app names
in its User-Agent, such as "GoChat app on Pixel 8", and are revoked there
like any other; each session has its own request budget.
They talk in voice channels as pages do: `VoiceService/JoinVoice` hands
them the voice server's websocket and a token for it. The
API never reads cookies, so no page can make a browser call it for a
signed-in person, and it opens only the calls people make: not the gateways'
streams, nor the photo calls that expect a picture the gateway has already
processed. [client.proto](api/protobuf/proto/gochat/v1/client.proto)
documents it, close codes included. `/api/v1` stays until the apps move to
the GoChat protocol, which serves the same calls.

### Storage

- **ScyllaDB** holds everything. Messages are partitioned by
  `(channel_id, 10-day bucket)` with time UUIDs, so history pages read one
  partition at a time. Uniqueness (usernames, emails, team and channel names),
  invite consumption, the first-admin bootstrap and TOTP replay protection use
  lightweight transactions.
- **MinIO** stores attachments under `attachments/<channel>/<id>`. Uploads
  stream through the server, which sniffs the content type and enforces the
  size limit and the uploader's quota; attachments never posted are deleted
  by a janitor. What each person stores is kept as a running total, updated
  with lightweight transactions as files come and go, so a quota never needs
  a scan.
- **Voice messages** are attachments marked as voice. The browser records
  them with `MediaRecorder` and uploads them with their length; the server
  takes them only in channels that allow voice, only up to
  `services.files.max_voice_duration` (10 minutes by default) and
  `services.files.max_voice_size` (16 MiB), and only when their first bytes
  are a WebM, Ogg or MP4 recording. Each channel allows
  voice unless its admins, or the team's, turn it off in the channel's
  settings (or when creating it); direct conversations always allow it.
  Turning it off keeps the voice messages already sent.
- **Casbin** policies live in Scylla too (custom adapter). Policies grant
  actions to role names; who holds which role in a team or channel is part of
  the membership rows.

## Security

- Passwords: argon2id with configurable cost; constant-time paths for unknown
  users; per-account and per-IP sign-in limits.
- Sessions: 256-bit random tokens, only their SHA-256 is stored; revocation
  (logout, password change, TOTP change) closes live sockets at once. Account
  settings list each session's device, addresses and when it was last seen,
  and sign any of them out.
- Administration: system admins find people by username or email at
  `/admin/users`, deactivate them, which signs them out everywhere and closes
  their sockets at once, sign out all their sessions, and set their storage
  quota (`services.files.quota` by default). Deactivated people cannot sign
  in; an admin cannot deactivate themself.
- Bootstrap: the first account, which becomes the system admin, needs the
  setup token unless `services.accounts.allow_open_bootstrap` is set.
- TOTP: optional per user, secrets sealed with AES-256-GCM bound to the user
  id, codes single-use, ten hashed single-use recovery codes.
- Invites: one-time, expiring links; only the token hash is stored and it is
  consumed with a lightweight transaction. Team admins see the open invites
  and can revoke them; an invite is suspended while its creator may not
  invite, and creating them is capped per person.
- Web: strict CSP (no inline script, no eval; WebAssembly only for the noise
  filter), CSRF double-submit token plus
  `Origin` and `Sec-Fetch-Site` checks, `__Host-` `Secure` cookies and HSTS on
  https, `X-Frame-Options: DENY`, `nosniff`, per-IP rate limits. The page may
  use the microphone, for voice messages, and no other device.
- Content: messages are rendered as Markdown with raw HTML disabled; images
  become links so a message cannot make readers fetch outside URLs; links open
  with `rel="nofollow noopener noreferrer"`. Attachments are served with
  `Content-Disposition: attachment` unless they are safe images or voice
  messages, which play in place with the audio type the server detected.
- Voice: join tokens are single-use, short-lived, stored hashed and bound to
  one voice server; its websocket takes the web gateway's pages and no other
  site's (the apps, which are not pages, send no origin). Only
  Opus is negotiated, and each voice is capped at 512 kbit/s so the room
  cannot be made to repeat a flood. Connections that have not identified and
  signaling messages are bounded. The voice server is ICE lite: it answers
  browsers and never sends to an address a browser names. Voice is
  encrypted in transit between each browser and the voice server
  (DTLS-SRTP); the voice server itself can hear it, as Discord's can.
- Server API: every DRPC call carries the shared gateway key (checked in
  constant time) and the user's session; voice servers hold a key of their
  own, which opens only their link to the server, so the internet-facing
  voice server never holds the key that acts for users; errors returned to gateways never
  carry internal causes. TLS and client certificates are supported on the DRPC
  link and on the Scylla connection.

## Requirements

- Go 1.27.3, [just](https://github.com/casey/just), [buf](https://buf.build)
- Docker with Compose v2

## Getting started

```bash
cp .env.template .env
```

Fill in `.env`: database and MinIO credentials, a `GATEWAY_KEY` of at least 32
characters, a different `VOICE_SERVER_KEY` for voice channels from
`openssl rand -hex 32`, a `SERVICES_ACCOUNTS_SECRET_KEY` from `openssl rand -base64 32` and
a `SERVICES_ACCOUNTS_SETUP_TOKEN` from `openssl rand -hex 24`.
If Docker Hub is not reachable, set `DOCKER_REGISTRY` to a mirror such as
`mirror.gcr.io/`.

### Everything in Docker

```bash
just up
```

This builds the image, starts Scylla and MinIO on an internal network, applies
the migrations and starts the server and the web gateway on
`http://localhost:8080` (`WEB_PORT`). The first account to sign up, with the
setup token, becomes the system admin and is asked to create the first team;
everyone else joins through invite links (or freely when
`services.accounts.open_signup` is on).

Without a setup token the server refuses to create that first account, and
says so in its log and on the sign-up page: whoever reached it first would
own it. On a machine nobody else can reach, `services.accounts.allow_open_bootstrap`
(`SERVICES_ACCOUNTS_ALLOW_OPEN_BOOTSTRAP` for host runs) lets the first sign-up
become the admin without one.

### Running the binaries on the host

```bash
just stores    # scylla on 127.0.0.1:9042, minio on 127.0.0.1:19000
just migrate
just server    # in one terminal
just web       # in another, then open http://localhost:8080
just voice     # in a third, for voice channels
```

These use the development configs in [config/](config).

### Recipes

| Recipe | What it does |
| --- | --- |
| `just build` | regenerate protobuf code and build `.bin/gochat` |
| `just up` / `down` / `downv` | run, stop, or stop and wipe the Docker stack |
| `just logs <service>` | follow a service's logs |
| `just migrate-status` | list applied and pending migrations |
| `just proto` | lint and regenerate the protobuf and DRPC code |
| `just lint`, `just test`, `just test-race` | checks |
| `just pre-commit` | generate, tidy, lint and test |

## Deploying

Every push to `main` that passes CI is deployed to a Linux server with
Docker Compose. The `image` job publishes the image to
`ghcr.io/ssenerg/gochat`, for amd64 and arm64, tagged `latest` and
`sha-COMMIT`; the `deploy` job sends its digest over SSH. The key GitHub
signs in with runs nothing but [`deploy.sh`](deployments/docker/deploy.sh)
there, which pulls exactly that image, applies the migrations and restarts
the server and the web gateway with it. If they do not come up healthy, the
script puts the image that ran before back and the job fails. The voice
server restarts after them, when `.env` sets `VOICE_SERVER_KEY`: if it
fails, chat stays on the new image and the job fails, so voice alone never
rolls a deploy back. The output of the migrations, the server, the gateway
and the voice server goes to the server's journal
(`journalctl -t gochat-deploy`), not to the public CI log. Scylla and MinIO
keep running across deploys. The server needs to reach `ghcr.io`.

A leaked key can therefore only deploy images published to that
repository, in the setup the server has: the script,
`docker-compose.yml` and `.env` belong to root, and the key cannot change
them. When `deployments/docker/deploy.sh` or
`deployments/docker/docker-compose.yml` change here, install them on the
server again, as below; voice channels changed both.

Once, on the server, as root, in a clone of this repository: a user to
deploy as (in the `docker` group, which is as good as root on that server)
and the directory GoChat runs from, which it may read but not change, with
its `.env` (see [`.env.template`](.env.template); generate every secret,
e.g. with `openssl rand -hex 32`, and keep it, since the data is only
readable with the same credentials):

```sh
useradd --create-home --groups docker deploy
install -d -o root -g deploy -m 750 /opt/apps/gochat
install -o root -g deploy -m 640 deployments/docker/docker-compose.yml .env /opt/apps/gochat/
install -o root -g deploy -m 750 deployments/docker/deploy.sh /opt/apps/gochat/
```

The web gateway listens on `127.0.0.1:$WEB_PORT`: put a TLS-terminating
proxy or tunnel in front of it, set `WEB_PUBLIC_URL` to the `https` address
people use and `WEB_TRUSTED_PROXIES` to where the proxy's requests come
from. Set `SERVICES_ACCOUNTS_SETUP_TOKEN` until the first account exists.

On your computer, a key for GitHub to sign in with, and the server's key
for GitHub to check it by. Check the key `ssh-keyscan` prints against
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the server:

```sh
ssh-keygen -t ed25519 -N '' -C gochat-deploy -f gochat-deploy
ssh-keyscan -t ed25519 SERVER > known_hosts   # with -p PORT for another port
```

Add `gochat-deploy.pub` to `~deploy/.ssh/authorized_keys` on the server,
with the image it may deploy in front of it. `restrict` takes away shells,
terminals and forwarding; `command` runs whatever GitHub asks for, with
what it asked for as the digest:

```
restrict,command="/opt/apps/gochat/deploy.sh ghcr.io/ssenerg/gochat" ssh-ed25519 AAAA... gochat-deploy
```

On GitHub, under Settings → Environments, create the environment
`production`, let only `main` deploy to it (Deployment branches → Selected
branches), and give it these secrets:

```sh
gh secret set DEPLOY_HOST --env production --body SERVER
gh secret set DEPLOY_SSH_KEY --env production < gochat-deploy
gh secret set DEPLOY_KNOWN_HOSTS --env production < known_hosts
rm gochat-deploy   # GitHub has it now
```

Its variables, all optional: `DEPLOY_USER` (`deploy`), `DEPLOY_PORT`
(`22`) and `DEPLOY_URL`, the address shown with each deploy. Until the
secrets are set, the job skips deploying with a warning. If the package on
`ghcr.io` is private, sign the server in to it once with a token that may
read packages: `sudo -u deploy docker login ghcr.io`.

To deploy again, run the CI workflow on `main` (Actions → CI → Run
workflow). To deploy a published image by hand, signed in as the server's
admin, `just deploy admin@SERVER` takes `latest` or a `sha-COMMIT` tag. To
go back one deploy on the server:

```sh
cd /opt/apps/gochat && sudo -u deploy sh -c 'docker image tag gochat:previous gochat:latest && docker compose up -d --no-deps --force-recreate server web'
```

Migrations only add to the schema, so going back works as long as they keep
doing so.

### Voice in production

Voice needs the server to have a public IP address and a `VOICE_SERVER_KEY` in
`.env` (`openssl rand -hex 32`, not the gateway key). Two things must reach the
voice server:

1. **The signaling websocket** (`VOICE_BIND:VOICE_PORT`, 127.0.0.1:8090),
   through the same TLS proxy or tunnel as the web gateway. The simplest
   route is a path on the same host: send `/rtc` to it, and set
   `VOICE_PUBLIC_URL=wss://chat.example.com/rtc`. With nginx:

   ```nginx
   location /rtc {
       proxy_pass http://127.0.0.1:8090;
       proxy_http_version 1.1;
       proxy_set_header Upgrade $http_upgrade;
       proxy_set_header Connection "upgrade";
       proxy_read_timeout 1h;
   }
   ```

   On a host of its own, such as `wss://voice.example.com/rtc`, also set
   `WEB_VOICE_ORIGINS=wss://voice.example.com`.

2. **The media port** directly, without any proxy: `VOICE_MEDIA_PORT`
   (7882), on UDP and on TCP. Set `VOICE_PUBLIC_IP` to the server's public
   address and open the port in the firewall, e.g.
   `ufw allow 7882/udp && ufw allow 7882/tcp`, or in the cloud provider's
   security group. A Cloudflare Tunnel or HTTP proxy cannot carry it.

Browsers use UDP, and fall back to TCP on networks that block UDP. For
networks that block both, run a TURN server such as coturn with
`use-auth-secret` and `static-auth-secret=<secret>`, and set
`VOICE_TURN_URLS` and `VOICE_TURN_SECRET` to match: the voice server hands
browsers credentials for it that expire.

Without `VOICE_PUBLIC_IP`, the voice server announces its container's own
addresses, which only browsers on the same Linux machine reach, as in
development.

## Command line

```
gochat server   [-p config-path] [-n config-name] [-e env-prefix]
gochat web      [-p config-path] [-n config-name] [-e env-prefix]
gochat voice    [-p config-path] [-n config-name] [-e env-prefix]
gochat migrate scylla up     <dir>
gochat migrate scylla status <dir>
gochat version
```

## Configuration

Each command embeds its defaults ([server](cmd/serve/default.yaml),
[web](cmd/web/default.yaml), [voice](cmd/voice/default.yaml),
[migrate](cmd/migrate/default.yaml)) and overlays
`<config-path>/<config-name>.yaml` and then environment variables. A key's
variable is its path in upper case with dots as underscores, optionally behind
a prefix: `database.scylla.password` → `DATABASE_SCYLLA_PASSWORD`. Unknown
keys are rejected at start-up so typos do not pass silently.

Secrets that should come from the environment:

| Variable | Used by |
| --- | --- |
| `DATABASE_SCYLLA_USERNAME`, `DATABASE_SCYLLA_PASSWORD` | server (the app's role) |
| `DATABASE_SCYLLA_ADMIN_USERNAME`, `DATABASE_SCYLLA_ADMIN_PASSWORD` | migrate, scylla-init |
| `STORAGE_MINIO_ACCESS_KEY`, `STORAGE_MINIO_SECRET_KEY` | server (the app's account) |
| `MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD` | minio, minio-init |
| `SERVER_DRPC_GATEWAY_KEY` / `CLIENT_DRPC_GATEWAY_KEY` | server / web (same value) |
| `SERVER_DRPC_VOICE_SERVER_KEY` / `CLIENT_DRPC_GATEWAY_KEY` | server / voice (same value, not the gateway key) |
| `VOICE_ICE_TURN_SECRET` (optional) | voice |
| `SERVICES_ACCOUNTS_SECRET_KEY` | server |
| `SERVICES_ACCOUNTS_SETUP_TOKEN` (until the first account exists) | server |

`services.updates` sets how long the update logs keep updates (`retention`,
a week by default), and `services.bots` how many bots a person may own and
how webhooks are delivered. Webhooks go only to public addresses unless
`services.bots.allow_private_webhooks` is set, for development.

Set `web.public_url` to the origin browsers use; an `https` origin turns on
`Secure` cookies and HSTS. Behind a reverse proxy, list its addresses in
`web.trusted_proxies` so rate limits see client addresses.

The server exposes `/healthz`, `/readyz` and Prometheus `/metrics` on its admin
listener (`server.admin.address`). Keep it and the DRPC listener on a private
network, or enable TLS with client certificates on the DRPC link.

## Tests

```bash
just test
```

The end-to-end suite in [test/e2e](test/e2e) drives a running stack through the
web gateway: invites, real-time delivery, typing, attachments, voice messages
and their channel setting, voice channels (with Pion peers that talk through
the voice server), the client API the apps use, bots (commands, buttons,
catching up after a restart, the HTTP Bot API and token resets through
[pkg/gochat](pkg/gochat)), private channel isolation,
CSRF, TOTP with recovery codes, session revocation, deactivation and storage
quotas. It needs an existing admin account and team, or with
`GOCHAT_E2E_BOOTSTRAP=1` and the server's token in `GOCHAT_E2E_SETUP_TOKEN`
creates them on a fresh server:

```bash
GOCHAT_E2E_URL=http://localhost:8080 GOCHAT_E2E_ADMIN=admin GOCHAT_E2E_ADMIN_PASSWORD=... GOCHAT_E2E_TEAM=acme go test ./test/e2e -count=1
```

## Layout

```
api/protobuf/      protobuf sources and generated protobuf + DRPC code
cmd/               cobra commands and embedded default configs
config/            configs for host runs and for the Docker stack
deployments/       Dockerfile and compose files
docs/              the GoChat protocol's specification
examples/          bots built on pkg/gochat
internal/actors    ask, pools and middleware on top of hollywood
internal/app       wiring of the server and web processes
internal/authz     casbin model and enforcer
internal/core      ids, passwords, TOTP, sealing, validation, models, caches
internal/repos     Scylla and MinIO repositories
internal/server    DRPC handlers and the admin listener
internal/services  hub, accounts, directory, channels, updates, bots, files, voice
internal/voiceserver  the voice server: signaling, rooms, media
internal/web       HTTP handlers, gateway actors, templates and static assets
migrations/scylla  CQL migrations
mobile/            the Android and iOS apps (Kotlin Multiplatform, Compose)
pkg/gochat         a Go client of the GoChat protocol, for bots
test/e2e           end-to-end tests
```
