// Package weather implements weather.current with WeatherAPI, Open-Meteo and wttr.in.
// Providers resolve city names; the IP lookup's coordinates are never used.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical Tool name.
const Name = "weather.current"

// SecretName is the optional WeatherAPI key. The module declares it, so the
// write-only store can hold it and the Dashboard Secrets page lists it; the
// .env variable of the same name is the bootstrap.
const SecretName = "WEATHERAPI_API_KEY"

// UnitKey is the live temperature-unit preference declared on the Dashboard
// Settings page (issue #318). Its value is one of UnitAuto, UnitCelsius or
// UnitFahrenheit.
const UnitKey = "weather.temperature_unit"

const (
	UnitAuto       = "auto"
	UnitCelsius    = "celsius"
	UnitFahrenheit = "fahrenheit"
)

// usTimezones are the IANA zones where Fahrenheit is the everyday unit. "auto"
// picks Fahrenheit for these and Celsius for everywhere else (issue #318). A
// fixed table keeps the answer independent of the host's locale or clock.
var usTimezones = map[string]bool{
	"America/New_York":               true,
	"America/Chicago":                true,
	"America/Denver":                 true,
	"America/Los_Angeles":            true,
	"America/Phoenix":                true,
	"America/Anchorage":              true,
	"Pacific/Honolulu":               true,
	"America/Detroit":                true,
	"America/Boise":                  true,
	"America/Indiana/Indianapolis":   true,
	"America/Kentucky/Louisville":    true,
	"America/North_Dakota/New_Salem": true,
	"America/Puerto_Rico":            true,
	"America/St_Thomas":              true,
	"Pacific/Guam":                   true,
	"Pacific/Pago_Pago":              true,
}

// Definitions declares the live temperature-unit setting. It is registered in
// the one settings service exactly like lights.* and spotify.*, so the
// Dashboard Settings page lists it and a write applies on the next weather
// request without a restart (issue #318).
func Definitions() []settings.Definition {
	return []settings.Definition{
		settings.String(UnitKey, UnitAuto,
			settings.WithDescription("Unit for weather replies: auto follows the location's timezone (Fahrenheit in US zones, Celsius elsewhere)."),
			settings.WithValidator(func(value any) error {
				text, ok := value.(string)
				if !ok || validUnit(text) != text {
					return fmt.Errorf("%s must be %q, %q or %q", UnitKey, UnitAuto, UnitCelsius, UnitFahrenheit)
				}
				return nil
			}),
		),
	}
}

// validUnit returns text when it is a declared value, otherwise the "auto"
// default. Closed-form so the validator, the live reader and the renderer all
// agree on what is legal.
func validUnit(text string) string {
	switch text {
	case UnitAuto, UnitCelsius, UnitFahrenheit:
		return text
	default:
		return UnitAuto
	}
}

// UnitFrom reads the live unit preference. A nil service or an unexpected
// stored value falls back to the code default; callers still resolve "auto"
// against the location (issue #318).
func UnitFrom(svc interface {
	Get(string) (settings.Value, bool)
}) string {
	if svc == nil {
		return UnitAuto
	}
	value, ok := svc.Get(UnitKey)
	if !ok {
		return UnitAuto
	}
	text, ok := value.Value.(string)
	if !ok {
		return UnitAuto
	}
	return validUnit(text)
}

// ToFahrenheit and ToMph keep Out's stored Celsius/km/h values authoritative
// while exposing Fahrenheit/mph for the Agent and the spoken renderer.
func ToFahrenheit(celsius float64) float64 { return celsius*9/5 + 32 }
func ToMph(kph float64) float64            { return kph / 1.609344 }

// withConversions populates the alternate-unit fields from the provider's
// Celsius and km/h values. Providers report metric; every Out that leaves the
// package carries both units.
func withConversions(out Out) Out {
	out.TemperatureF = ToFahrenheit(out.TemperatureC)
	out.ApparentF = ToFahrenheit(out.ApparentC)
	out.WindMph = ToMph(out.WindKph)
	return out
}

const (
	// DefaultWeatherAPIURL resolves a city and returns current conditions in one call.
	DefaultWeatherAPIURL = "https://api.weatherapi.com/v1/current.json"
	// DefaultGeocodingURL is Open-Meteo's city search.
	DefaultGeocodingURL = "https://geocoding-api.open-meteo.com/v1/search"
	// DefaultForecastURL is Open-Meteo's current-conditions endpoint.
	DefaultForecastURL = "https://api.open-meteo.com/v1/forecast"
	// DefaultFallbackURL serves wttr.in's current conditions when Open-Meteo fails.
	DefaultFallbackURL   = "https://wttr.in"
	currentFields        = "temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m"
	maxResponseBytes     = 256 << 10
	defaultFallbackDelay = 250 * time.Millisecond
	// Leave roughly one second of the Fast path budget for endpointing, STT and TTS.
	defaultBudget = time.Second
	geocodeTTL    = 10 * time.Minute
	// Reuse successful conditions for 15 minutes to avoid repeated network waits.
	forecastTTL = 15 * time.Minute
)

// upstreamUnavailable is the truthful message for a timeout, transport error
// or upstream 5xx. The user can retry; we never invent a weather answer.
const upstreamUnavailable = "Weather is unavailable right now."

// In optionally names a city. An empty city uses the current location.
type In struct {
	City string `json:"city,omitempty"`
}

// Out is current conditions for one city. It deliberately has no coordinates:
// this result may go to the model.
type Out struct {
	City         string  `json:"city"`
	Condition    string  `json:"condition"`
	TemperatureC float64 `json:"temperature_c"`
	ApparentC    float64 `json:"apparent_temperature_c"`
	// Fahrenheit and mph mirror the metric values above so the Agent can pick
	// either unit. They are derived in withConversions, never spoken directly.
	TemperatureF float64 `json:"temperature_f"`
	ApparentF    float64 `json:"apparent_temperature_f"`
	WindKph      float64 `json:"wind_speed_kph"`
	WindMph      float64 `json:"wind_speed_mph"`
	Humidity     int     `json:"relative_humidity_percent"`
}

// Deps are the Tool's outside dependencies. Tests replace the URLs and the
// location source; production leaves the defaults.
type Deps struct {
	HTTP *http.Client
	// APIKey returns the optional WeatherAPI key. Nil or empty skips that
	// provider and leaves the two keyless ones. The module reads it per
	// request, so a Dashboard rotation applies without a restart.
	APIKey        func() (string, bool)
	WeatherAPIURL string
	GeocodingURL  string
	ForecastURL   string
	FallbackURL   string
	Location      func() (location.Location, bool)
	// Settings returns the live settings service. The module supplies it so
	// the Fast path renderer reads the temperature unit per reply (#318).
	// Nil means only the code default ("auto") is available.
	Settings func() *settings.Service
	// FallbackDelay starts the next provider if one is still pending; zero uses 250ms.
	// Budget bounds all providers together; zero uses 1s.
	FallbackDelay time.Duration
	Budget        time.Duration
}

// New builds the weather.current Tool.
func New(deps Deps) tool.Tool[In, Out] {
	if deps.FallbackDelay <= 0 {
		deps.FallbackDelay = defaultFallbackDelay
	}
	if deps.Budget <= 0 {
		deps.Budget = defaultBudget
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: deps.Budget}
	}
	if deps.WeatherAPIURL == "" {
		deps.WeatherAPIURL = DefaultWeatherAPIURL
	}
	if deps.GeocodingURL == "" {
		deps.GeocodingURL = DefaultGeocodingURL
	}
	if deps.ForecastURL == "" {
		deps.ForecastURL = DefaultForecastURL
	}
	if deps.FallbackURL == "" {
		deps.FallbackURL = DefaultFallbackURL
	}

	cache := newCache()
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Get the current weather where the user is, or in a named city.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     deps.Budget,
		Handler: func(ctx context.Context, in In) (Out, error) {
			return current(ctx, deps, in, cache)
		},
		Speak: speakFor(deps),
		Examples: []string{
			"what's the weather like",
			"what's the weather",
			"how's the weather outside",
			"what's the temperature outside",
			"is it raining",
			"weather in Berlin",
		},
		Extract: extract,
	}
}

// Register adds weather.current to the registry.
func Register(r *tool.Registry, deps Deps) error {
	return tool.Add(r, New(deps))
}

// extract fills the optional city from "<something> in <city>".
func extract(text string) (In, bool) {
	lower := strings.ToLower(text)
	index := strings.Index(lower, " in ")
	if index < 0 {
		return In{}, true
	}
	city := strings.TrimSpace(text[index+len(" in "):])
	city = strings.Trim(city, " ?.!,;:")
	if city == "" {
		return In{}, false
	}
	return In{City: city}, true
}

// speakFor builds the Fast path renderer. It reads the live setting and the
// current location at speak time, so a Dashboard change applies to the next
// reply without rebuilding the Tool. The effective unit is resolved once per
// answer; the same weather never depends on what was spoken before (#318).
func speakFor(deps Deps) func(Out) string {
	return func(out Out) string { return speak(out, effectiveUnit(deps)) }
}

// effectiveUnit resolves the preference to a concrete unit. "auto" follows the
// stored location's timezone; explicit celsius/fahrenheit always win.
func effectiveUnit(deps Deps) string {
	preference := UnitAuto
	if deps.Settings != nil {
		if svc := deps.Settings(); svc != nil {
			preference = UnitFrom(svc)
		}
	}
	if preference != UnitAuto {
		return preference
	}
	timezone := ""
	if deps.Location != nil {
		if loc, ok := deps.Location(); ok {
			timezone = strings.TrimSpace(loc.Timezone)
		}
	}
	if usTimezones[timezone] {
		return UnitFahrenheit
	}
	return UnitCelsius
}

// speak renders conditions for the Fast path, always naming the unit once.
func speak(out Out, unit string) string {
	temperature := out.TemperatureC
	unitWord := "Celsius"
	if unit == UnitFahrenheit {
		temperature = ToFahrenheit(out.TemperatureC)
		unitWord = "Fahrenheit"
	}
	rounded := math.Round(temperature)
	degree := "degrees"
	if math.Abs(rounded) == 1 {
		degree = "degree"
	}
	return fmt.Sprintf("It's %.0f %s %s and %s in %s.", rounded, degree, unitWord, out.Condition, out.City)
}

func current(ctx context.Context, deps Deps, in In, cache *cache) (Out, error) {
	if deps.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.Budget)
		defer cancel()
	}
	city, err := resolveCity(in, deps)
	if err != nil {
		return Out{}, err
	}

	// Cache the whole answer so fallback hits also bypass a failing geocoder.
	key := cacheKey(city)
	cache.mu.Lock()
	entry, ok := cache.forecasts[key]
	cache.mu.Unlock()
	if ok && time.Since(entry.at) < forecastTTL {
		return entry.value, nil
	}
	out, err := fetchCurrent(ctx, deps, cache, city)
	if err != nil {
		return Out{}, userFacing(err)
	}
	cache.mu.Lock()
	cache.forecasts[key] = cacheEntry[Out]{value: out, at: time.Now()}
	cache.mu.Unlock()
	return out, nil
}

// apiKey resolves the optional WeatherAPI key. A nil resolver or a blank value
// means the two keyless providers are used alone.
func apiKey(deps Deps) (string, bool) {
	if deps.APIKey == nil {
		return "", false
	}
	key, ok := deps.APIKey()
	key = strings.TrimSpace(key)
	return key, ok && key != ""
}

// fetchCurrent starts fallback on primary failure or delay, keeping the first successful answer.
func fetchCurrent(ctx context.Context, deps Deps, cache *cache, city string) (Out, error) {
	type result struct {
		out Out
		err error
	}
	providers := []func() (Out, error){
		func() (Out, error) { return primaryCurrent(ctx, deps, cache, city) },
		func() (Out, error) { return fallbackForecast(ctx, deps, city) },
	}
	// Without a configured key, the two keyless providers remain usable.
	if key, ok := apiKey(deps); ok {
		providers = append([]func() (Out, error){
			func() (Out, error) { return weatherAPICurrent(ctx, deps, key, city) },
		}, providers...)
	}
	// Buffered results let the cancelled loser exit without waiting for a receiver.
	results := make(chan result, len(providers))
	started := 0
	startNext := func() {
		provider := providers[started]
		started++
		go func() {
			out, err := provider()
			results <- result{out: out, err: err}
		}()
	}
	startNext()
	delay := time.NewTimer(deps.FallbackDelay)
	defer delay.Stop()
	// A ToolError ("I couldn't find that place") is a definitive answer about
	// the request, so no further provider starts: wttr.in would otherwise
	// guess some place for a name the geocoder rejected. It must not cancel a
	// provider already in flight, though: a fast keyless geocoder miss can
	// arrive while WeatherAPI is about to answer, and that answer still wins.
	var lastErr, sayErr error
	for completed := 0; completed < started; {
		select {
		case <-ctx.Done():
			if sayErr != nil {
				return Out{}, sayErr
			}
			return Out{}, ctx.Err()
		case reply := <-results:
			completed++
			if reply.err == nil {
				return reply.out, nil
			}
			var toolErr *tool.ToolError
			if errors.As(reply.err, &toolErr) {
				if sayErr == nil {
					sayErr = reply.err
				}
			} else {
				lastErr = reply.err
			}
		case <-delay.C:
		}
		if sayErr == nil && started < len(providers) && ctx.Err() == nil {
			startNext()
			delay.Reset(deps.FallbackDelay)
		}
	}
	if sayErr != nil {
		return Out{}, sayErr
	}
	return Out{}, lastErr
}

// primaryCurrent shares the overall deadline across Open-Meteo's two requests.
func primaryCurrent(ctx context.Context, deps Deps, cache *cache, city string) (Out, error) {
	place, err := cachedGeocode(ctx, deps, cache, city)
	if err != nil {
		return Out{}, err
	}
	forecast, err := forecast(ctx, deps, place.Latitude, place.Longitude)
	if err != nil {
		return Out{}, err
	}
	return withConversions(Out{
		City:         place.Name,
		Condition:    condition(forecast.Current.WeatherCode),
		TemperatureC: forecast.Current.Temperature,
		ApparentC:    forecast.Current.Apparent,
		WindKph:      forecast.Current.Wind,
		Humidity:     forecast.Current.Humidity,
	}), nil
}

func unknownLocation() error {
	return &tool.ToolError{Say: "I don't know where I am yet."}
}

// resolveCity uses the named city, then city, region and timezone from the
// current location, so an incomplete lookup still gives a geocodable name.
func resolveCity(in In, deps Deps) (string, error) {
	if city := strings.TrimSpace(in.City); city != "" {
		return city, nil
	}
	if deps.Location == nil {
		return "", unknownLocation()
	}
	loc, ok := deps.Location()
	if !ok {
		return "", unknownLocation()
	}
	for _, candidate := range []string{loc.City, loc.Region, loc.Timezone} {
		if city := strings.TrimSpace(candidate); city != "" {
			return city, nil
		}
	}
	return "", unknownLocation()
}

// userFacing keeps a specific ToolError (unknown place, unknown location) but
// turns a timeout, transport error or upstream status into one truthful,
// actionable message instead of a generic internal failure.
func userFacing(err error) error {
	var toolErr *tool.ToolError
	if errors.As(err, &toolErr) {
		return err
	}
	return &tool.ToolError{Say: upstreamUnavailable}
}

type cacheEntry[T any] struct {
	value T
	at    time.Time
}

// cache is a tiny in-process TTL cache. It is not persistence: a restart just
// starts empty, and the prototype stays within its single-process scope (#35).
type cache struct {
	mu        sync.Mutex
	places    map[string]cacheEntry[place]
	forecasts map[string]cacheEntry[Out]
}

func newCache() *cache {
	return &cache{
		places:    make(map[string]cacheEntry[place]),
		forecasts: make(map[string]cacheEntry[Out]),
	}
}

func cacheKey(city string) string { return strings.ToLower(strings.TrimSpace(city)) }

func cachedGeocode(ctx context.Context, deps Deps, c *cache, city string) (place, error) {
	key := cacheKey(city)
	if c != nil {
		c.mu.Lock()
		entry, ok := c.places[key]
		c.mu.Unlock()
		if ok && time.Since(entry.at) < geocodeTTL {
			return entry.value, nil
		}
	}
	value, err := geocode(ctx, deps, city)
	if err != nil {
		return place{}, err
	}
	if c != nil {
		c.mu.Lock()
		c.places[key] = cacheEntry[place]{value: value, at: time.Now()}
		c.mu.Unlock()
	}
	return value, nil
}

type place struct {
	Name      string
	Latitude  float64
	Longitude float64
}

type geocodingResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"results"`
}

func geocode(ctx context.Context, deps Deps, city string) (place, error) {
	query := url.Values{}
	query.Set("name", city)
	query.Set("count", "1")
	query.Set("language", "en")
	query.Set("format", "json")
	var body geocodingResponse
	if err := getJSON(ctx, deps.HTTP, deps.GeocodingURL+"?"+query.Encode(), &body); err != nil {
		return place{}, err
	}
	if len(body.Results) == 0 {
		return place{}, &tool.ToolError{Say: "I couldn't find that place."}
	}
	result := body.Results[0]
	if result.Name == "" || (result.Latitude == 0 && result.Longitude == 0) {
		return place{}, &tool.ToolError{Say: "I couldn't find that place."}
	}
	return place{Name: result.Name, Latitude: result.Latitude, Longitude: result.Longitude}, nil
}

type forecastResponse struct {
	Current struct {
		Temperature float64 `json:"temperature_2m"`
		Apparent    float64 `json:"apparent_temperature"`
		Humidity    int     `json:"relative_humidity_2m"`
		WeatherCode int     `json:"weather_code"`
		Wind        float64 `json:"wind_speed_10m"`
	} `json:"current"`
}

func forecast(ctx context.Context, deps Deps, latitude, longitude float64) (forecastResponse, error) {
	query := url.Values{}
	query.Set("latitude", strconv.FormatFloat(latitude, 'f', 4, 64))
	query.Set("longitude", strconv.FormatFloat(longitude, 'f', 4, 64))
	query.Set("current", currentFields)
	var body forecastResponse
	if err := getJSON(ctx, deps.HTTP, deps.ForecastURL+"?"+query.Encode(), &body); err != nil {
		return forecastResponse{}, err
	}
	return body, nil
}

func getJSON(ctx context.Context, client *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("weather: build request: %w", err)
	}
	// Identify the application consistently to both public weather services.
	req.Header.Set("User-Agent", "clanka-bot/0.1 (+https://github.com/Jairik/clanka-bot)")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("weather: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("weather: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("weather: decode response: %w", err)
	}
	return nil
}

// condition maps a WMO weather code to a short spoken phrase.
func condition(code int) string {
	switch code {
	case 0:
		return "clear skies"
	case 1:
		return "mainly clear"
	case 2:
		return "partly cloudy"
	case 3:
		return "overcast"
	case 45, 48:
		return "foggy"
	case 51, 53, 55, 56, 57:
		return "drizzling"
	case 61, 63, 65, 66, 67:
		return "raining"
	case 71, 73, 75, 77:
		return "snowing"
	case 80, 81, 82:
		return "rain showers"
	case 85, 86:
		return "snow showers"
	case 95, 96, 99:
		return "thunderstorms"
	default:
		return "changeable"
	}
}
