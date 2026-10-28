# GoChat for Android and iOS

Native apps for GoChat, written once in Kotlin with
[Kotlin Multiplatform](https://kotlinlang.org/docs/multiplatform.html) and
[Compose Multiplatform](https://www.jetbrains.com/compose-multiplatform/).
They talk to a GoChat server over [the GoChat protocol](../docs/protocol.md):
one socket at `/api/v2/socket` for the chat API's calls and the person's
updates, in the same protobuf messages the web gateway uses.

## Layout

```
core/        the protocol, the API client and the app's state, in plain Kotlin (JVM and iOS)
ui/          the Compose screens, shared by Android and iOS, and each platform's services
androidApp/  the Android app: the Application, the Activity, the manifest and the icon
iosApp/      the Xcode project: a SwiftUI shell that shows the shared screens
```

- **core** generates its protobuf types with [Wire](https://square.github.io/wire/)
  from the server's own `api/protobuf/proto`. `Socket` keeps the protocol's
  socket open over [Ktor](https://ktor.io): it multiplexes calls by id,
  pings, and reconnects with backoff, resuming after the last account update
  applied. `ApiClient` makes calls over it, and moves files over HTTP.
  `GoChatApp` restores the session and signs people in over an anonymous
  socket, which then says hello again with the session's token. `Updates`
  applies the account's and each channel's numbered updates in order and
  asks for the difference after a gap. `ChatSession` holds teams, channels,
  presence and voice states, applies every update, catches up after a
  reconnect, and keeps the account's number so a restarted app resumes after
  it; `Timeline` and `ThreadView` hold a channel's
  and a thread's messages; `VoiceCall` talks in a voice channel, speaking the
  voice server's signaling protocol as the web's voice client does, over the
  platform's WebRTC (`RtcEngine`). Formatting and the message Markdown live
  here too, so the screens only draw, and `OpusCaf`, which readies the
  browsers' voice messages for iOS (below).
- **ui** draws the app with Material 3 in the web's colors, light and dark.
  Each platform supplies storage (Android's private preferences, sealed with
  a key the Android Keystore holds; the iOS Keychain), voice recording and playback (`MediaRecorder`/`MediaPlayer`,
  `AVAudioRecorder`/`AVAudioPlayer`), the file picker and sharing. Android
  supplies WebRTC too, from [Stream's build](https://github.com/GetStream/webrtc-android)
  of Google's library, and a foreground service that keeps a call going
  with the screen off. On iOS, WebRTC comes from [stasel/WebRTC](https://github.com/stasel/WebRTC),
  a Swift package the Xcode project links: `VoiceRtc.swift` implements the
  small protocols of `IosRtc.kt`, which adapt it to the core's `RtcEngine`.
- **iosApp/GoChatBroadcast** is the broadcast upload extension that shares
  the iPhone's screen: ReplayKit hands it the screen, and it writes each
  frame as a JPEG to a Unix socket the app listens on in the container of
  the app group they share (`Broadcast.swift`, in both), where
  `ScreenBroadcast.swift` hands it to WebRTC.

## What the apps do

- Sign in to any GoChat server, with two-factor codes, or sign up with an
  invite link.
- Teams, channels, voice channels, direct and group conversations, with
  presence, unread marks and mention counts, all live.
- Voice channels: join, talk, mute, deafen and leave, see who is talking,
  and, as a moderator, disconnect people. The call goes on in the
  background, on the loudspeaker or a headset, and rejoins after a network
  change; on Android it can be left from its notification.
- Cameras and screens in voice channels: turn the front camera on, share
  the screen (Android asks which; iOS starts a broadcast from the system's
  picker or the Control Center), see the others' cameras in their tiles and
  their screens above them, full screen at a tap. Cameras in tiles come in
  their smallest layer, and the one full screen in its best. The sidebar
  marks who has their camera on and who is live, as the web does.
- Messages with Markdown, mentions, emoji, images, files, reactions,
  threads, edits, deletes, pins and saves; typing indicators; search;
  followed threads; saved messages.
- Voice messages: record, send and play. Both apps record AAC in MP4,
  which the server takes and every platform plays. They play the browsers'
  recordings too: Chrome and Firefox record Opus in WebM (or Ogg). Android
  plays that as it is; iOS's player decodes Opus but opens neither
  container, so the iOS app first copies the Opus packets into CAF, which
  it opens. Nothing is decoded or encoded again, and the server is not
  involved.
- Bots: a BOT badge, the buttons under their messages (a press tells the
  bot, whose answer shows as a notice or an alert; a link opens once the
  person saw where it leads), and their commands suggested as one types `/`.
- Browse, create, mute and leave channels; moderators rename them, change
  their privacy, turn voice messages on or off and archive them. Delete a
  direct conversation for yourself or for both people. Conversations belong
  to the team they were opened in, as on the web.
- Profile name and photo, status with do not disturb, custom status,
  password, sign-out.
- Push notifications while the app is closed, from servers that push to
  this build (below): direct and group messages, replies in their threads
  and mentions. A tap opens the conversation or the thread, in its team.

Not in the apps yet:

- Two-factor setup, team settings and team photos stay on the web.

## Building

You need JDK 17 or later.

**Android**: Android Studio, or the Android SDK with platform 37.

```bash
cd mobile
./gradlew :androidApp:installDebug   # on a connected device or a running emulator
```

**iOS**: a Mac with Xcode 16 or later. Open `iosApp/iosApp.xcodeproj`, choose
your team under Signing & Capabilities for both targets, `iosApp` and
`GoChatBroadcast`, and run. The project's first build phase builds the
Kotlin framework with Gradle; Xcode fetches the WebRTC package itself. Voice
channels need a device or a simulator with a microphone, and the server's
voice settings as for Android below.

The app, its broadcast extension and the app group they share take their
identifiers from one build setting, `GOCHAT_BUNDLE_ID` at the project's
level (`io.github.ssenerg.gochat`): under another team, set it to an
identifier of yours, and both targets keep the App Groups capability with
`group.` and it. Sharing the screen needs an iPhone or iPad: simulators
have no broadcasts, and no camera.

**Tests**: the core's tests run anywhere:

```bash
cd mobile
./gradlew :core:jvmTest
```

With a running stack, the same command also drives it as the apps do: it
signs in, receives events from another device, sends, reacts, replies,
uploads a voice message, edits, deletes, opens a conversation and signs out.
It catches up after being away and after a restart, and runs a bot written
with the core: its command, buttons and answers.

```bash
GOCHAT_E2E_URL=http://localhost:8080 GOCHAT_E2E_ADMIN=admin \
GOCHAT_E2E_ADMIN_PASSWORD=... GOCHAT_E2E_TEAM=acme ./gradlew :core:jvmTest
```

CI (`.github/workflows/mobile.yml`) checks the Gradle wrapper, runs the
core's tests on the JVM and the iOS simulator, builds the Android app's
debug and release builds and lints it, and builds the iOS app. It runs on
every pull request, which can't merge until its `android` and `ios` jobs
pass; they skip when the change touches neither `mobile/`, the protos nor
the workflow. The main workflow's e2e job runs the core's tests against
the stack it starts.

**Release builds** are signed when these are set, as CI secrets or in the
environment: `GOCHAT_RELEASE_KEYSTORE` (the keystore's path),
`GOCHAT_RELEASE_KEYSTORE_PASSWORD`, `GOCHAT_RELEASE_KEY_ALIAS` and
`GOCHAT_RELEASE_KEY_PASSWORD`; without them the release build comes out
unsigned.

## Push notifications

The server pushes through Firebase Cloud Messaging on Android and APNs on
iOS (see "Push notifications" in the [main README](../README.md)), to the
builds of the app its credentials belong to: a build gets pushes from a
server holding the service account of the build's Firebase project, or a
signing key of the build's Apple team. After signing in, the app asks to
show notifications, once, and registers its device's token with the
session; signing out unregisters it and drops the token on the device too.
`PushService` in the core is each platform's side, `AndroidPush` and
`IosPush` in ui.

**Android.** A build has FCM once it has its Firebase app's settings
(Firebase console → Project settings → General → Your apps → the Android
app of the package you build: `io.github.ssenerg.gochat`, or
`io.github.ssenerg.gochat.debug` for debug builds). Give them as Gradle
properties, in `~/.gradle/gradle.properties`, or as environment variables:

```properties
gochat.fcm.appId=1:123456789012:android:0123456789abcdef   # GOCHAT_FCM_APP_ID
gochat.fcm.apiKey=AIza...                                  # GOCHAT_FCM_API_KEY
gochat.fcm.projectId=my-gochat                             # GOCHAT_FCM_PROJECT_ID
gochat.fcm.senderId=123456789012                           # GOCHAT_FCM_SENDER_ID
```

They are the values `google-services.json` holds, which the app does not
need. Without them the app builds and runs as before, without pushes. A
push that comes while the app runs shows as the system shows the others,
in the Messages channel of the app's notification settings.

**iOS.** The app has the Push Notifications capability: under your team,
Xcode adds it to the App ID with the bundle id. The server's APNs `topic`
is that bundle id (`GOCHAT_BUNDLE_ID`). Builds Xcode runs, and the
simulator's, get tokens of Apple's sandbox and say so when they register;
App Store and TestFlight builds get production ones. The simulator gets
pushes on a Mac with Apple silicon.

## Trying it on a phone

1. **The APK.** Each run of the Mobile workflow keeps the debug build for
   two weeks: open the run in GitHub Actions and download the
   `GoChat-debug-apk` artifact. Or build it with
   `./gradlew :androidApp:assembleDebug` and find it in
   `androidApp/build/outputs/apk/debug/`. It installs as
   `io.github.ssenerg.gochat.debug`, beside a release build (Android asks to
   allow installs from the browser or file manager once).
2. **A server with the client API**:
   - your server, at its HTTPS address; or
   - the Docker stack on your computer (Linux, or Docker Desktop on Windows
     or macOS), opened to the local network as below: `just up`, then sign
     in on the phone, on the same Wi-Fi, to `http://<the computer's IP>:8080`.
     Debug builds take plain HTTP to any address for this; release builds
     take HTTPS only.

### The Docker stack, for a phone

The phone reaches the computer at its address on the Wi-Fi, such as
`192.168.1.20`: `ipconfig` shows it on Windows (the Wi-Fi adapter's IPv4
address), `ipconfig getifaddr en0` on macOS. By default the stack only
accepts this computer, and announces its voice server at `localhost`, which
on a phone is the phone. In `.env`, with your address:

```bash
WEB_BIND=0.0.0.0
# Voice channels: on with a key of their own (`openssl rand -hex 32`), the
# signaling websocket and the voice itself at this computer's address.
VOICE_SERVER_KEY=...
VOICE_BIND=0.0.0.0
VOICE_PUBLIC_URL=ws://192.168.1.20:8090/rtc
WEB_VOICE_ORIGINS=ws://192.168.1.20:8090
VOICE_PUBLIC_IP=192.168.1.20
```

Then `just up` again. Keep `WEB_PUBLIC_URL=http://localhost:8080`: browsers
on the computer still open the chat there, and talk too. The phone needs
ports 8080 and 8090 (TCP) and 7882 (UDP and TCP, the voice itself): allow
Docker in the firewall when asked, and on Windows mark the Wi-Fi as a
private network. If the address changes, change it here too.

When voice does not connect, the app says why:

- *The server announces its voice server at localhost*: `VOICE_PUBLIC_URL`
  is still the default.
- *Cannot reach the voice server*: port 8090 is closed, or `VOICE_BIND` is
  still `127.0.0.1`.
- *The voice connection failed: your network may block it*: the phone
  cannot reach port 7882 at `VOICE_PUBLIC_IP`; check the address and the
  firewall. Some guest and office Wi-Fi networks keep devices apart.

## Servers

The apps take the server's address on the sign-in screen: `chat.example.com`
means `https://chat.example.com`. Release builds speak HTTPS only; debug
builds take plain HTTP too, for a development machine:

- the Android emulator reaches the host as `http://10.0.2.2:8080`;
- a device on USB reaches it as `http://localhost:8080` after
  `adb reverse tcp:8080 tcp:8080`;
- the iOS simulator reaches it as `http://localhost:8080`.

The session token stays on the device: on iOS in the Keychain, readable
once the phone was first unlocked and never moved to another device; on
Android in the app's private storage, sealed with a key the Android Keystore
holds, which backups and device transfers leave out.

A server can turn away apps older than a version, with `min_app_version`
in its web configuration: they ask their person to update before signing
in.
