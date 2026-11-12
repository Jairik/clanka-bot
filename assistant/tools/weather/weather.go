// Package weather implements the weather.current Tool against Open-Meteo,
// which needs no API key. It geocodes the city name locally; the IP lookup's
// coordinates are never used.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical Tool name.
const Name = "weather.current"

const (
	// DefaultGeocodingURL is Open-Meteo's city search.
	DefaultGeocodingURL = "https://geocoding-api.open-meteo.com/v1/search"
	// DefaultForecastURL is Open-Meteo's current-conditions endpoint.
	DefaultForecastURL = "https://api.open-meteo.com/v1/forecast"
	currentFields      = "temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m"
	maxResponseBytes   = 256 << 10
	defaultHTTPTimeout = 1500 * time.Millisecond
	defaultBudget      = 2 * time.Second
	geocodeTTL         = 10 * time.Minute
	forecastTTL        = time.Minute
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
	WindKph      float64 `json:"wind_speed_kph"`
	Humidity     int     `json:"relative_humidity_percent"`
}

// Deps are the Tool's outside dependencies. Tests replace the URLs and the
// location source; production leaves the defaults.
type Deps struct {
	HTTP         *http.Client
	GeocodingURL string
	ForecastURL  string
	Location     func() (location.Location, bool)
	// Timeout bounds each upstream call; zero uses 1.5s. Budget bounds
	// geocode plus forecast together; zero uses 2s. Both keep a bad upstream
	// from turning one Fast path request into a 6+ second turn.
	Timeout time.Duration
	Budget  time.Duration
}

// New builds the weather.current Tool.
func New(deps Deps) tool.Tool[In, Out] {
	if deps.Timeout <= 0 {
		deps.Timeout = defaultHTTPTimeout
	}
	if deps.Budget <= 0 {
		deps.Budget = defaultBudget
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: deps.Timeout}
	}
	if deps.GeocodingURL == "" {
		deps.GeocodingURL = DefaultGeocodingURL
	}
	if deps.ForecastURL == "" {
		deps.ForecastURL = DefaultForecastURL
	}

	cache := newCache()
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Get the current weather where the user is, or in a named city.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     8 * time.Second,
		Handler: func(ctx context.Context, in In) (Out, error) {
			return current(ctx, deps, in, cache)
		},
		Speak: speak,
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

// speak renders conditions for the Fast path.
func speak(out Out) string {
	unit := "degrees"
	if out.TemperatureC > -1 && out.TemperatureC < 1 {
		unit = "degree"
	}
	return fmt.Sprintf("It's %.0f %s and %s in %s.", out.TemperatureC, unit, out.Condition, out.City)
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

	place, err := cachedGeocode(ctx, deps, cache, city)
	if err != nil {
		return Out{}, userFacing(err)
	}
	forecast, err := cachedForecast(ctx, deps, cache, place)
	if err != nil {
		return Out{}, userFacing(err)
	}

	return Out{
		City:         place.Name,
		Condition:    condition(forecast.Current.WeatherCode),
		TemperatureC: forecast.Current.Temperature,
		ApparentC:    forecast.Current.Apparent,
		WindKph:      forecast.Current.Wind,
		Humidity:     forecast.Current.Humidity,
	}, nil
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
	forecasts map[string]cacheEntry[forecastResponse]
}

func newCache() *cache {
	return &cache{
		places:    make(map[string]cacheEntry[place]),
		forecasts: make(map[string]cacheEntry[forecastResponse]),
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

func forecastKey(p place) string {
	return strconv.FormatFloat(p.Latitude, 'f', 4, 64) + "," + strconv.FormatFloat(p.Longitude, 'f', 4, 64)
}

func cachedForecast(ctx context.Context, deps Deps, c *cache, p place) (forecastResponse, error) {
	key := forecastKey(p)
	if c != nil {
		c.mu.Lock()
		entry, ok := c.forecasts[key]
		c.mu.Unlock()
		if ok && time.Since(entry.at) < forecastTTL {
			return entry.value, nil
		}
	}
	value, err := forecast(ctx, deps, p.Latitude, p.Longitude)
	if err != nil {
		return forecastResponse{}, err
	}
	if c != nil {
		c.mu.Lock()
		c.forecasts[key] = cacheEntry[forecastResponse]{value: value, at: time.Now()}
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
