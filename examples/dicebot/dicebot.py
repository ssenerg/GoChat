#!/usr/bin/env python3
"""A GoChat bot over the HTTP Bot API, with long polling and nothing but
Python's standard library: /roll answers a die's roll under a Roll again
button, and /roll 20 rolls a twenty-sided one.

    GOCHAT_URL=https://chat.example.com GOCHAT_BOT_TOKEN=b_... python3 examples/dicebot/dicebot.py

The server remembers the last update the bot confirmed, so after a restart
the bot gets what it missed. docs/bots.md walks through it.
"""

import json
import os
import random
import sys
import time
import urllib.error
import urllib.request

URL = os.environ.get("GOCHAT_URL", "").rstrip("/")
TOKEN = os.environ.get("GOCHAT_BOT_TOKEN", "")
# How long getUpdates waits for an update, at most 50 seconds.
POLL = 50


class APIError(Exception):
    def __init__(self, status, description, retry_after=0):
        super().__init__(f"{description} ({status})")
        self.status = status
        self.retry_after = retry_after


def call(method, body=None, timeout=10):
    """Makes one call of the HTTP Bot API and answers its result."""
    req = urllib.request.Request(
        f"{URL}/api/v2/bot/{method}",
        data=json.dumps(body or {}).encode(),
        headers={"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:
            return json.load(res).get("result", {})
    except urllib.error.HTTPError as e:
        try:
            answer = json.load(e)
        except ValueError:
            answer = {}
        # Calls over the request budget answer 429 with an ApiError and
        # Retry-After.
        description = answer.get("description") or answer.get("message") or e.reason
        raise APIError(e.code, description, int(e.headers.get("Retry-After") or 0)) from None


def command(text, bot):
    """Reads "/roll 20", or "/roll@dice_bot 20" addressed to this bot."""
    if not text.startswith("/"):
        return None
    word, _, args = text[1:].partition(" ")
    name, at, to = word.partition("@")
    if not name or (at and to.lower() != bot.lower()):
        return None
    return name.lower(), args.strip()


def roll(sides):
    """A roll and its button, which carries the die in its data."""
    text = f"🎲 d{sides}: {random.randint(1, sides)}"
    markup = {"rows": [{"buttons": [{"text": "Roll again", "callback_data": f"roll:{sides}"}]}]}
    return text, markup


def handle(update, me):
    m = update.get("message_posted", {}).get("message")
    if m and m.get("author", {}).get("id") != me["id"]:
        cmd = command(m.get("content", ""), me["username"])
        if cmd and cmd[0] == "roll":
            sides = int(cmd[1]) if cmd[1].isdigit() and 2 <= int(cmd[1]) <= 1000 else 6
            text, markup = roll(sides)
            call("sendMessage", {
                "channel_id": m["channel_id"], "root_id": m.get("root_id", ""),
                "content": text, "markup": markup,
            })
        return
    q = update.get("callback_query")
    if q:
        kind, _, sides = q.get("data", "").partition(":")
        if kind != "roll" or not sides.isdigit():
            call("answerCallback", {"query_id": q["id"]})
            return
        text, markup = roll(int(sides))
        msg = q["message"]
        # Editing replaces the buttons: send them again to keep them.
        call("editMessage", {
            "channel_id": msg["channel_id"], "message_id": msg["id"], "root_id": msg.get("root_id", ""),
            "content": text, "markup": markup,
        })
        call("answerCallback", {"query_id": q["id"], "text": "Rolled again."})


def main():
    if not URL or not TOKEN:
        sys.exit("set GOCHAT_URL and GOCHAT_BOT_TOKEN")
    me = call("getMe")["user"]
    print(f"signed in as @{me['username']}", flush=True)
    call("setCommands", {"commands": [{"name": "roll", "description": "Roll a die: /roll or /roll 20"}]})

    # 0 starts after the last update the server knows the bot handled.
    offset, delay = 0, 1
    while True:
        try:
            res = call("getUpdates", {"offset": offset, "timeout": POLL}, timeout=POLL + 10)
        except APIError as e:
            if e.status == 401:
                sys.exit("the token is not valid: was it reset?")
            # 409: another copy of the bot is polling, or a webhook is set.
            print(f"getUpdates: {e}", file=sys.stderr, flush=True)
            time.sleep(e.retry_after or delay)
            delay = min(delay * 2, 60)
            continue
        except OSError as e:
            print(f"getUpdates: {e}", file=sys.stderr, flush=True)
            time.sleep(delay)
            delay = min(delay * 2, 60)
            continue
        delay = 1
        for u in res.get("updates", []):
            # One update the bot cannot handle must not hold back the rest.
            try:
                handle(u, me)
            except Exception as e:
                print(f"update {u['pts']}: {e!r}", file=sys.stderr, flush=True)
            # Numbers are strings in JSON: int64 in protobuf's mapping.
            offset = int(u["pts"]) + 1
        if res.get("too_long"):
            print("updates were lost while the bot was away", file=sys.stderr, flush=True)
            offset = int(res["pts"]) + 1


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        pass
