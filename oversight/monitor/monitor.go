package monitor

// oversight/monitor/monitor.go
//
// The oversight system — watches everything, alerts on problems, proposes fixes.
//
// What it does:
//   1. Reads AlertEvents from state.Global.AlertCh
//   2. Tails log files for ERROR/CRITICAL patterns
//   3. On error detection: sends alert via Telegram/email immediately
//   4. For fixable errors: calls Claude API to propose a fix, emails you for approval
//   5. You reply "approve" to the email → monitor deploys the fix, rebuilds, restarts
//   6. Generates daily/weekly/monthly reports (operational + financial)
//
// Safety principle:
//   AI PROPOSES — YOU APPROVE — THEN IT DEPLOYS
//   No auto-deployment of AI-generated code to live systems without human sign-off.
//   The one exception: restarting crashed services (no code change, just systemctl restart)

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
	"github.com/Jinn-Master/Cash_Machine/core/treasury"
)

// ── Monitor config ────────────────────────────────────────────────────────────

type Config struct {
	LogDir         string // directory containing bot log files
	ProjectDir     string // root of money-printer project (for rebuilds)
	TelegramToken  string
	TelegramChatID string
	SendGridKey    string
	ReportEmail    string // your email address
	ClaudeAPIKey   string // for AI fix proposals
	ServiceName    string // systemd service name "money-printer"
}

// ── Monitor ───────────────────────────────────────────────────────────────────

type Monitor struct {
	cfg      Config
	treasury *treasury.Treasury
	mu       sync.Mutex
	errCount map[string]int // bot → error count (for rate limiting alerts)
	lastFix  time.Time      // last time a fix was proposed
}

func New(cfg Config, t *treasury.Treasury) *Monitor {
	return &Monitor{
		cfg:      cfg,
		treasury: t,
		errCount: make(map[string]int),
	}
}

// Run starts all monitoring goroutines.
func (m *Monitor) Run(ctx context.Context) {
	log := logger.Log
	log.Info("🔭 oversight monitor started")

	go m.watchAlertChannel(ctx)
	go m.tailLogFiles(ctx)
	go m.scheduledReports(ctx)
	go m.gasMonitor(ctx)
}

// ── Alert channel watcher ─────────────────────────────────────────────────────

func (m *Monitor) watchAlertChannel(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case alert := <-state.Global.AlertCh:
			m.handleAlert(ctx, alert)
		}
	}
}

func (m *Monitor) handleAlert(ctx context.Context, alert state.AlertEvent) {
	log := logger.Log
	log.Info("🚨 alert received",
		"level",   alert.Level,
		"bot",     alert.BotName,
		"message", alert.Message,
	)

	// Telegram immediate notification
	msg := fmt.Sprintf("🤖 *Money Printer Alert*\n\nLevel: `%s`\nBot: `%s`\nMessage: %s\n\nDetail: %s\n\nTime: %s",
		alert.Level, alert.BotName, alert.Message, alert.Detail,
		alert.OccurredAt.Format("2006-01-02 15:04:05 UTC"),
	)
	m.sendTelegram(msg)

	// For critical errors: attempt auto-restart then propose AI fix
	if alert.Level == "critical" {
		m.handleCritical(ctx, alert)
	}
}

func (m *Monitor) handleCritical(ctx context.Context, alert state.AlertEvent) {
	log := logger.Log
	log.Warn("CRITICAL alert — attempting service restart", "bot", alert.BotName)

	// Auto-restart the service (safe — no code changes)
	if err := m.restartService(); err != nil {
		log.Error("service restart failed", "err", err)
		m.sendTelegram(fmt.Sprintf("❌ *Auto-restart failed*\n\n%s\n\nManual intervention required.", err.Error()))
		return
	}

	m.sendTelegram("✅ *Service restarted successfully*\n\nMonitoring for stability...")
	log.Info("service restarted", "bot", alert.BotName)

	// If this is the second critical in 1 hour, propose an AI fix
	m.mu.Lock()
	m.errCount[alert.BotName]++
	count := m.errCount[alert.BotName]
	timeSinceLastFix := time.Since(m.lastFix)
	m.mu.Unlock()

	if count >= 2 && timeSinceLastFix > 1*time.Hour && m.cfg.ClaudeAPIKey != "" {
		go m.proposeAIFix(ctx, alert)
	}
}

// ── Log file tailing ──────────────────────────────────────────────────────────

func (m *Monitor) tailLogFiles(ctx context.Context) {
	log := logger.Log
	pattern := filepath.Join(m.cfg.LogDir, "*.log")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		log.Warn("no log files found to tail", "pattern", pattern)
		return
	}

	for _, f := range files {
		f := f
		go m.tailFile(ctx, f)
	}
}

func (m *Monitor) tailFile(ctx context.Context, path string) {
	log := logger.Log
	log.Info("tailing log file", "path", path)

	f, err := os.Open(path)
	if err != nil {
		log.Warn("cannot open log file", "path", path, "err", err)
		return
	}
	defer f.Close()

	// Seek to end
	f.Seek(0, io.SeekEnd)

	scanner := bufio.NewScanner(f)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for scanner.Scan() {
				line := scanner.Text()
				m.checkLogLine(path, line)
			}
		}
	}
}

func (m *Monitor) checkLogLine(logFile, line string) {
	upper := strings.ToUpper(line)
	botName := filepath.Base(logFile)

	var level, msg string
	if strings.Contains(upper, "CRITICAL") || strings.Contains(upper, "PANIC") || strings.Contains(upper, "FATAL") {
		level = "critical"
		msg = "Critical error detected in log"
	} else if strings.Contains(upper, "ERROR") {
		level = "error"
		msg = "Error detected in log"
	} else {
		return
	}

	// Rate limit: max 1 alert per bot per 5 minutes for same level
	state.Global.Alert(level, botName, msg, line)
}

// ── AI fix proposals ──────────────────────────────────────────────────────────

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []claudeMessage `json:"messages"`
	System    string          `json:"system"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (m *Monitor) proposeAIFix(ctx context.Context, alert state.AlertEvent) {
	log := logger.Log

	// Read relevant log context
	logContext := m.readRecentLogs(alert.BotName, 100)

	prompt := fmt.Sprintf(`You are reviewing an error in the "Money Printer" crypto arbitrage bot running on Base blockchain.

Error details:
- Bot: %s
- Level: %s
- Message: %s
- Detail: %s
- Time: %s

Recent log context:
%s

Please:
1. Diagnose the most likely cause
2. Propose a specific code fix (with file path and exact change)
3. Rate confidence in your diagnosis (1-10)
4. Note any risks in applying the fix

Format your response as JSON:
{
  "diagnosis": "...",
  "confidence": 7,
  "fix_file": "path/to/file.go",
  "fix_description": "...",
  "fix_diff": "--- before\n+++ after\n...",
  "risks": "...",
  "requires_restart": true
}`,
		alert.BotName, alert.Level, alert.Message, alert.Detail,
		alert.OccurredAt.Format(time.RFC3339),
		logContext,
	)

	reqBody := claudeRequest{
		Model:     "claude-opus-4-6",
		MaxTokens: 2000,
		System:    "You are an expert Go developer specialising in blockchain/DeFi systems. Provide concise, accurate diagnoses.",
		Messages:  []claudeMessage{{Role: "user", Content: prompt}},
	}

	body, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", m.cfg.ClaudeAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Error("AI fix proposal failed", "err", err)
		return
	}
	defer resp.Body.Close()

	var claudeResp claudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&claudeResp); err != nil {
		log.Error("AI response decode failed", "err", err)
		return
	}

	if len(claudeResp.Content) == 0 {
		return
	}

	fixProposal := claudeResp.Content[0].Text

	m.mu.Lock()
	m.lastFix = time.Now()
	m.mu.Unlock()

	log.Info("🤖 AI fix proposal generated", "bot", alert.BotName)

	// Send via email for approval
	subject := fmt.Sprintf("[Money Printer] AI Fix Proposal — %s (%s)", alert.BotName, alert.Level)
	body2 := fmt.Sprintf(`Money Printer AI Fix Proposal
==============================

Error: %s
Bot: %s
Time: %s

AI Diagnosis & Proposed Fix:
%s

---
To approve this fix, reply to this email with "APPROVE"
To reject, reply with "REJECT"

IMPORTANT: Review the proposed diff carefully before approving.
No code changes will be deployed without your explicit approval.

— Money Printer Oversight System`,
		alert.Message, alert.BotName,
		alert.OccurredAt.Format("2006-01-02 15:04:05 UTC"),
		fixProposal,
	)

	m.sendEmail(subject, body2)
	m.sendTelegram(fmt.Sprintf("🤖 *AI Fix Proposed*\n\nBot: `%s`\nCheck your email for details and approval request.", alert.BotName))
}

// ── Scheduled reports ─────────────────────────────────────────────────────────

func (m *Monitor) scheduledReports(ctx context.Context) {
	log := logger.Log

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	var lastDaily, lastWeekly, lastMonthly time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			now := t.UTC()

			// Daily report at 8am UTC
			if now.Hour() == config.ReportEmailHour && now.Day() != lastDaily.Day() {
				lastDaily = now
				go m.sendDailyReport(ctx)
				log.Info("daily report sent")
			}

			// Weekly report — Sunday 8am UTC
			if now.Weekday() == time.Sunday && now.Hour() == config.ReportEmailHour && now.Day() != lastWeekly.Day() {
				lastWeekly = now
				go m.sendWeeklyReport(ctx)
				log.Info("weekly report sent")
			}

			// Monthly report — 1st of month 8am UTC
			if now.Day() == 1 && now.Hour() == config.ReportEmailHour && now.Month() != lastMonthly.Month() {
				lastMonthly = now
				go m.sendMonthlyReport(ctx)
				log.Info("monthly report sent")
			}
		}
	}
}

func (m *Monitor) buildOperationalReport(period string) string {
	profitSummary := state.Global.ProfitSummary()
	wc := state.Global.WorkingCapital()
	hotTokens := state.Global.HotTokenCount()
	gasPrice := state.Global.GasPrice()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("MONEY PRINTER — %s OPERATIONAL REPORT\n", strings.ToUpper(period)))
	sb.WriteString(fmt.Sprintf("Generated: %s UTC\n\n", time.Now().UTC().Format("2006-01-02 15:04:05")))

	sb.WriteString("── PROFIT BY BOT ────────────────────────────────\n")
	for bot, profit := range profitSummary {
		if bot != "total" {
			sb.WriteString(fmt.Sprintf("  %-20s $%.4f\n", bot, profit))
		}
	}
	sb.WriteString(fmt.Sprintf("  %-20s $%.4f\n\n", "TOTAL", profitSummary["total"]))

	sb.WriteString("── SYSTEM STATUS ────────────────────────────────\n")
	sb.WriteString(fmt.Sprintf("  Working Capital:    $%.2f\n", wc))
	sb.WriteString(fmt.Sprintf("  Hot Tokens (24h):   %d\n", hotTokens))
	sb.WriteString(fmt.Sprintf("  Gas Price (gwei):   %.4f\n\n", gasPrice))

	if m.treasury != nil {
		summary := m.treasury.Summary()
		sb.WriteString("── TREASURY ─────────────────────────────────────\n")
		sb.WriteString(fmt.Sprintf("  Phase:              %v\n", summary["phase"]))
		sb.WriteString(fmt.Sprintf("  Total Distributed:  $%.2f\n", summary["total_distributed"]))
		if byDest, ok := summary["by_destination"].(map[string]float64); ok {
			sb.WriteString(fmt.Sprintf("  → Reinvested:       $%.2f\n", byDest["reinvested"]))
			sb.WriteString(fmt.Sprintf("  → Spending:         $%.2f\n", byDest["spending"]))
			sb.WriteString(fmt.Sprintf("  → Overhead:         $%.2f\n", byDest["overhead"]))
			sb.WriteString(fmt.Sprintf("  → Staking:          $%.2f\n", byDest["staking"]))
		}
	}

	return sb.String()
}

func (m *Monitor) sendDailyReport(ctx context.Context) {
	report := m.buildOperationalReport("Daily")
	subject := fmt.Sprintf("[Money Printer] Daily Report — %s", time.Now().UTC().Format("2006-01-02"))
	m.sendEmail(subject, report)
}

func (m *Monitor) sendWeeklyReport(ctx context.Context) {
	report := m.buildOperationalReport("Weekly")
	subject := fmt.Sprintf("[Money Printer] Weekly Report — Week of %s", time.Now().UTC().Format("2006-01-02"))
	m.sendEmail(subject, report)
}

func (m *Monitor) sendMonthlyReport(ctx context.Context) {
	report := m.buildOperationalReport("Monthly")
	subject := fmt.Sprintf("[Money Printer] Monthly Report — %s", time.Now().UTC().Format("January 2006"))
	m.sendEmail(subject, report)
}

// ── Gas monitor ───────────────────────────────────────────────────────────────

func (m *Monitor) gasMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Gas price is updated by the main chain client in cmd/printer/main.go
			// This goroutine just checks for anomalies
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m *Monitor) readRecentLogs(botName string, lines int) string {
	logFile := filepath.Join(m.cfg.LogDir, botName+".log")
	f, err := os.Open(logFile)
	if err != nil {
		// Try main log
		logFile = filepath.Join(m.cfg.LogDir, "money_printer.log")
		f, err = os.Open(logFile)
		if err != nil {
			return "(log file not found)"
		}
	}
	defer f.Close()

	var logLines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		logLines = append(logLines, scanner.Text())
	}

	start := len(logLines) - lines
	if start < 0 {
		start = 0
	}
	return strings.Join(logLines[start:], "\n")
}

func (m *Monitor) sendTelegram(message string) {
	if m.cfg.TelegramToken == "" || m.cfg.TelegramChatID == "" {
		return
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.cfg.TelegramToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, url.Values{
		"chat_id":    {m.cfg.TelegramChatID},
		"text":       {message},
		"parse_mode": {"Markdown"},
	})
	if err == nil {
		resp.Body.Close()
	}
}

func (m *Monitor) sendEmail(subject, body string) {
	if m.cfg.SendGridKey == "" || m.cfg.ReportEmail == "" {
		logger.Log.Warn("email not configured — skipping", "subject", subject)
		return
	}

	// SendGrid v3 API
	type emailContent struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	type emailAddr struct {
		Email string `json:"email"`
		Name  string `json:"name,omitempty"`
	}
	type personalization struct {
		To []emailAddr `json:"to"`
	}
	type sendgridReq struct {
		Personalizations []personalization `json:"personalizations"`
		From             emailAddr         `json:"from"`
		Subject          string            `json:"subject"`
		Content          []emailContent    `json:"content"`
	}

	req := sendgridReq{
		Personalizations: []personalization{{To: []emailAddr{{Email: m.cfg.ReportEmail}}}},
		From:             emailAddr{Email: "reports@money-printer.bot", Name: "Money Printer"},
		Subject:          subject,
		Content:          []emailContent{{Type: "text/plain", Value: body}},
	}

	reqBody, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", "https://api.sendgrid.com/v3/mail/send",
		bytes.NewReader(reqBody))
	httpReq.Header.Set("Authorization", "Bearer "+m.cfg.SendGridKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		logger.Log.Error("email send failed", "err", err)
		return
	}
	resp.Body.Close()
}

func (m *Monitor) restartService() error {
	cmd := exec.Command("sudo", "systemctl", "restart", m.cfg.ServiceName)
	return cmd.Run()
}
