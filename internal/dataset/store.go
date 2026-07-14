package dataset

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

// Store is a Postgres-backed Recorder. Writes are drained by a single background goroutine
// off bounded buffers; when a buffer is full the record is dropped (with a WARN) rather than
// blocking the caller — the dataset is a nice-to-have, never a pipeline dependency.
type Store struct {
	db           *sql.DB
	txCh         chan TranscriptionRecord
	llmCh        chan LLMInteractionRecord
	done         chan struct{}
	closeOnce    sync.Once
	wg           sync.WaitGroup
	writeTimeout time.Duration
}

// Compile-time proof Store satisfies Recorder.
var _ Recorder = (*Store)(nil)

// NewStore opens the connection, applies embedded goose migrations, and starts the writer.
func NewStore(ctx context.Context, dsn string, bufferSize int) (*Store, error) {
	if bufferSize <= 0 {
		bufferSize = 1000
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	s := &Store{
		db:           db,
		txCh:         make(chan TranscriptionRecord, bufferSize),
		llmCh:        make(chan LLMInteractionRecord, bufferSize),
		done:         make(chan struct{}),
		writeTimeout: 5 * time.Second,
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

func runMigrations(db *sql.DB) error {
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, "migrations")
}

// RecordTranscription enqueues a transcription for async insert. Non-blocking: drops on a
// full buffer. The input channel is never closed, so a late call after Close simply buffers
// or drops rather than panicking.
func (s *Store) RecordTranscription(rec TranscriptionRecord) {
	select {
	case s.txCh <- rec:
	default:
		slog.Warn("dataset: dropping transcription record (buffer full)", slog.String("s3_key", rec.S3Key))
	}
}

// RecordLLMInteraction enqueues an LLM interaction for async insert. Non-blocking.
func (s *Store) RecordLLMInteraction(rec LLMInteractionRecord) {
	select {
	case s.llmCh <- rec:
	default:
		slog.Warn("dataset: dropping llm interaction record (buffer full)", slog.String("kind", rec.Kind))
	}
}

// Close signals the writer to drain and stop, then closes the DB. Safe to call once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	s.wg.Wait()
	return s.db.Close()
}

func (s *Store) run() {
	defer s.wg.Done()
	for {
		select {
		case rec := <-s.txCh:
			s.writeTranscription(rec)
		case rec := <-s.llmCh:
			s.writeLLM(rec)
		case <-s.done:
			s.drain()
			return
		}
	}
}

// drain flushes whatever is already buffered on shutdown, then returns.
func (s *Store) drain() {
	for {
		select {
		case rec := <-s.txCh:
			s.writeTranscription(rec)
		case rec := <-s.llmCh:
			s.writeLLM(rec)
		default:
			return
		}
	}
}

func (s *Store) writeTranscription(rec TranscriptionRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()

	var capturedAt any
	if !rec.CapturedAt.IsZero() {
		capturedAt = rec.CapturedAt
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transcriptions (s3_key, talkgroup, captured_at, transcription, no_speech, is_dispatch)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.S3Key, rec.Talkgroup, capturedAt, rec.Transcription, rec.NoSpeechDetected, rec.IsDispatch,
	)
	if err != nil {
		slog.Warn("dataset: failed to insert transcription", slog.String("error", err.Error()), slog.String("s3_key", rec.S3Key))
	}
}

func (s *Store) writeLLM(rec LLMInteractionRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()

	var output any // string (cast to jsonb below) or NULL
	if len(rec.Output) > 0 {
		output = string(rec.Output)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO llm_interactions (kind, backend, model, prompt_hash, s3_key, talkgroup, input_text, output, error, latency_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)`,
		rec.Kind, rec.Backend, rec.Model, rec.PromptHash,
		nullIfEmpty(rec.S3Key), nullIfEmpty(rec.Talkgroup), rec.InputText, output, nullIfEmpty(rec.Err), rec.LatencyMS,
	)
	if err != nil {
		slog.Warn("dataset: failed to insert llm interaction", slog.String("error", err.Error()), slog.String("kind", rec.Kind))
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
