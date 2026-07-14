-- +goose Up
-- Raw ASR transcriptions of every audio object the service transcribes (all Fire Dispatch
-- traffic plus TAC traffic during active rescues). This is the corpus for prompt refinement.
CREATE TABLE IF NOT EXISTS transcriptions (
    id            BIGSERIAL PRIMARY KEY,
    s3_key        TEXT NOT NULL,
    talkgroup     TEXT NOT NULL,
    captured_at   TIMESTAMPTZ,
    transcription TEXT NOT NULL,
    no_speech     BOOLEAN NOT NULL DEFAULT FALSE,
    is_dispatch   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_transcriptions_talkgroup ON transcriptions (talkgroup);
CREATE INDEX IF NOT EXISTS idx_transcriptions_captured_at ON transcriptions (captured_at);

-- Every LLM call the service makes, with its input, structured output (or error), model,
-- and a hash of the exact system prompt used. Join to transcriptions on s3_key to inspect
-- input->output pairs when tuning prompts.
CREATE TABLE IF NOT EXISTS llm_interactions (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL,          -- 'dispatch_parse' | 'rescue_summary'
    backend     TEXT NOT NULL,          -- 'anthropic' | 'openai'
    model       TEXT NOT NULL,
    prompt_hash TEXT NOT NULL,          -- xxhash of the system prompt actually sent
    s3_key      TEXT,                   -- source audio, when known (from request context)
    talkgroup   TEXT,
    input_text  TEXT NOT NULL,          -- the user content sent to the model
    output      JSONB,                  -- structured result; NULL when the call errored
    error       TEXT,                   -- non-empty when the call failed
    latency_ms  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_llm_interactions_kind ON llm_interactions (kind);
CREATE INDEX IF NOT EXISTS idx_llm_interactions_s3_key ON llm_interactions (s3_key);
CREATE INDEX IF NOT EXISTS idx_llm_interactions_prompt_hash ON llm_interactions (prompt_hash);

-- +goose Down
DROP TABLE IF EXISTS llm_interactions;
DROP TABLE IF EXISTS transcriptions;
