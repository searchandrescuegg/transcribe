-- +goose Up
-- Human corrections from Slack: transcript edits (dispatch + TAC) via the "Correct transcript"
-- shortcut, and free-form `correction:` thread notes. Ground-truth labels for tuning the ASR
-- cleanup and summary prompts. Join to transcriptions / llm_interactions on s3_key for raw ASR.
CREATE TABLE IF NOT EXISTS human_corrections (
    id             BIGSERIAL PRIMARY KEY,
    kind           TEXT NOT NULL,   -- 'dispatch_transcript' | 'tac_transcript' | 'operator_note'
    action         TEXT NOT NULL,   -- 'create' | 'edit' | 'delete'
    tgid           TEXT NOT NULL,
    s3_key         TEXT,
    slack_ts       TEXT NOT NULL,
    slack_user_id  TEXT NOT NULL,
    prior_text     TEXT,            -- text shown before the correction (NULL for new notes)
    corrected_text TEXT,            -- NULL for deletes
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_human_corrections_s3_key ON human_corrections (s3_key);
CREATE INDEX IF NOT EXISTS idx_human_corrections_tgid ON human_corrections (tgid);

-- +goose Down
DROP TABLE IF EXISTS human_corrections;
