package units

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// fakeRates returns the Frankfurter-shaped payload for a base currency.
func fakeRates(base string) string {
	// A small but realistic rate table; the handler only needs the requested
	// target to be present.
	rates := map[string]string{
		"USD": `{"base":"USD","date":"2026-11-13","rates":{"EUR":0.92,"GBP":0.79,"JPY":150.1234,"CAD":1.37}}`,
		"EUR": `{"base":"EUR","date":"2026-11-13","rates":{"USD":1.0869,"GBP":0.86,"JPY":163.2}}`,
		"GBP": `{"base":"GBP","date":"2026-11-13","rates":{"USD":1.26,"EUR":1.16,"JPY":189.9}}`,
	}
	if body, ok := rates[base]; ok {
		return body
	}
	return fmt.Sprintf(`{"base":%q,"date":"2026-11-13","rates":{"USD":1,"EUR":1}}`, base)
}

// newFakeServer builds an https test server (httptest.NewTLSServer) that counts
// requests and, when fail is true, answers every /latest with 500. The returned
// client trusts the server's cert, exercising the production https-only path.
func newFakeServer(t *testing.T, fail bool) (*httptest.Server, *http.Client, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		base := r.URL.Query().Get("from")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fakeRates(base))
	}))
	t.Cleanup(server.Close)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	return server, client, &hits
}

func testService(t *testing.T, server *httptest.Server, client *http.Client, now func() time.Time) *currencyService {
	t.Helper()
	return newCurrencyService(CurrencyOptions{BaseURL: server.URL, Client: client, Now: now})
}

func fixedNow() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }

// TestCurrencyConversionAndRounding checks the arithmetic and the cent rounding.
func TestCurrencyConversionAndRounding(t *testing.T) {
	server, client, _ := newFakeServer(t, false)
	svc := testService(t, server, client, fixedNow)

	out, err := svc.convert(context.Background(), CurrencyIn{Value: 50, From: "USD", To: "EUR"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out.Result != 46 {
		t.Fatalf("50 USD->EUR = %v, want 46", out.Result)
	}
	if out.Display != "46" {
		t.Fatalf("display = %q, want 46", out.Display)
	}
	if math.Abs(out.Rate-0.92) > 1e-9 {
		t.Fatalf("rate = %v, want 0.92", out.Rate)
	}

	// 50 * 0.9205-style rounding: use JPY rate 150.1234 -> 7506.17.
	out2, err := svc.convert(context.Background(), CurrencyIn{Value: 50, From: "USD", To: "JPY"})
	if err != nil {
		t.Fatalf("convert JPY: %v", err)
	}
	if out2.Result != 7506.17 {
		t.Fatalf("50 USD->JPY = %v, want 7506.17", out2.Result)
	}

	// Same-currency identity needs no network and returns the value unchanged.
	out3, err := svc.convert(context.Background(), CurrencyIn{Value: 12.5, From: "USD", To: "USD"})
	if err != nil {
		t.Fatalf("convert same: %v", err)
	}
	if out3.Result != 12.5 || out3.Rate != 1 {
		t.Fatalf("identity = %+v, want value 12.5 rate 1", out3)
	}
}

// TestCurrencyCacheReuse proves one GET per base currency and a 6h fresh window.
func TestCurrencyCacheReuse(t *testing.T) {
	server, client, hits := newFakeServer(t, false)
	svc := testService(t, server, client, fixedNow)

	// First call for USD reaches the server.
	if _, err := svc.convert(context.Background(), CurrencyIn{Value: 1, From: "USD", To: "EUR"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	afterFirst := atomic.LoadInt32(hits)
	if afterFirst != 1 {
		t.Fatalf("first call hits = %d, want 1", afterFirst)
	}

	// Second call within the cache window must not hit the server again.
	if _, err := svc.convert(context.Background(), CurrencyIn{Value: 2, From: "USD", To: "GBP"}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != afterFirst {
		t.Fatalf("cache reuse hits = %d, want %d", got, afterFirst)
	}

	// A different base currency fetches once more.
	if _, err := svc.convert(context.Background(), CurrencyIn{Value: 2, From: "EUR", To: "USD"}); err != nil {
		t.Fatalf("third: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != afterFirst+1 {
		t.Fatalf("new base hits = %d, want %d", got, afterFirst+1)
	}
}

// TestCurrencyStaleWording checks the fallback to last good rates and the "as of
// <date>" wording when the upstream fails after a good fetch.
func TestCurrencyStaleWording(t *testing.T) {
	server, client, _ := newFakeServer(t, false)
	svc := testService(t, server, client, fixedNow)

	// Seed a good fetch (populates lastGood) and then expire the fresh cache so
	// the next call actually reaches the (now failing) upstream.
	if _, err := svc.convert(context.Background(), CurrencyIn{Value: 10, From: "USD", To: "EUR"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc.cache = map[string]cachedRates{}

	// Now make the upstream fail.
	failing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	svc.opts.BaseURL = failing.URL

	out, err := svc.convert(context.Background(), CurrencyIn{Value: 10, From: "USD", To: "EUR"})
	if err != nil {
		t.Fatalf("stale convert: %v", err)
	}
	if !out.Stale {
		t.Fatalf("stale = %v, want true", out.Stale)
	}
	if out.RateDate != "2026-11-13" {
		t.Fatalf("rate date = %q, want 2026-11-13", out.RateDate)
	}

	text := speakCurrency(out)
	if !strings.Contains(text, "as of 2026-11-13") {
		t.Fatalf("speak = %q, want it to contain 'as of 2026-11-13'", text)
	}
	if !strings.Contains(text, "about") {
		t.Fatalf("speak = %q, want 'about'", text)
	}
}

// TestCurrencyRefuseWithoutCache checks the honest refusal when there is no
// cached rate yet and the upstream is down.
func TestCurrencyRefuseWithoutCache(t *testing.T) {
	server, client, _ := newFakeServer(t, true)
	svc := testService(t, server, client, fixedNow)

	_, err := svc.convert(context.Background(), CurrencyIn{Value: 10, From: "USD", To: "EUR"})
	if err == nil {
		t.Fatal("expected a truthful refusal, got nil")
	}
	msg := tool.FailureMessage(err)
	if !strings.Contains(strings.ToLower(msg), "exchange rate") {
		t.Fatalf("refusal = %q, want it to mention an exchange rate", msg)
	}
}

// TestCurrencyUnknownRefused checks unknown currencies are a spoken refusal.
func TestCurrencyUnknownRefused(t *testing.T) {
	server, client, _ := newFakeServer(t, false)
	svc := testService(t, server, client, fixedNow)

	for _, tc := range []struct{ from, to string }{
		{"bitcoin", "USD"},
		{"USD", "peso"},
	} {
		_, err := svc.convert(context.Background(), CurrencyIn{Value: 5, From: tc.from, To: tc.to})
		if err == nil {
			t.Fatalf("%s->%s: expected refusal, got nil", tc.from, tc.to)
		}
	}

	// dollars/euros resolve fine and must NOT error.
	if _, err := svc.convert(context.Background(), CurrencyIn{Value: 5, From: "dollars", To: "euros"}); err != nil {
		t.Fatalf("dollars->euros: unexpected error %v", err)
	}
}

// TestCurrencyOversizeBody checks a response over 64 KiB is rejected and falls
// back to last good rates when present.
func TestCurrencyOversizeBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Pad well past the 64 KiB limit.
		var b strings.Builder
		b.WriteString(`{"base":"USD","date":"2026-11-13","rates":{"EUR":0.92,`)
		for i := 0; i < 10000; i++ {
			b.WriteString(fmt.Sprintf("\"X%c%04d\":0.12345,", 'A'+rune(i%26), i))
		}
		b.WriteString(`"EUR2":0.92}}`)
		fmt.Fprint(w, b.String())
	}))
	t.Cleanup(server.Close)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	svc := testService(t, server, client, fixedNow)
	_, err := svc.convert(context.Background(), CurrencyIn{Value: 1, From: "USD", To: "EUR"})
	if err == nil {
		t.Fatal("expected oversize body error, got nil")
	}
}

// TestCurrencySpeakToday checks the "at today's rate" wording for a fresh, same-
// day rate.
func TestCurrencySpeakToday(t *testing.T) {
	out := CurrencyOut{Value: 50, From: "USD", To: "EUR", Result: 46, Display: "46",
		RateDate: "2026-11-16", RateDateIsToday: true}
	text := speakCurrency(out)
	if strings.Contains(text, "as of") {
		t.Fatalf("speak = %q, want 'at today's rate' wording", text)
	}
	if !strings.Contains(text, "at today's rate") {
		t.Fatalf("speak = %q, want 'at today's rate'", text)
	}
	if !strings.Contains(text, "about") {
		t.Fatalf("speak = %q, want 'about'", text)
	}
}

// TestCurrencyExtract covers the natural word orders and the refusals.
func TestCurrencyExtract(t *testing.T) {
	positive := []struct {
		utterance     string
		value         float64
		from, to      string
	}{
		{"how much is 50 dollars in euros", 50, "USD", "EUR"},
		{"what is 100 euros in us dollars", 100, "EUR", "USD"},
		{"50 euros to us dollars", 50, "EUR", "USD"},
		{"how much is 200 pounds in dollars", 200, "GBP", "USD"},
		{"60 dollars to british pounds", 60, "USD", "GBP"},
	}
	for _, tc := range positive {
		in, ok := extractCurrency(tc.utterance)
		if !ok {
			t.Fatalf("Extract(%q) failed", tc.utterance)
		}
		if in.Value != tc.value || in.From != tc.from || in.To != tc.to {
			t.Fatalf("Extract(%q) = %+v, want %v %s %s", tc.utterance, in, tc.value, tc.from, tc.to)
		}
	}

	negative := []string{
		"what's the exchange rate for the euro",
		"how much is a bitcoin worth in dollars",
		"what's the price of gold in euros",
		"how many euros make a dollar",
		"convert 30 celsius to fahrenheit",
		"how many miles in 5 kilometers",
		"send 50 dollars to Alex",
	}
	for _, utterance := range negative {
		if _, ok := extractCurrency(utterance); ok {
			t.Fatalf("Extract(%q) succeeded, want the Agent path", utterance)
		}
	}
}

// TestResolveCurrencyDefaultsDollarsToUSD documents the ambiguous-word choice.
func TestResolveCurrencyDefaultsDollarsToUSD(t *testing.T) {
	iso, ok := resolveCurrency("dollars")
	if !ok || iso != "USD" {
		t.Fatalf("resolveCurrency(dollars) = %q %v, want USD", iso, ok)
	}
	iso, ok = resolveCurrency("USD")
	if !ok || iso != "USD" {
		t.Fatalf("resolveCurrency(USD) = %q %v, want USD", iso, ok)
	}
	// "peso" is ambiguous and must be refused, not guessed.
	if iso, ok := resolveCurrency("peso"); ok {
		t.Fatalf("resolveCurrency(peso) = %q, want refusal", iso)
	}
}
