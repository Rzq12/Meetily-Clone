package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"notulensi/internal/template"
)

var ErrEmpty = errors.New("llm returned no visible minutes")

type Client struct {
	BaseURL string
	Model   string
	APIKey  string

	CallTimeout time.Duration
	RetryDelay  time.Duration
}

func NewClient(baseURL, model, apiKey string) *Client {
	return &Client{BaseURL: baseURL, Model: model, APIKey: apiKey, CallTimeout: 5 * time.Minute, RetryDelay: 10 * time.Second}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *Client) complete(ctx context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.CallTimeout)
	defer cancel()
	payload, err := json.Marshal(map[string]any{
		"model":       c.Model,
		"messages":    []message{{"system", system}, {"user", user}},
		"temperature": 0.2,
		"stream":      false,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode llm response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", ErrEmpty
	}
	return out.Choices[0].Message.Content, nil
}

// call runs one completion, retrying once after RetryDelay (FR-007), and returns cleaned Markdown.
func (c *Client) call(ctx context.Context, stage, system, user string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			slog.Warn("llm call retry", "stage", stage, "err", lastErr)
			select {
			case <-time.After(c.RetryDelay):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		raw, err := c.complete(ctx, system, user)
		if err != nil {
			lastErr = err
			continue
		}
		cleaned, ok := Clean(raw)
		if !ok {
			lastErr = ErrEmpty
			continue
		}
		return cleaned, nil
	}
	return "", fmt.Errorf("%s: %w", stage, lastErr)
}

// Generate follows Meetily's map-reduce: chunk summaries and a combine step only when the
// transcript reaches the token threshold, then one final call that fills the template.
func (c *Client) Generate(ctx context.Context, transcript, userContext string, sections []template.Section, tokenThreshold int) (string, error) {
	source := transcript
	if EstimateTokens(transcript) >= tokenThreshold {
		chunks := ChunkText(transcript, tokenThreshold-300, 100)
		summaries := make([]string, 0, len(chunks))
		for i, chunk := range chunks {
			s, err := c.call(ctx, fmt.Sprintf("chunk %d/%d", i+1, len(chunks)), systemSummarizer, ChunkPrompt(chunk))
			if err != nil {
				return "", err
			}
			summaries = append(summaries, s)
		}
		combined, err := c.call(ctx, "combine", systemSummarizer, CombinePrompt(summaries))
		if err != nil {
			return "", err
		}
		source = combined
	}
	return c.call(ctx, "final", FinalSystemPrompt(sections), FinalUserPrompt(source, userContext))
}
