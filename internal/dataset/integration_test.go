package dataset

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// StoreSuite exercises the Postgres-backed Recorder against a real Postgres (via
// testcontainers), so the goose migrations, the JSONB output column, and the NULL handling
// for empty-string / zero-time fields are all tested against the actual database — not a
// mock. The async writer means every assertion polls until the row lands (or times out).
type StoreSuite struct {
	suite.Suite

	ctx       context.Context
	container *postgres.PostgresContainer
	store     *Store
	rawDB     *sql.DB // independent connection used only for assertions
}

func TestStoreSuite(t *testing.T) {
	suite.Run(t, new(StoreSuite))
}

func (s *StoreSuite) SetupSuite() {
	s.ctx = context.Background()

	pg, err := postgres.Run(s.ctx, "postgres:17-alpine",
		postgres.WithDatabase("transcribe"),
		postgres.WithUsername("transcribe"),
		postgres.WithPassword("transcribe"),
		postgres.BasicWaitStrategies(),
	)
	s.Require().NoError(err, "start postgres container")
	s.container = pg

	dsn, err := pg.ConnectionString(s.ctx, "sslmode=disable")
	s.Require().NoError(err)

	// NewStore applies the embedded goose migrations as part of construction, so a successful
	// return already proves migrations ran cleanly against a real Postgres.
	store, err := NewStore(s.ctx, dsn, 100)
	s.Require().NoError(err, "NewStore (runs migrations)")
	s.store = store

	s.rawDB, err = sql.Open("pgx", dsn)
	s.Require().NoError(err)
	s.Require().NoError(s.rawDB.PingContext(s.ctx))
}

func (s *StoreSuite) TearDownSuite() {
	if s.store != nil {
		_ = s.store.Close()
	}
	if s.rawDB != nil {
		_ = s.rawDB.Close()
	}
	if s.container != nil {
		_ = s.container.Terminate(s.ctx)
	}
}

func (s *StoreSuite) SetupTest() {
	// Every test above waits for its own rows to land before asserting, so by the time the
	// next test's SetupTest runs the writer is idle and truncation is safe.
	_, err := s.rawDB.ExecContext(s.ctx, "TRUNCATE transcriptions, llm_interactions RESTART IDENTITY")
	s.Require().NoError(err, "truncate between tests")
}

// eventuallyCount polls until the scalar count query returns want, or fails after the timeout.
func (s *StoreSuite) eventuallyCount(want int, query string, args ...any) {
	s.Require().Eventually(func() bool {
		var n int
		if err := s.rawDB.QueryRowContext(s.ctx, query, args...).Scan(&n); err != nil {
			return false
		}
		return n == want
	}, 5*time.Second, 25*time.Millisecond, "expected %d row(s) for query: %s", want, query)
}

func (s *StoreSuite) TestRecordTranscription_PersistsAllFields() {
	captured := time.Date(2026, 7, 8, 13, 24, 0, 0, time.UTC)
	s.store.RecordTranscription(TranscriptionRecord{
		S3Key:            "audio/1399-call.wav",
		Talkgroup:        "1399",
		CapturedAt:       captured,
		Transcription:    "engine 8171 responding",
		NoSpeechDetected: false,
		IsDispatch:       true,
	})

	s.eventuallyCount(1, "SELECT count(*) FROM transcriptions WHERE s3_key = $1", "audio/1399-call.wav")

	var (
		talkgroup  string
		text       string
		noSpeech   bool
		isDispatch bool
		capturedAt time.Time
	)
	err := s.rawDB.QueryRowContext(s.ctx,
		"SELECT talkgroup, transcription, no_speech, is_dispatch, captured_at FROM transcriptions WHERE s3_key = $1",
		"audio/1399-call.wav",
	).Scan(&talkgroup, &text, &noSpeech, &isDispatch, &capturedAt)
	s.Require().NoError(err)

	s.Equal("1399", talkgroup)
	s.Equal("engine 8171 responding", text)
	s.False(noSpeech)
	s.True(isDispatch)
	s.WithinDuration(captured, capturedAt.UTC(), time.Second)
}

func (s *StoreSuite) TestRecordTranscription_ZeroCapturedAt_StoresNull() {
	s.store.RecordTranscription(TranscriptionRecord{
		S3Key:         "audio/no-time.wav",
		Talkgroup:     "1965",
		Transcription: "copy",
		// CapturedAt left zero
	})

	s.eventuallyCount(1, "SELECT count(*) FROM transcriptions WHERE s3_key = $1", "audio/no-time.wav")

	var capturedIsNull bool
	err := s.rawDB.QueryRowContext(s.ctx,
		"SELECT captured_at IS NULL FROM transcriptions WHERE s3_key = $1", "audio/no-time.wav",
	).Scan(&capturedIsNull)
	s.Require().NoError(err)
	s.True(capturedIsNull, "zero CapturedAt must persist as SQL NULL, not a zero timestamp")
}

func (s *StoreSuite) TestRecordLLMInteraction_Success_StoresJSONBAndFields() {
	s.store.RecordLLMInteraction(LLMInteractionRecord{
		Kind:       "rescue_summary",
		Backend:    "anthropic",
		Model:      "claude-sonnet-5",
		PromptHash: "deadbeefdeadbeef",
		S3Key:      "audio/1965-tac.wav",
		Talkgroup:  "1965",
		InputText:  "=== DISPATCH ===",
		Output:     json.RawMessage(`{"headline":"hiker down near Rattlesnake Ledge"}`),
		LatencyMS:  1234,
	})

	s.eventuallyCount(1, "SELECT count(*) FROM llm_interactions WHERE kind = $1", "rescue_summary")

	// The output column must be real JSONB — query into it to prove it wasn't stored as text.
	var (
		headline string
		model    string
		latency  int64
		s3Key    string
	)
	err := s.rawDB.QueryRowContext(s.ctx,
		"SELECT output->>'headline', model, latency_ms, s3_key FROM llm_interactions WHERE kind = $1",
		"rescue_summary",
	).Scan(&headline, &model, &latency, &s3Key)
	s.Require().NoError(err)

	s.Equal("hiker down near Rattlesnake Ledge", headline)
	s.Equal("claude-sonnet-5", model)
	s.EqualValues(1234, latency)
	s.Equal("audio/1965-tac.wav", s3Key)
}

func (s *StoreSuite) TestRecordLLMInteraction_Error_NullsOutputAndEmptyFields() {
	s.store.RecordLLMInteraction(LLMInteractionRecord{
		Kind:       "dispatch_parse",
		Backend:    "openai",
		Model:      "gpt-4o-mini",
		PromptHash: "cafebabecafebabe",
		InputText:  "raw transcription",
		Err:        "chat completion error: 429",
		// no S3Key / Talkgroup / Output
	})

	s.eventuallyCount(1, "SELECT count(*) FROM llm_interactions WHERE error = $1", "chat completion error: 429")

	var (
		outputNull    bool
		s3Null        bool
		talkgroupNull bool
		errText       string
	)
	err := s.rawDB.QueryRowContext(s.ctx,
		"SELECT output IS NULL, s3_key IS NULL, talkgroup IS NULL, error FROM llm_interactions WHERE kind = $1",
		"dispatch_parse",
	).Scan(&outputNull, &s3Null, &talkgroupNull, &errText)
	s.Require().NoError(err)

	s.True(outputNull, "errored call must store NULL output")
	s.True(s3Null, "empty s3_key must persist as NULL")
	s.True(talkgroupNull, "empty talkgroup must persist as NULL")
	s.Equal("chat completion error: 429", errText)
}

// End-to-end for the LLM-capture path: the RecordingMLClient decorator writing through to a
// real Postgres, with source correlation coming from request context (the same way the
// transcribe worker stamps it). Exercises the decorator → store → DB composition that the
// isolated unit tests cover only in pieces.
func (s *StoreSuite) TestRecordingDecorator_WritesThroughToPostgres() {
	inner := &fakeInner{dispatchOut: &ml.DispatchMessages{Transcription: "cleaned"}}
	dec := NewRecordingMLClient(inner, s.store, DecoratorOptions{
		Backend:          "anthropic",
		DispatchModel:    "claude-haiku-4-5",
		AllowedCallTypes: []string{"Rescue - Trail"},
	})

	ctx := ContextWithSource(s.ctx, "audio/decorated.wav", "1399")
	out, err := dec.ParseRelevantInformationFromDispatchMessage(ctx, "raw dispatch text")
	s.Require().NoError(err)
	s.Same(inner.dispatchOut, out, "decorator must return the wrapped result unchanged")

	s.eventuallyCount(1, "SELECT count(*) FROM llm_interactions WHERE s3_key = $1", "audio/decorated.wav")

	var kind, backend, model, promptHash, input, talkgroup string
	err = s.rawDB.QueryRowContext(s.ctx,
		"SELECT kind, backend, model, prompt_hash, input_text, talkgroup FROM llm_interactions WHERE s3_key = $1",
		"audio/decorated.wav",
	).Scan(&kind, &backend, &model, &promptHash, &input, &talkgroup)
	s.Require().NoError(err)

	s.Equal("dispatch_parse", kind)
	s.Equal("anthropic", backend)
	s.Equal("claude-haiku-4-5", model)
	s.Equal("raw dispatch text", input)
	s.Equal("1399", talkgroup)
	s.NotEmpty(promptHash, "system-prompt hash must be recorded")
}
