package asr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type ASRClient struct {
	client         *http.Client
	endpoint       string
	defaultTimeout time.Duration
}

// TranscriptionResponse mirrors the JSON the production ASR (parakeet-tdt) returns. The
// field name `text` (not `transcription`) was the cluster's actual choice; an earlier
// version of this struct used `transcription` and silently dropped every transcript on the
// floor because json.Unmarshal couldn't find the field.
//
// NoSpeechDetected is included so callers can distinguish "audio contained no speech"
// (a radio squelch click, a brief tone) from "ASR errored": the former is normal and
// should not push the message to the DLQ.
type TranscriptionResponse struct {
	Transcription    string `json:"text"`
	NoSpeechDetected bool   `json:"no_speech_detected"`
}

func NewASRClient(endpoint string, defaultTimeout time.Duration) *ASRClient {
	return &ASRClient{
		client:         &http.Client{}, // TODO: update this to pass in a custom HTTP client if needed
		endpoint:       endpoint,
		defaultTimeout: defaultTimeout,
	}
}

func (c *ASRClient) Transcribe(ctx context.Context, fileName string, fileContent io.Reader) (*TranscriptionResponse, error) {
	transcribeCtx, cancel := context.WithTimeout(ctx, c.defaultTimeout)
	defer cancel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}

	_, err = io.Copy(part, fileContent)
	if err != nil {
		return nil, fmt.Errorf("failed to copy file content: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(transcribeCtx, "POST", c.endpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the response body so 4xx/5xx errors are debuggable from the worker logs
		// instead of just a bare status code. Truncate to a sane size so the log line stays
		// readable even when the upstream returns a large HTML error page.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ASR returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var transcriptionResp TranscriptionResponse
	err = json.NewDecoder(resp.Body).Decode(&transcriptionResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &transcriptionResp, nil
}
