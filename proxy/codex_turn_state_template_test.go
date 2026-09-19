package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func fakeTurnState(n int, seed byte) string {
	return strings.Repeat(string(seed), n)
}

// fakeFernetTurnState builds a len==n base64url blob whose Fernet issuedAt is ts.
func fakeFernetTurnState(n int, ts time.Time) string {
	raw := make([]byte, 9)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(ts.Unix()))
	// Pad so base64url length equals n.
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > n {
		tpanic := "fernet fixture longer than requested"
		panic(tpanic)
	}
	return encoded + strings.Repeat("A", n-len(encoded))
}

func enableTurnStateTemplateCache(t *testing.T) {
	t.Helper()
	t.Setenv("CODEX_TURN_STATE_TEMPLATE_CACHE", "true")
	t.Setenv("CODEX_TURN_STATE_TEMPLATE_LENGTH", "292")
	t.Setenv("CODEX_TURN_STATE_REPLACE_LENGTH", "312")
	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "replace-only")
	t.Setenv("CODEX_TURN_STATE_TTL", "1h")
	t.Setenv("CODEX_TURN_STATE_LOG_DECISIONS", "false")
	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "false")
	t.Setenv("CODEX_TURN_STATE_MAX_ENTRIES", "8")
	resetTurnStateTemplateStoreForTest()
	t.Cleanup(func() {
		resetTurnStateTemplateStoreForTest()
		setTurnStateTemplateNowForTest(nil)
	})
}

func TestCaptureCodexTurnStateTemplateRejectsWrongLength(t *testing.T) {
	enableTurnStateTemplateCache(t)
	acc := &auth.Account{DBID: 11}
	model := "gpt-5.4"
	for _, n := range []int{0, 291, 312, 332} {
		h := http.Header{}
		if n > 0 {
			h.Set(codexTurnStateHeader, fakeTurnState(n, 'x'))
		}
		CaptureCodexTurnStateTemplate(acc, model, h)
	}
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatalf("store should reject non-template lengths, got %d entries", globalTurnStateTemplates.lenForTest())
	}
	// multi-value rejected
	h := http.Header{}
	h.Add(codexTurnStateHeader, fakeTurnState(292, 'a'))
	h.Add(codexTurnStateHeader, fakeTurnState(292, 'b'))
	CaptureCodexTurnStateTemplate(acc, model, h)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("multi-value header must not be stored")
	}
}

func TestCaptureCodexTurnStateTemplateAccepts292AndIsolatesKeys(t *testing.T) {
	enableTurnStateTemplateCache(t)
	a1 := &auth.Account{DBID: 1}
	a2 := &auth.Account{DBID: 2}
	m1, m2 := "gpt-5.4", "gpt-5.5"
	v1 := fakeTurnState(292, '1')
	v2 := fakeTurnState(292, '2')
	v3 := fakeTurnState(292, '3')

	h1 := http.Header{}
	h1.Set(codexTurnStateHeader, v1)
	CaptureCodexTurnStateTemplate(a1, m1, h1)

	h2 := http.Header{}
	h2.Set(codexTurnStateHeader, v2)
	CaptureCodexTurnStateTemplate(a1, m2, h2)

	h3 := http.Header{}
	h3.Set(codexTurnStateHeader, v3)
	CaptureCodexTurnStateTemplate(a2, m1, h3)

	cfg := loadTurnStateTemplateConfig()
	got, ok := globalTurnStateTemplates.lookup(cfg, 1, m1)
	if !ok || got != v1 {
		t.Fatalf("acct1/m1 lookup = %q ok=%v", got, ok)
	}
	got, ok = globalTurnStateTemplates.lookup(cfg, 1, m2)
	if !ok || got != v2 {
		t.Fatalf("acct1/m2 lookup = %q ok=%v", got, ok)
	}
	got, ok = globalTurnStateTemplates.lookup(cfg, 2, m1)
	if !ok || got != v3 {
		t.Fatalf("acct2/m1 lookup = %q ok=%v", got, ok)
	}
}

func TestTurnStateTemplateFernetTTLAndCaptureFallback(t *testing.T) {
	enableTurnStateTemplateCache(t)
	acc := &auth.Account{DBID: 7}
	model := "gpt-5.4"
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	// Expired Fernet issuedAt → stored but lookup miss after purge.
	old := fakeFernetTurnState(292, now.Add(-2*time.Hour))
	h := http.Header{}
	h.Set(codexTurnStateHeader, old)
	CaptureCodexTurnStateTemplate(acc, model, h)
	cfg := loadTurnStateTemplateConfig()
	if _, ok := globalTurnStateTemplates.lookup(cfg, 7, model); ok {
		t.Fatal("expired Fernet template should not be usable")
	}

	// Non-Fernet 292 → TTL from capture time (now).
	plain := fakeTurnState(292, 'p')
	h2 := http.Header{}
	h2.Set(codexTurnStateHeader, plain)
	CaptureCodexTurnStateTemplate(acc, model, h2)
	got, ok := globalTurnStateTemplates.lookup(cfg, 7, model)
	if !ok || got != plain {
		t.Fatalf("non-Fernet capture lookup = %q ok=%v", got, ok)
	}

	// Advance past TTL → miss.
	setTurnStateTemplateNowForTest(func() time.Time { return now.Add(time.Hour + time.Second) })
	if _, ok := globalTurnStateTemplates.lookup(cfg, 7, model); ok {
		t.Fatal("capture-time TTL should expire")
	}
}

func TestApplyCodexTurnStateTemplateReplaceOnlyAndAlways(t *testing.T) {
	enableTurnStateTemplateCache(t)
	acc := &auth.Account{DBID: 9}
	model := "gpt-5.4"
	tmpl := fakeTurnState(292, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(acc, model, hCap)

	// replace-only + inbound 312 → substitute
	out := http.Header{}
	out.Set(codexTurnStateHeader, fakeTurnState(312, 'D'))
	ApplyCodexTurnStateTemplate(out, acc, model)
	if got := out.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("substitute: got len=%d want template", len(got))
	}
	if len(out.Values(codexTurnStateHeader)) != 1 {
		t.Fatal("Del+Set must leave a single header value")
	}

	// replace-only + empty → pass
	empty := http.Header{}
	ApplyCodexTurnStateTemplate(empty, acc, model)
	if got := empty.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("replace-only must not inject into empty, got len=%d", len(got))
	}

	// replace-only + already 292 (different) → pass (not replace_length)
	other292 := http.Header{}
	other292.Set(codexTurnStateHeader, fakeTurnState(292, 'O'))
	ApplyCodexTurnStateTemplate(other292, acc, model)
	if got := other292.Get(codexTurnStateHeader); got != fakeTurnState(292, 'O') {
		t.Fatal("replace-only must leave non-312 inbound alone")
	}

	// always + empty → inject
	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "always")
	alwaysEmpty := http.Header{}
	ApplyCodexTurnStateTemplate(alwaysEmpty, acc, model)
	if got := alwaysEmpty.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("always inject: got len=%d", len(got))
	}

	// always + 312 → inject
	alwaysDeg := http.Header{}
	alwaysDeg.Set(codexTurnStateHeader, fakeTurnState(312, 'Z'))
	ApplyCodexTurnStateTemplate(alwaysDeg, acc, model)
	if got := alwaysDeg.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("always on 312: got len=%d", len(got))
	}
}

func TestApplyCodexTurnStateTemplateDryRunAndDisabled(t *testing.T) {
	enableTurnStateTemplateCache(t)
	acc := &auth.Account{DBID: 3}
	model := "gpt-5.4"
	tmpl := fakeTurnState(292, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(acc, model, hCap)

	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "true")
	out := http.Header{}
	degraded := fakeTurnState(312, 'D')
	out.Set(codexTurnStateHeader, degraded)
	ApplyCodexTurnStateTemplate(out, acc, model)
	if got := out.Get(codexTurnStateHeader); got != degraded {
		t.Fatal("dry-run must not mutate headers")
	}

	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "false")
	t.Setenv("CODEX_TURN_STATE_TEMPLATE_CACHE", "false")
	out2 := http.Header{}
	out2.Set(codexTurnStateHeader, degraded)
	ApplyCodexTurnStateTemplate(out2, acc, model)
	if got := out2.Get(codexTurnStateHeader); got != degraded {
		t.Fatal("feature-off must not mutate headers")
	}
}

func TestGuardThenApplyReplaceOnlyLeavesStrippedEmpty(t *testing.T) {
	enableTurnStateTemplateCache(t)
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	model := "gpt-5.4"
	affinityKey := "turn-tmpl-compose::api-key:1"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	tmpl := fakeTurnState(292, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(other, model, hCap)

	// Provenance: minter issued the echo the client still carries (degraded).
	noteCodexTurnStateProvenance(affinityKey, minter)
	echo := http.Header{}
	echo.Set(codexTurnStateHeader, fakeTurnState(312, 'D'))
	guardCodexTurnStateEcho(affinityKey, other, echo)
	if got := echo.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("guard should strip foreign echo, got len=%d", len(got))
	}
	// replace-only: stripped empty must stay empty
	ApplyCodexTurnStateTemplate(echo, other, model)
	if got := echo.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("replace-only after strip must not reinject, got len=%d", len(got))
	}

	// always: may inject current account template
	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "always")
	ApplyCodexTurnStateTemplate(echo, other, model)
	if got := echo.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("always after strip should inject current tmpl, got len=%d", len(got))
	}
}

func TestTurnStateTemplateMaxEntriesEvictsOldest(t *testing.T) {
	enableTurnStateTemplateCache(t)
	t.Setenv("CODEX_TURN_STATE_MAX_ENTRIES", "2")
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	cfg := loadTurnStateTemplateConfig()
	for i, seed := range []byte{'a', 'b', 'c'} {
		acc := &auth.Account{DBID: int64(i + 1)}
		h := http.Header{}
		h.Set(codexTurnStateHeader, fakeTurnState(292, seed))
		CaptureCodexTurnStateTemplate(acc, "gpt-5.4", h)
		now = now.Add(time.Minute)
		setTurnStateTemplateNowForTest(func() time.Time { return now })
	}
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("expected 2 entries after eviction, got %d", globalTurnStateTemplates.lenForTest())
	}
	if _, ok := globalTurnStateTemplates.lookup(cfg, 1, "gpt-5.4"); ok {
		t.Fatal("oldest account=1 should have been evicted")
	}
	if _, ok := globalTurnStateTemplates.lookup(cfg, 2, "gpt-5.4"); !ok {
		t.Fatal("account=2 should remain")
	}
	if _, ok := globalTurnStateTemplates.lookup(cfg, 3, "gpt-5.4"); !ok {
		t.Fatal("account=3 should remain")
	}
}

func TestApplyCodexTurnStateTemplateSkipsRelayAccounts(t *testing.T) {
	enableTurnStateTemplateCache(t)
	// OpenAI Responses relay account should be ignored.
	relay := &auth.Account{DBID: 55, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "sk-test"}
	model := "gpt-5.4"
	tmpl := fakeTurnState(292, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(relay, model, hCap)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("relay accounts must not harvest templates")
	}
	out := http.Header{}
	out.Set(codexTurnStateHeader, fakeTurnState(312, 'D'))
	ApplyCodexTurnStateTemplate(out, relay, model)
	if got := out.Get(codexTurnStateHeader); len(got) != 312 {
		t.Fatal("relay accounts must not apply templates")
	}
}
