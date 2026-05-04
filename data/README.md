# Test fixtures

The `transcription*.txt` files in this directory are sample dispatch transcripts used by
`cmd/test-transcription` to exercise the OpenAI parsing path end-to-end without depending on
S3 / Pulsar / ASR.

## Source

These transcripts are excerpts of NORCOM Fire Dispatch radio traffic, which is broadcast in
the clear and publicly archived on [OpenMHz](https://openmhz.com/system/psernlfp). The
content is therefore already public, but checking it into the repo creates a durable,
indexed copy — keep that in mind before adding new fixtures.

## When adding new fixtures

- Prefer transcripts that exercise a new call type or edge case (e.g. multiple calls in one
  message, a call with an ambiguous TAC channel, a malformed transcription).
- Avoid fixtures that pin down a single household to a specific incident (e.g. a unit number
  at a residential address paired with a medical aid call). The dispatch broadcast is public
  but the combination is more identifying than the original audio.
- The format is free-form text — whatever the ASR service produces. The parser/ML are
  responsible for cleaning it up, so messy input is fine and even desirable.

## Usage

```bash
TRANSCRIPTION_FILE=data/transcription.txt \
OPENAI_API_KEY=... \
OPENAI_MODEL_NAME=gpt-4o-mini \
go run ./cmd/test-transcription
```
