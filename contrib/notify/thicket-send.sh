#!/usr/bin/env bash
# Carry a filed gate to a person through a thicket agent.
#
# This is the example `--notify-command` (docs/architecture.md §"Gate steps",
# decision D1): mill writes the gate to this script's standard input as JSON,
# the script renders one plain-text message, and `thicket send` hands it to a
# named agent over A2A. The agent reaches the person through its own tools.
#
# `thicket send AGENT -` takes one message on standard input and no JSON at
# all; it exits 0 when the agent accepted the message and 3 when it did not.
# This script exits with whatever `thicket send` exits with, which is what
# mill records in the run's history.
#
#   mill work --notify-command /path/to/thicket-send.sh
#   THICKET_AGENT=ops                 # which agent hears about it
#
# A `probe` notice is `mill doctor` asking whether this works. It is carried
# like any other, because a notifier nobody would notice failing is a
# notifier that is not being probed.
set -euo pipefail

agent="${THICKET_AGENT:-ops}"
notice="$(cat)"

field() {
	printf '%s' "$notice" | jq -r --arg name "$1" '.[$name] // ""'
}

kind="$(field kind)"
gate="$(field gate)"
node="$(field node)"
title="$(field title)"
body="$(field body)"

if [ "$kind" = "probe" ]; then
	message="mill notify probe: this deployment can reach ${agent}."
else
	message="$(
		printf '%s gate %s on %s: %s\n\n%s\n' \
			"$kind" "$gate" "$node" "$title" "$body"
	)"
fi

printf '%s' "$message" | thicket send "$agent" -
