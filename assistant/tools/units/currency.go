package units

// Currency conversion with daily ECB reference rates, fetched from the keyless
// Frankfurter API. units.convert stays the network-free table of length, mass,
// volume, temperature, time, speed and area units; this Tool is the currency
// counterpart and is the only part of the module that touches the network.
//
// One GET /latest?from=<base> is made per base currency and cached for 6 hours.
// A failed fetch falls back to the last good rates with an "as of <date>"
// phrase; when there is no cached rate yet the Tool refuses truthfully instead
// of inventing one.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// CurrencyName is the canonical units.currency Tool name.
const CurrencyName = "units.currency"

const (
	// frankfurterBaseURL is the fixed production upstream. It publishes ECB
	// reference rates and needs no key; the https scheme is enforced on every
	// request, so a stale or mis-set base URL fails closed.
	frankfurterBaseURL = "https://api.frankfurter.app"
	// currencyTimeout bounds one upstream request, including the body.
	currencyTimeout = 2 * time.Second
	// currencyCacheTTL is how long a fetched rate table stays fresh.
	currencyCacheTTL = 6 * time.Hour
	// currencyMaxBody is the response size ceiling (64 KiB).
	currencyMaxBody = 64 * 1024
)

// CurrencyOptions configures the units.currency Tool's upstream. Tests inject a
// fake server URL, client and clock; production uses the zero value, which
// resolves to the fixed https endpoint and a real keep-alive client.
type CurrencyOptions struct {
	BaseURL string
	Client  *http.Client
	Now     func() time.Time
}

func (o CurrencyOptions) withDefaults() CurrencyOptions {
	if o.BaseURL == "" {
		o.BaseURL = frankfurterBaseURL
	}
	if o.Client == nil {
		o.Client = httpclient.New(httpclient.Options{
			Timeout: currencyTimeout,
			NoProxy: true,
		})
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// CurrencyIn is one conversion request. From and To are ISO codes or spoken
// aliases resolved by the Tool.
type CurrencyIn struct {
	Value float64 `json:"value"`
	From  string  `json:"from"`
	To    string  `json:"to"`
}

// CurrencyOut is the converted result. RateDate is the ECB reference date of the
// rates used; Stale is true when the live fetch failed and the last good rates
// were used; RateDateIsToday records whether RateDate is the current day, so
// Speak can say "at today's rate" instead of repeating the date.
type CurrencyOut struct {
	Value           float64 `json:"value"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	Rate            float64 `json:"rate"`
	Result          float64 `json:"result"`
	Display         string  `json:"display"`
	RateDate        string  `json:"rate_date"`
	Stale           bool    `json:"stale"`
	RateDateIsToday bool    `json:"rate_date_is_today"`
}

// NewCurrency builds the units.currency Tool. opts is the production default
// when zero; tests pass a fake server URL and client.
func NewCurrency(opts CurrencyOptions) tool.Tool[CurrencyIn, CurrencyOut] {
	service := newCurrencyService(opts)
	return tool.Tool[CurrencyIn, CurrencyOut]{
		Name:        CurrencyName,
		Description: "Convert an amount between currencies using the daily ECB reference rates.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     3 * time.Second,
		Handler: func(ctx context.Context, in CurrencyIn) (CurrencyOut, error) {
			return service.convert(ctx, in)
		},
		Speak:    speakCurrency,
		Extract:  extractCurrency,
		Examples: currencyExamples,
	}
}

// currencyService holds the cached rate tables and the injectable upstream.
type currencyService struct {
	opts CurrencyOptions
	mu   sync.Mutex
	// cache holds fresh rate tables per base currency.
	cache map[string]cachedRates
	// lastGood keeps the most recent successful fetch per base currency so a
	// later failure can fall back to it.
	lastGood map[string]cachedRates
}

type cachedRates struct {
	date      string
	rates     map[string]float64
	expiresAt time.Time
}

func newCurrencyService(opts CurrencyOptions) *currencyService {
	opts = opts.withDefaults()
	return &currencyService{
		opts:     opts,
		cache:    make(map[string]cachedRates),
		lastGood: make(map[string]cachedRates),
	}
}

// convert resolves both currencies, fetches (or replays) the rate table for the
// base currency, and applies the rate. It refuses unknown currencies and a
// missing rate truthfully.
func (s *currencyService) convert(ctx context.Context, in CurrencyIn) (CurrencyOut, error) {
	fromISO, ok := resolveCurrency(in.From)
	if !ok {
		return CurrencyOut{}, &tool.ToolError{Say: unknownCurrency(in.From)}
	}
	toISO, ok := resolveCurrency(in.To)
	if !ok {
		return CurrencyOut{}, &tool.ToolError{Say: unknownCurrency(in.To)}
	}
	// Same currency is a trivial, network-free identity conversion.
	if fromISO == toISO {
		return CurrencyOut{
			Value:   in.Value,
			From:    fromISO,
			To:      toISO,
			Rate:    1,
			Result:  in.Value,
			Display: formatMoney(in.Value),
		}, nil
	}

	rates, stale, err := s.rates(ctx, fromISO)
	if err != nil {
		return CurrencyOut{}, &tool.ToolError{Say: fmt.Sprintf(
			"I don't have a recent exchange rate for %s yet.", currencyNoun(fromISO, 1))}
	}
	rate, ok := rates.rates[toISO]
	if !ok {
		return CurrencyOut{}, &tool.ToolError{Say: fmt.Sprintf(
			"I don't have a rate from %s to %s.", currencyNoun(fromISO, 2), currencyNoun(toISO, 2))}
	}
	result := math.Round(in.Value*rate*100) / 100
	today := s.opts.Now().UTC().Format("2006-01-02")
	return CurrencyOut{
		Value:           in.Value,
		From:            fromISO,
		To:              toISO,
		Rate:            rate,
		Result:          result,
		Display:         formatMoney(result),
		RateDate:        rates.date,
		Stale:           stale,
		RateDateIsToday: rates.date == today,
	}, nil
}

// rates returns the fresh cache entry when it has not expired; otherwise it
// fetches a new one. On a fetch failure it returns the last good entry (stale),
// and on no cached entry at all it returns the fetch error so the caller can
// refuse truthfully.
func (s *currencyService) rates(ctx context.Context, base string) (cachedRates, bool, error) {
	s.mu.Lock()
	if c, ok := s.cache[base]; ok && s.opts.Now().Before(c.expiresAt) {
		s.mu.Unlock()
		return c, false, nil
	}
	s.mu.Unlock()

	fetched, err := s.fetchRates(ctx, base)
	if err != nil {
		s.mu.Lock()
		if lg, ok := s.lastGood[base]; ok {
			s.mu.Unlock()
			return lg, true, nil
		}
		s.mu.Unlock()
		return cachedRates{}, false, err
	}

	s.mu.Lock()
	s.cache[base] = fetched
	s.lastGood[base] = fetched
	s.mu.Unlock()
	return fetched, false, nil
}

// fetchRates performs one GET /latest?from=<base> against the configured
// upstream, enforcing https, a 2 s timeout, a 64 KiB body cap and JSON shape.
func (s *currencyService) fetchRates(ctx context.Context, base string) (cachedRates, error) {
	endpoint := strings.TrimRight(s.opts.BaseURL, "/") + "/latest?from=" + url.QueryEscape(base)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return cachedRates{}, fmt.Errorf("currency: parse %s: %w", endpoint, err)
	}
	if parsed.Scheme != "https" {
		return cachedRates{}, fmt.Errorf("currency: refusing non-https upstream %q", endpoint)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return cachedRates{}, fmt.Errorf("currency: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.opts.Client.Do(request)
	if err != nil {
		return cachedRates{}, fmt.Errorf("currency: fetch %s: %w", base, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return cachedRates{}, fmt.Errorf("currency: fetch %s: upstream status %s", base, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, currencyMaxBody+1))
	if err != nil {
		return cachedRates{}, fmt.Errorf("currency: read %s: %w", base, err)
	}
	if len(body) > currencyMaxBody {
		return cachedRates{}, fmt.Errorf("currency: fetch %s: response over %d bytes", base, currencyMaxBody)
	}
	var payload struct {
		Base  string             `json:"base"`
		Date  string             `json:"date"`
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return cachedRates{}, fmt.Errorf("currency: decode %s: %w", base, err)
	}
	if payload.Date == "" || len(payload.Rates) == 0 {
		return cachedRates{}, fmt.Errorf("currency: fetch %s: missing date or rates", base)
	}
	return cachedRates{
		date:      payload.Date,
		rates:     payload.Rates,
		expiresAt: s.opts.Now().Add(currencyCacheTTL),
	}, nil
}

// speakCurrency renders the one-line Fast path reply. It always says "about",
// and includes the rate date only when it is not today or the rates are a stale
// fallback.
func speakCurrency(out CurrencyOut) string {
	fromWord := currencyNoun(out.From, out.Value)
	toWord := currencyNoun(out.To, out.Result)
	clause := ""
	switch {
	case out.RateDate == "":
		clause = ""
	case out.Stale || !out.RateDateIsToday:
		clause = " as of " + out.RateDate
	default:
		clause = " at today's rate"
	}
	return formatMoney(out.Value) + " " + fromWord + " is about " + out.Display + " " + toWord + clause + "."
}

// unknownCurrency is the spoken refusal for a currency the table does not know.
func unknownCurrency(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Tell me an amount and two currencies to convert."
	}
	return fmt.Sprintf("I don't know the currency %q.", name)
}

// formatMoney rounds to cents and trims trailing zeros so speech stays short.
func formatMoney(value float64) string {
	if value == 0 {
		return "0"
	}
	rounded := math.Round(value*100) / 100
	text := strconv.FormatFloat(rounded, 'f', 2, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "-0" {
		return "0"
	}
	return text
}

// currencyExamples are the utterances a user would actually say. Every Example
// must route to units.currency alone on the semantic Fast path.
var currencyExamples = []string{
	"how much is 50 dollars in euros",
	"what is 100 euros in us dollars",
	"how much is 200 pounds in dollars",
	"50 euros to us dollars",
	"1000 yen to us dollars",
	"what is 75 canadian dollars in euros",
	"how much is 500 mexican pesos in dollars",
	"250 australian dollars in pounds",
	"how much is 30 swiss francs in euros",
	"what is 1000 indian rupees in dollars",
	"60 dollars to british pounds",
	"150 euros in japanese yen",
	"what is 40 dollars in canadian dollars",
	"how much is 999 euros in us dollars",
	"5000 won to us dollars",
	"how much is 10 dollars in swiss francs",
	"450 singapore dollars in us dollars",
	"what is 100 new zealand dollars in australian dollars",
	"how much is 1 dollar in euros",
	"300 norwegian kroner to euros",
	"25 brazilian reais in dollars",
	"how much is 800 hong kong dollars in us dollars",
	"what is 5 dollars in japanese yen",
}

// extractCurrency fills the Tool from ordinary currency phrasing. It returns
// false when either currency is unknown or the grammar does not match, so
// amount-less currency chatter ("what's the exchange rate for the euro") stays on
// the Agent path instead of becoming a Fast path guess.
func extractCurrency(text string) (CurrencyIn, bool) {
	raw := strings.ToLower(strings.TrimSpace(text))
	raw = strings.Trim(raw, " \t\r\n?!.,")
	raw = strings.ReplaceAll(raw, "-", " ")

	for _, prefix := range []string{
		"how much is ", "how much are ", "what is ", "what's ", "whats ",
		"what was ", "how many ", "convert ",
	} {
		raw = strings.TrimPrefix(raw, prefix)
	}
	raw = strings.TrimSpace(raw)

	connector := ""
	index := -1
	for _, candidate := range []string{" to ", " into ", " in "} {
		if at := strings.Index(raw, candidate); at >= 0 {
			connector, index = candidate, at
			break
		}
	}
	if index < 0 {
		return CurrencyIn{}, false
	}
	left := strings.TrimSpace(raw[:index])
	right := strings.TrimSpace(raw[index+len(connector):])

	value, fromCur, ok := amountAndCurrency(left)
	if !ok {
		return CurrencyIn{}, false
	}
	fromISO, ok := resolveCurrency(fromCur)
	if !ok {
		return CurrencyIn{}, false
	}
	toISO, ok := resolveCurrency(right)
	if !ok {
		return CurrencyIn{}, false
	}
	return CurrencyIn{Value: value, From: fromISO, To: toISO}, true
}

// amountAndCurrency reads "<amount> <currency>" and resolves the currency.
func amountAndCurrency(phrase string) (float64, string, bool) {
	phrase = strings.TrimSpace(phrase)
	phrase = strings.TrimPrefix(phrase, "about ")
	phrase = strings.TrimPrefix(phrase, "roughly ")
	if match := amountPattern.FindStringSubmatch(phrase); match != nil {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return 0, "", false
		}
		cur := strings.TrimSpace(match[2])
		if _, ok := resolveCurrency(cur); !ok {
			return 0, "", false
		}
		return value, cur, true
	}
	return 0, "", false
}
