package proxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// X-Codex-Turn-State length-292 template cache (v1).
//
// Process-memory map keyed by (account DBID, exact upstream model) stores reusable
// Fernet templates observed on upstream responses. Only len==template_length
// (default 292) is stored; never forge; never harvest client request headers;
// never cross account or model. Compose AFTER guardCodexTurnStateEcho.
//
// Master switch defaults OFF (CODEX_TURN_STATE_TEMPLATE_CACHE).

const (
	defaultTurnStateTemplateLength = 292
	defaultTurnStateReplaceLength  = 312
	defaultTurnStateTemplateTTL    = time.Hour
	defaultTurnStateTemplateMax    = 256

	turnStateInjectReplaceOnly = "replace-only"
	turnStateInjectAlways      = "always"
)

type turnStateTemplateConfig struct {
	Enabled        bool
	TemplateLength int
	ReplaceLength  int
	InjectMode     string
	TTL            time.Duration
	LogDecisions   bool
	MaxEntries     int
	DryRun         bool
}

func loadTurnStateTemplateConfig() turnStateTemplateConfig {
	// Master switch is system settings (Codex experimental UI); env is no longer the primary toggle.
	cfg := turnStateTemplateConfig{
		Enabled:        CurrentRuntimeSettings().CodexTurnStateTemplateCache,
		TemplateLength: defaultTurnStateTemplateLength,
		ReplaceLength:  defaultTurnStateReplaceLength,
		InjectMode:     turnStateInjectReplaceOnly,
		TTL:            defaultTurnStateTemplateTTL,
		LogDecisions:   parseTurnStateBoolEnv(os.Getenv("CODEX_TURN_STATE_LOG_DECISIONS")),
		MaxEntries:     defaultTurnStateTemplateMax,
		DryRun:         parseTurnStateBoolEnv(os.Getenv("CODEX_TURN_STATE_DRY_RUN")),
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_TEMPLATE_LENGTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TemplateLength = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_REPLACE_LENGTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ReplaceLength = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.TTL = d
		} else if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			cfg.TTL = time.Duration(sec) * time.Second
		}
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_MAX_ENTRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxEntries = n
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_INJECT_MODE"))) {
	case turnStateInjectAlways:
		cfg.InjectMode = turnStateInjectAlways
	default:
		cfg.InjectMode = turnStateInjectReplaceOnly
	}
	// Reject equal/invalid lengths by disabling (arden rule).
	if cfg.TemplateLength <= 0 || cfg.ReplaceLength <= 0 || cfg.TemplateLength == cfg.ReplaceLength {
		cfg.Enabled = false
	}
	return cfg
}

func (c turnStateTemplateConfig) injectAlways() bool {
	return c.InjectMode == turnStateInjectAlways
}

func parseTurnStateBoolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

type turnStateTemplateKey struct {
	AccountID int64
	Model     string
}

type turnStateTemplateEntry struct {
	Value    string
	IssuedAt time.Time
}

type turnStateTemplateStore struct {
	mu      sync.Mutex
	entries map[turnStateTemplateKey]turnStateTemplateEntry
	now     func() time.Time
}

func newTurnStateTemplateStore() *turnStateTemplateStore {
	return &turnStateTemplateStore{
		entries: make(map[turnStateTemplateKey]turnStateTemplateEntry),
		now:     time.Now,
	}
}

var globalTurnStateTemplates = newTurnStateTemplateStore()

func resetTurnStateTemplateStoreForTest() {
	globalTurnStateTemplates.mu.Lock()
	defer globalTurnStateTemplates.mu.Unlock()
	globalTurnStateTemplates.entries = make(map[turnStateTemplateKey]turnStateTemplateEntry)
	globalTurnStateTemplates.now = time.Now
}

func setTurnStateTemplateNowForTest(now func() time.Time) {
	globalTurnStateTemplates.mu.Lock()
	defer globalTurnStateTemplates.mu.Unlock()
	if now == nil {
		globalTurnStateTemplates.now = time.Now
		return
	}
	globalTurnStateTemplates.now = now
}

func turnStateTemplateUsable(issuedAt, now time.Time, ttl time.Duration) bool {
	if issuedAt.After(now) {
		return false
	}
	return now.Before(issuedAt.Add(ttl))
}

// fernetIssuedAt extracts the issuance time embedded in a Codex
// X-Codex-Turn-State Fernet token: 0x80 || be64(unix secs) || …, base64url.
func fernetIssuedAt(value string) (time.Time, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil || len(raw) < 9 || raw[0] != 0x80 {
		return time.Time{}, false
	}
	secs := binary.BigEndian.Uint64(raw[1:9])
	return time.Unix(int64(secs), 0), true
}

func (s *turnStateTemplateStore) capture(cfg turnStateTemplateConfig, accountID int64, model string, values ...string) bool {
	if !cfg.Enabled || accountID <= 0 {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" || len(values) != 1 {
		return false
	}
	value := values[0]
	if value == "" || len(value) != cfg.TemplateLength {
		return false
	}
	issued, ok := fernetIssuedAt(value)
	now := s.now()
	if !ok {
		issued = now
	}
	// Reject expired/future-issued templates before purge/eviction/store so a
	// bad capture cannot shrink a full cache.
	if !turnStateTemplateUsable(issued, now, cfg.TTL) {
		return false
	}
	key := turnStateTemplateKey{AccountID: accountID, Model: model}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(cfg.TTL, now)
	if _, exists := s.entries[key]; !exists {
		s.evictOldestLocked(cfg.MaxEntries)
	}
	s.entries[key] = turnStateTemplateEntry{Value: value, IssuedAt: issued}
	return true
}

func (s *turnStateTemplateStore) lookup(cfg turnStateTemplateConfig, accountID int64, model string) (string, bool) {
	if !cfg.Enabled || accountID <= 0 {
		return "", false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	key := turnStateTemplateKey{AccountID: accountID, Model: model}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(cfg.TTL, now)
	entry, ok := s.entries[key]
	if !ok || !turnStateTemplateUsable(entry.IssuedAt, now, cfg.TTL) {
		if ok {
			delete(s.entries, key)
		}
		return "", false
	}
	return entry.Value, true
}

func (s *turnStateTemplateStore) clearAccount(accountID int64) {
	if accountID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if key.AccountID == accountID {
			delete(s.entries, key)
		}
	}
}

func (s *turnStateTemplateStore) purgeExpiredLocked(ttl time.Duration, now time.Time) {
	for key, entry := range s.entries {
		if !turnStateTemplateUsable(entry.IssuedAt, now, ttl) {
			delete(s.entries, key)
		}
	}
}

func (s *turnStateTemplateStore) evictOldestLocked(maxEntries int) {
	if maxEntries < 1 {
		maxEntries = 1
	}
	for len(s.entries) >= maxEntries {
		var oldestKey turnStateTemplateKey
		var oldest turnStateTemplateEntry
		first := true
		for key, entry := range s.entries {
			if first || entry.IssuedAt.Before(oldest.IssuedAt) ||
				(entry.IssuedAt.Equal(oldest.IssuedAt) && (key.AccountID < oldestKey.AccountID ||
					(key.AccountID == oldestKey.AccountID && key.Model < oldestKey.Model))) {
				oldestKey = key
				oldest = entry
				first = false
			}
		}
		delete(s.entries, oldestKey)
	}
}

func (s *turnStateTemplateStore) lenForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func accountEligibleForTurnStateTemplate(account *auth.Account) bool {
	if account == nil || account.ID() <= 0 {
		return false
	}
	// Codex ChatGPT OAuth / Agent Identity only — skip OpenAI Responses / other relays.
	if account.IsRelayStyle() || account.IsOpenAIResponsesAPI() {
		return false
	}
	return true
}

// CaptureCodexTurnStateTemplate stores an upstream-minted template when enabled
// and the sole header value length matches template_length. Never stores
// replace_length / other lengths. Never harvest client request headers.
func CaptureCodexTurnStateTemplate(account *auth.Account, model string, headers http.Header) {
	cfg := loadTurnStateTemplateConfig()
	if !cfg.Enabled || !accountEligibleForTurnStateTemplate(account) || headers == nil {
		return
	}
	values := headers.Values(codexTurnStateHeader)
	trimmed := make([]string, 0, len(values))
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	if len(trimmed) == 0 {
		return
	}
	stored := globalTurnStateTemplates.capture(cfg, account.ID(), model, trimmed...)
	if stored {
		logTurnStateTemplateDecision(cfg, "harvest", account.ID(), model, len(trimmed[0]), "template stored")
		return
	}
	if cfg.LogDecisions && len(trimmed) == 1 && len(trimmed[0]) == cfg.ReplaceLength {
		logTurnStateTemplateDecision(cfg, "skip", account.ID(), model, len(trimmed[0]),
			"upstream issued degraded state (len=replace_length)")
	}
}

// ClearCodexTurnStateTemplatesForAccount drops cached templates for a DBID.
func ClearCodexTurnStateTemplatesForAccount(accountID int64) {
	globalTurnStateTemplates.clearAccount(accountID)
}

func decideCodexTurnStateHeader(cfg turnStateTemplateConfig, inbound, tmpl string, haveTmpl bool) (decision, reason, replacement string) {
	if haveTmpl && inbound != tmpl {
		if cfg.injectAlways() {
			return "inject", injectTurnStateReason(inbound, cfg), tmpl
		}
		if inbound != "" && len(inbound) == cfg.ReplaceLength {
			return "substitute", "inbound len=replace_length", tmpl
		}
	}
	switch {
	case haveTmpl && inbound == tmpl:
		return "pass", "header already current", ""
	case len(inbound) == cfg.ReplaceLength && !haveTmpl:
		return "pass", "no live template for bucket", ""
	default:
		return "pass", "nothing to do", ""
	}
}

func injectTurnStateReason(value string, cfg turnStateTemplateConfig) string {
	switch len(value) {
	case 0:
		return "added (request carried no state)"
	case cfg.ReplaceLength:
		return "replaced degraded state"
	default:
		return "replaced non-template state (len " + strconv.Itoa(len(value)) + ")"
	}
}

// ApplyCodexTurnStateTemplate rewrites outbound X-Codex-Turn-State from the
// selected account's cached template. Call AFTER guardCodexTurnStateEcho.
// Clear then Set to avoid duplicate casings. Never logs the state value.
// ctx carries usage-log audit (turn-state decision/lengths); nil ctx skips audit.
func ApplyCodexTurnStateTemplate(ctx context.Context, headers http.Header, account *auth.Account, model string) {
	cfg := loadTurnStateTemplateConfig()
	if !cfg.Enabled || headers == nil || !accountEligibleForTurnStateTemplate(account) {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	inbound := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	tmpl, ok := globalTurnStateTemplates.lookup(cfg, account.ID(), model)
	decision, reason, replacement := decideCodexTurnStateHeader(cfg, inbound, tmpl, ok)
	outboundLen := len(inbound)
	rewritten := false
	if replacement != "" && !cfg.DryRun {
		headers.Del(codexTurnStateHeader)
		headers.Set(codexTurnStateHeader, replacement)
		outboundLen = len(replacement)
		rewritten = decision == "substitute" || decision == "inject"
	} else if replacement != "" && cfg.DryRun {
		outboundLen = len(replacement)
	}
	recordTurnStateTemplateAudit(ctx, decision, len(inbound), outboundLen, rewritten)
	logTurnStateTemplateDecision(cfg, decision, account.ID(), model, len(inbound), reason)
}

func logTurnStateTemplateDecision(cfg turnStateTemplateConfig, decision string, accountID int64, model string, length int, reason string) {
	if !cfg.LogDecisions {
		return
	}
	// NEVER log the state value — decision + account id + model + lengths only.
	log.Printf("[codex-turn-state] %s account=%d model=%q len=%d (%s)", decision, accountID, model, length, reason)
}

// ==================== usage-log turn-state audit ====================

type turnStateTemplateAuditContextKey struct{}

type turnStateTemplateAudit struct {
	mu          sync.Mutex
	decision    string
	inboundLen  int
	outboundLen int
	rewritten   bool
	recorded    bool
}

func withTurnStateTemplateAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if turnStateTemplateAuditFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, turnStateTemplateAuditContextKey{}, &turnStateTemplateAudit{})
}

func turnStateTemplateAuditFromContext(ctx context.Context) *turnStateTemplateAudit {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(turnStateTemplateAuditContextKey{}).(*turnStateTemplateAudit)
	return audit
}

func attachTurnStateTemplateAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withTurnStateTemplateAudit(c.Request.Context()))
}

func recordTurnStateTemplateAudit(ctx context.Context, decision string, inboundLen, outboundLen int, rewritten bool) {
	audit := turnStateTemplateAuditFromContext(ctx)
	if audit == nil {
		return
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	audit.decision = decision
	audit.inboundLen = inboundLen
	audit.outboundLen = outboundLen
	audit.rewritten = rewritten
	audit.recorded = true
}

func populateTurnStateTemplateMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	audit := turnStateTemplateAuditFromContext(c.Request.Context())
	if audit == nil {
		return
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if !audit.recorded {
		return
	}
	// Only annotate when a rewrite was decided (substitute/inject). Pass stays blank
	// so the Usage table mirrors UA: silence unless something changed.
	if audit.decision != "substitute" && audit.decision != "inject" {
		return
	}
	input.TurnStateOverridden = audit.rewritten
	note := ""
	switch audit.decision {
	case "substitute":
		note = strconv.Itoa(audit.inboundLen) + "→" + strconv.Itoa(audit.outboundLen)
	case "inject":
		if audit.inboundLen == 0 {
			note = "inject"
		} else {
			note = "inject " + strconv.Itoa(audit.inboundLen) + "→" + strconv.Itoa(audit.outboundLen)
		}
	}
	input.TurnStateRewriteNote = note
}

