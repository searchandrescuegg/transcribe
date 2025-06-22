package asr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

type ASRClient struct {
	client   *http.Client
	endpoint string
}

type TranscriptionResponse struct {
	Transcription string `json:"transcription"`
	Filename      string `json:"filename"`
}

func NewASRClient(endpoint string) *ASRClient {
	return &ASRClient{
		client:   &http.Client{},
		endpoint: endpoint,
	}
}

func (c *ASRClient) Transcribe(ctx context.Context, fileName string, fileContent io.Reader) (*TranscriptionResponse, error) {
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

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, &body)
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
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var transcriptionResp TranscriptionResponse
	err = json.NewDecoder(resp.Body).Decode(&transcriptionResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &transcriptionResp, nil
}
