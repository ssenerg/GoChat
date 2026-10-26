# Retiring `/api/v1`

The apps speak [the GoChat protocol](protocol.md) from version 0.3.0: every
call and update goes over `/api/v2/socket`, and attachments over
`/api/v2/files`. `/api/v1` stays, unchanged, until the steps below are done.

## What still uses `/api/v1`

- **Apps 0.2.x and older**, already installed: all of it. That is
  `/api/v1/rpc`, the `/api/v1/gateway` event socket, files, photos and
  `/api/v1/info`.
- **Apps 0.3.0** use three endpoints that `/api/v2` does not serve yet:
  - `GET /api/v1/info`, the `min_app_version` check at start and before
    signing in;
  - `GET /api/v1/photos/<u|t>/<id>/<version>`, people's and teams' photos;
  - `POST /api/v1/photo`, to set one's own photo. `SetPhoto` is not a
    socket call: the picture goes through the gateway's image processing.
- **Tests and docs**:
  - `test/e2e/api_test.go`;
  - `internal/web/api_test.go`;
  - the README's "Client API for native apps" section;
  - the comments in `client.proto`.
- **Protos**:
  - `ClientFrame`, `ClientReady` and `AppFrame` belong to `/api/v1` alone.
  - `ApiError`, `AppView`, `AppWatch` and `ServerInfo` are shared:
    `protocol.proto` imports them.

## Steps

1. **Serve the three under `/api/v2`.** Add `GET /api/v2/info`,
   `GET /api/v2/photos/...` and `POST /api/v2/photo` as aliases of the same
   handlers, and list them in `protocol.md`. The app release after that
   uses them, and falls back to `/api/v1/info` when a server answers 404.
2. **Ship the apps at 0.3.0** (Android `versionCode` 3, iOS build 3).
3. **Wait for adoption.** Allow the store rollout and a few weeks more.
   Self-hosted servers update on their own schedule, and apps reach every
   server.
4. **Bump `min_app_version` to `0.3`.** It lives in the web's configuration
   (`cmd/web/default.yaml`, empty today). Apps 0.2.x read
   `GET /api/v1/info` when they start and before they sign in, then ask
   their person to update. Signed-in apps hear it on their next start.
5. **Remove the rest of `/api/v1`:**
   - `/api/v1/rpc`, `/api/v1/gateway`, `/api/v1/files`, `/api/v1/photos`
     and `/api/v1/photo` (the last two once step 1 has shipped);
   - the event socket's actor, `clientSession` in
     `internal/web/gateway/client.go`, and its tests;
   - `ClientFrame`, `ClientReady` and `AppFrame`. Reserve their names in
     `client.proto` so they are never reused;
   - the v1 steps of `test/e2e`.

   **Keep `GET /api/v1/info`.** It is how an old app learns it is too old;
   without it, an old app would only show errors.
6. **Version later changes by layer.** Breaking protocol changes take a new
   layer. The server closes clients older than it supports with 4011, and
   the apps then ask their person to update. Raise `min_app_version` only
   for changes that are not about the protocol.
