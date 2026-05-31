package ai

// oversight/ai/ai.go
//
// AI oversight client — proposes diagnoses and fixes for bot errors.
//
// Supports two backends:
//   1. Claude API (cloud) — set AI_BACKEND=claude and CLAUDE_API_KEY
//   2. Local OWL (Hermes) — set AI_BACKEND=local and LOCAL_AI_URL
//
// Switch backends via .env — no code changes needed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config configures which AI backend to use.
type Config struct {
	Backend      string
	ClaudeAPIKey string
	ClaudeModel  string
	LocalURL     string
	LocalModel   string
}

// Proposal is the structured output from the AI.
type Proposal struct {
	Diagnosis       string `json:"diagnosis"`
	Confidence      int    `json:"confidence"`
	FixFile         string `json:"fix_file"`
	FixDescription  string `json:"fix_description"`
	FixDiff         string `json:"fix_diff"`
	Risks           string `json:"risks"`
	RequiresRestart bool   `json:"requires_restart"`
	Raw             string
}

// Client is the AI oversight client.
type Client struct {
	cfg    Config
	client *http.Client
}

// NewClient creates an AI client from config.
func NewClient(cfg Config) *Client {
	if cfg.Backend == "" {
		cfg.Backend = os.Getenv("AI_BACKEND")
		if cfg.Backend == "" {
			cfg.Backend = "local"
		}
	}
	if cfg.ClaudeAPIKey == "" {
		cfg.ClaudeAPIKey = os.Getenv("CLAUDE_API_KEY")
	}
	if cfg.ClaudeModel == "" {
		cfg.ClaudeModel = "claude-opus-4-6"
	}
	if cfg.LocalURL == "" {
		cfg.LocalURL = os.Getenv("LOCAL_AI_URL")
		if cfg.LocalURL == "" {
			cfg.LocalURL = "http://localhost:11434"
		}
	}
	if cfg.LocalModel == "" {
		cfg.LocalModel = os.Getenv("LOCAL_AI_MODEL")
		if cfg.LocalModel == "" {
			cfg.LocalModel = "llama3"
		}
	}
	return &Client{
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Backend returns the active backend name.
func (c *Client) Backend() string {
	return c.cfg.Backend
}

// IsLocal returns true if using local AI.
func (c *Client) IsLocal() bool {
	return c.cfg.Backend == "local"
}

// ProposeFix sends error context to the AI and returns a fix proposal.
func (c *Client) ProposeFix(ctx context.Context, alertMsg, botName, level, detail, occurredAt, logContext string) (*Proposal, error) {
	prompt := buildPrompt(alertMsg, botName, level, detail, occurredAt, logContext)

	if c.cfg.Backend == "claude" {
		return c.proposeClaude(ctx, prompt)
	}
	return c.proposeLocal(ctx, prompt)
}

func buildPrompt(alertMsg, botName, level, detail, occurredAt, logContext string) string {
	return fmt.Sprintf(
		`You are an expert Go developer specialising in blockchain/DeFi systems. A crypto arbitrage bot has encountered an error.

Bot: %s | Level: %s | Message: %s | Detail: %s | Time: %s

Recent logs:
%s

Respond in valid JSON: {"diagnosis":"...","confidence":N,"fix_file":"...","fix_description":"...","fix_diff":"...","risks":"...","requires_restart":true}`,
		botName, level, alertMsg, detail, occurredAt, logContext,
	)
}

func (c *Client) proposeClaude(ctx context.Context, prompt string) (*Proposal, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model":      c.cfg.ClaudeModel,
		"max_tokens": 2000,
		"system":     "You are an expert Go developer specialising in blockchain/DeFi systems.",
		"messages":   []msg{{Role: "user", Content: prompt}},
	})

	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.ClaudeAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Content []struct{ Text string `json:"text"` } `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Content) == 0 {
		return nil, fmt.Errorf("empty Claude response")
	}
	return parseProposal(result.Content[0].Text), nil
}

func (c *Client) proposeLocal(ctx context.Context, prompt string) (*Proposal, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"model":     c.cfg.LocalModel,
		"prompt":    "You are an expert Go developer specialising in blockchain/DeFi systems.\n\n" + prompt,
		"stream":    false,
		"max_tokens": 2000,
	})

	endpoint := c.cfg.LocalURL
	if !strings.HasSuffix(endpoint, "/api/generate") {
		endpoint += "/api/generate"
	}

	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("local AI error: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil && result.Response != "" {
		return parseProposal(result.Response), nil
	}
	return parseProposal(string(respBody)), nil
}

func parseProposal(raw string) *Proposal {
	raw = strings.TrimSpace(raw)
	p := &Proposal{Raw: raw}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return p
	}
	json.Unmarshal([]byte(raw[start:end+1]), p)
	return p
}
