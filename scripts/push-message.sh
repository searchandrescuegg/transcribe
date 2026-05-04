#!/bin/bash
#
# Synthetic-trigger wrapper. Publishes Pulsar S3 events for every .wav fixture in the
# fixture directory, in timestamp order, with a delay between each — driving the local
# pipeline end-to-end against the docker-compose stack.
#
# Defaults match docker-compose.yml. Pass -- followed by extra args to forward them to
# the cmd/push-message tool, e.g.:
#
#   ./scripts/push-message.sh -- -delay 1s
#   ./scripts/push-message.sh -- -file 1399-1777832036_852162500.0-call_001.wav
#
set -e

PULSAR_URL="${PULSAR_URL:-pulsar://localhost:6650}"
TOPIC="${PULSAR_INPUT_TOPIC:-public/transcribe/file-queue}"
DIR="${FIXTURE_DIR:-docker/s3ninja/audio}"

echo "Pulsar URL : $PULSAR_URL"
echo "Topic      : $TOPIC"
echo "Fixture dir: $DIR"
echo

go run ./cmd/push-message -url "$PULSAR_URL" -topic "$TOPIC" -dir "$DIR" "$@"
