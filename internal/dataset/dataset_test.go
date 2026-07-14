package dataset

import (
	"context"
	"errors"
	"testing"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInner struct {
	dispatchOut *ml.DispatchMessages
	dispatchErr error
	summaryOut  *ml.RescueSummary
	summaryErr  error
	cleanupOut  *ml.TACCleanupResult
	cleanupErr  error
}

func (f *fakeInner) ParseRelevantInformationFromDispatchMessage(context.Context, string) (*ml.DispatchMessages, error) {
	return f.dispatchOut, f.dispatchErr
}

func (f *fakeInner) SummarizeRescue(context.Context, ml.RescueSummaryInput) (*ml.RescueSummary, error) {
	return f.summaryOut, f.summaryErr
}

func (f *fakeInner) CleanTACTranscript(context.Context, ml.TACCleanupInput) (*ml.TACCleanupResult, error) {
	return f.cleanupOut, f.cleanupErr
}

type fakeRecorder struct {
	llm []LLMInteractionRecord
}

func (r *fakeRecorder) RecordTranscription(TranscriptionRecord)     {}
func (r *fakeRecorder) RecordLLMInteraction(l LLMInteractionRecord) { r.llm = append(r.llm, l) }
func (r *fakeRecorder) Close() error                                { return nil }

func TestRecordingMLClient_DispatchSuccess_RecordsAndPassesThrough(t *testing.T) {
	inner := &fakeInner{dispatchOut: &ml.DispatchMessages{Transcription: "cleaned"}}
	rec := &fakeRecorder{}
	dec := NewRecordingMLClient(inner, rec, DecoratorOptions{Backend: "anthropic", DispatchModel: "claude-haiku-4-5"})

	ctx := ContextWithSource(context.Background(), "s3/key.wav", "1399")
	out, err := dec.ParseRelevantInformationFromDispatchMessage(ctx, "raw transcription")

	require.NoError(t, err)
	assert.Same(t, inner.dispatchOut, out, "result must pass through unchanged")

	require.Len(t, rec.llm, 1)
	got := rec.llm[0]
	assert.Equal(t, "dispatch_parse", got.Kind)
	assert.Equal(t, "anthropic", got.Backend)
	assert.Equal(t, "claude-haiku-4-5", got.Model)
	assert.Equal(t, "raw transcription", got.InputText)
	assert.Equal(t, "s3/key.wav", got.S3Key, "s3 key must come from context")
	assert.Equal(t, "1399", got.Talkgroup)
	assert.NotEmpty(t, got.PromptHash)
	assert.Empty(t, got.Err)
	assert.NotNil(t, got.Output, "successful call records structured output")
}

func TestRecordingMLClient_DispatchError_RecordsErrorNilOutput(t *testing.T) {
	inner := &fakeInner{dispatchErr: errors.New("boom")}
	rec := &fakeRecorder{}
	dec := NewRecordingMLClient(inner, rec, DecoratorOptions{Backend: "openai", DispatchModel: "gpt-4o-mini"})

	out, err := dec.ParseRelevantInformationFromDispatchMessage(context.Background(), "raw")

	require.Error(t, err)
	assert.Nil(t, out)

	require.Len(t, rec.llm, 1)
	got := rec.llm[0]
	assert.Equal(t, "boom", got.Err)
	assert.Nil(t, got.Output, "errored call records no output")
	assert.Empty(t, got.S3Key, "no source in context → empty s3 key")
}

func TestRecordingMLClient_Summary_RecordsSummaryKind(t *testing.T) {
	inner := &fakeInner{summaryOut: &ml.RescueSummary{Headline: "hiker down"}}
	rec := &fakeRecorder{}
	dec := NewRecordingMLClient(inner, rec, DecoratorOptions{Backend: "anthropic", SummaryModel: "claude-sonnet-5"})

	_, err := dec.SummarizeRescue(context.Background(), ml.RescueSummaryInput{DispatchTranscription: "dispatch text"})
	require.NoError(t, err)

	require.Len(t, rec.llm, 1)
	got := rec.llm[0]
	assert.Equal(t, "rescue_summary", got.Kind)
	assert.Equal(t, "claude-sonnet-5", got.Model)
	assert.Contains(t, got.InputText, "dispatch text", "input records the built summary prompt")
}

func TestRecordingMLClient_Cleanup_RecordsCleanupKind(t *testing.T) {
	inner := &fakeInner{cleanupOut: &ml.TACCleanupResult{CleanedText: "TAC2 Norway Hill Trail"}}
	rec := &fakeRecorder{}
	dec := NewRecordingMLClient(inner, rec, DecoratorOptions{Backend: "anthropic", CleanupModel: "claude-haiku-4-5"})

	out, err := dec.CleanTACTranscript(context.Background(), ml.TACCleanupInput{Text: "tac two norwell hill trail"})
	require.NoError(t, err)
	assert.Same(t, inner.cleanupOut, out, "result must pass through unchanged")

	require.Len(t, rec.llm, 1)
	got := rec.llm[0]
	assert.Equal(t, "tac_cleanup", got.Kind)
	assert.Equal(t, "claude-haiku-4-5", got.Model, "cleanup is recorded under the cleanup model")
	assert.Contains(t, got.InputText, "norwell hill trail", "input records the built cleanup prompt")
	assert.NotNil(t, got.Output)
}
