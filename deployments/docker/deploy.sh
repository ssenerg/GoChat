#!/bin/sh
# Deploys GoChat on this server: pulls IMAGE@DIGEST, applies the migrations
# and restarts the server and the web gateway with it, and the voice server
# when .env has a VOICE_SERVER_KEY. This script's directory holds
# docker-compose.yml and .env. If the server or the web gateway do not come
# up healthy, the image that ran before comes back and the script fails; a
# voice server that does not is reported and left down.
#
#   deploy.sh IMAGE [DIGEST]   e.g. deploy.sh ghcr.io/OWNER/gochat sha256:...
#
# CI runs it over SSH with a key that may run nothing else, with IMAGE set
# in authorized_keys and DIGEST sent by CI (see the README):
#
#   restrict,command="/opt/apps/gochat/deploy.sh ghcr.io/OWNER/gochat" ssh-ed25519 ...
#
# What it prints may end up in public CI logs, so the output of the
# migrations, the server, the web gateway and the voice server, which can name clients and
# users, goes to the journal: journalctl -t gochat-deploy
set -eu
cd "$(dirname "$0")"

fail() {
	echo "deploy: $*" >&2
	exit 1
}

image=${1:-}
digest=${2:-${SSH_ORIGINAL_COMMAND:-}}
[ -n "$image" ] || fail "usage: deploy.sh IMAGE [DIGEST]"
# The caller chooses the digest of an image in IMAGE, and nothing else.
hex=${digest#sha256:}
case $hex in
*[!0-9a-f]*) hex= ;;
esac
if [ "$digest" != "sha256:$hex" ] || [ ${#hex} -ne 64 ]; then
	fail "the digest must be sha256: and 64 lowercase hexadecimal digits"
fi
[ -f .env ] || fail "there is no .env in $PWD: create it from .env.template"

compose() { docker compose --file docker-compose.yml "$@"; }
journal() { logger --tag gochat-deploy; }

docker pull --quiet "$image@$digest" >/dev/null
# The image the server runs now stays at hand as gochat:previous.
running=$(docker inspect --format '{{.Image}}' "$(compose ps --all --quiet server 2>/dev/null)" 2>/dev/null || true)
if [ -n "$running" ]; then
	docker image tag "$running" gochat:previous
fi
docker image tag "$image@$digest" gochat:latest
docker image rm "$image@$digest" >/dev/null 2>&1 || true

started() {
	# The stores keep running across deploys; only the app is replaced.
	compose up --detach --no-build --wait --wait-timeout 300 scylla minio || return 1
	compose run --rm --no-deps scylla-init >/dev/null || return 1
	compose run --rm --no-deps minio-init >/dev/null || return 1
	out=$(compose run --rm --no-deps migrate 2>&1) || {
		printf '%s\n' "$out" | journal
		return 1
	}
	printf '%s\n' "$out" | journal
	compose up --detach --no-build --no-deps --force-recreate --wait --wait-timeout 180 server || return 1
	compose up --detach --no-build --no-deps --force-recreate --wait --wait-timeout 120 web
}

# Voice is optional: the voice server runs when .env gives it its key. One
# that does not come up is reported but rolls nothing back, since chat does
# not depend on it.
voice_on() { grep -Eq '^VOICE_SERVER_KEY=.{32,}' .env; }

voice() {
	if ! voice_on; then
		compose rm --stop --force voice >/dev/null 2>&1 || true
		return 0
	fi
	compose up --detach --no-build --no-deps --force-recreate --wait --wait-timeout 120 voice
}

if started; then
	# Images replaced by earlier deploys are not needed any more.
	docker image prune --force --filter label=org.opencontainers.image.title=gochat >/dev/null
	revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' gochat:latest)
	if voice; then
		echo "deploy: gochat $revision is running"
	else
		compose logs --no-color --tail 100 voice 2>&1 | journal || true
		echo "deploy: gochat $revision is running, but its voice server did not come up healthy, so voice channels are down. Its logs are on the server: journalctl -t gochat-deploy" >&2
	fi
	exit 0
fi

compose logs --no-color --tail 100 server web voice 2>&1 | journal || true
logs="Their logs are on the server: journalctl -t gochat-deploy"
[ -n "$running" ] || fail "gochat did not come up healthy. $logs"
docker image tag gochat:previous gochat:latest
if compose up --detach --no-build --no-deps --force-recreate --wait --wait-timeout 180 server web >/dev/null 2>&1; then
	voice >/dev/null 2>&1 || true
	fail "gochat did not come up healthy, so the image that ran before is back. $logs"
fi
fail "gochat did not come up healthy, and neither did the image that ran before. $logs"
