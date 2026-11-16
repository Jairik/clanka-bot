package weather

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// ForecastName is the canonical Tool name for the daily forecast.
const ForecastName = "weather.forecast"

// umbrellaThreshold is the rain-probability percent at or above which the
// forecast Tool answers "yes" to an umbrella question (#240).
const umbrellaThreshold = 40

// dailyFields is the one Open-Meteo daily request, exactly as the issue
// specifies.
const dailyFields = "temperature_2m_max,temperature_2m_min,precipitation_probability_max,weather_code,sunrise,sunset"

// dailyTTL is the separate 30-minute cache for daily forecasts, keyed by city
// and day.
const dailyTTL = 30 * time.Minute

// ForecastIn names a city, a day and whether the user is asking about an
// umbrella. Day is one of "today", "tomorrow" or a weekday name.
type ForecastIn struct {
	City     string `json:"city,omitempty"`
	Day      string `json:"day,omitempty"`
	Umbrella bool   `json:"umbrella,omitempty"`
}

// ForecastOut is a daily forecast for one city and day. It carries local clock
// times only; coordinates never leave the provider and are never stored.
type ForecastOut struct {
	City       string  `json:"city"`
	Day        string  `json:"day"`
	Condition  string  `json:"condition"`
	HighC      float64 `json:"high_c"`
	LowC       float64 `json:"low_c"`
	RainChance int     `json:"rain_chance"`
	Sunrise    string  `json:"sunrise"`
	Sunset     string  `json:"sunset"`
	// Umbrella marks an umbrella question so Speak can answer yes or no from
	// RainChance (>= 40%). It is a request intent, cleared before caching.
	Umbrella bool `json:"umbrella,omitempty"`
}

// weekdayNames maps a lower-cased weekday name to its time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

// forecastRangeTokens are multi-day phrases the forecast Tool must not claim:
// they stay on the Agent path. Single days (today, tomorrow, a weekday) are
// fine; ranges are not.
var forecastRangeTokens = []string{
	"weekend", "this week", "next week", "this month",
	"next 5 days", "next five days", "next 7 days", "next seven days",
	"5 days", "seven days", "7 days", "next few days", "coming days",
	"all week", "over the next", "the next",
}

// NewForecast builds the weather.forecast Tool. It reuses the geocode cache and
// shares the Open-Meteo / wttr.in providers with weather.current, with a
// separate 30-minute cache keyed by city and day.
func NewForecast(deps Deps) tool.Tool[ForecastIn, ForecastOut] {
	if deps.FallbackDelay <= 0 {
		deps.FallbackDelay = defaultFallbackDelay
	}
	if deps.Budget <= 0 {
		deps.Budget = defaultBudget
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: deps.Budget}
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
	if deps.Now == nil {
		deps.Now = time.Now
	}

	cache := newCache()
	return tool.Tool[ForecastIn, ForecastOut]{
		Name:        ForecastName,
		Description: "Get the daily forecast — today, tomorrow or a named weekday — for where you are or a named city: highs, lows, rain chance, sunrise and sunset.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     deps.Budget,
		Handler: func(ctx context.Context, in ForecastIn) (ForecastOut, error) {
			return forecastHandler(ctx, deps, in, cache)
		},
		Speak: forecastSpeak,
		Examples: []string{
			"what's the forecast for today",
			"what's the forecast for tomorrow",
			"what's the forecast for Friday",
			"what's the forecast for Wednesday",
			"what's the forecast for London tomorrow",
			"do I need an umbrella today",
			"what's the high tomorrow",
			"what's the high temperature tomorrow",
			"what's the high on Thursday",
			"what's the low tomorrow",
			"what are the highs and lows tomorrow",
			"when is the sunset",
			"what time is the sunset",
			"what time does the sun rise",
			"when's sunrise",
			"what time is sunset tomorrow",
			"when does the sun set in Paris",
			"do I need an umbrella tomorrow",
			"will I need an umbrella tomorrow",
			"should I bring an umbrella tomorrow",
			"do I need a coat tomorrow",
		},
		Extract: forecastExtract,
	}
}

// forecastHandler is the weather.forecast handler: resolve the city, reuse the
// 30-minute city+day cache, then race Open-Meteo against the wttr.in fallback.
func forecastHandler(ctx context.Context, deps Deps, in ForecastIn, cache *cache) (ForecastOut, error) {
	if deps.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.Budget)
		defer cancel()
	}
	city, err := resolveCityName(in.City, deps)
	if err != nil {
		return ForecastOut{}, err
	}
	day := normalizeDay(in.Day)
	key := cacheKeyDay(city, day)
	cache.mu.Lock()
	entry, ok := cache.daily[key]
	cache.mu.Unlock()
	if ok && time.Since(entry.at) < dailyTTL {
		out := entry.value
		out.Umbrella = in.Umbrella // the umbrella intent is per request, not cached
		return out, nil
	}
	out, err := raceFetch[ForecastOut](ctx, deps, []func() (ForecastOut, error){
		func() (ForecastOut, error) { return primaryForecast(ctx, deps, cache, city, day, in.Umbrella) },
		func() (ForecastOut, error) { return fallbackForecastDaily(ctx, deps, cache, city, day, in.Umbrella) },
	})
	if err != nil {
		return ForecastOut{}, userFacing(err)
	}
	// Clear the umbrella intent before caching so the day's forecast is stored
	// once and the request flag is applied on the way out.
	store := out
	store.Umbrella = false
	cache.mu.Lock()
	cache.daily[key] = cacheEntry[ForecastOut]{value: store, at: time.Now()}
	cache.mu.Unlock()
	return out, nil
}

// primaryForecast is the Open-Meteo daily path. The one call returns every field
// the Tool needs; the day maps to a daily index in the location's timezone.
func primaryForecast(ctx context.Context, deps Deps, c *cache, city, day string, umbrella bool) (ForecastOut, error) {
	place, err := cachedGeocode(ctx, deps, c, city)
	if err != nil {
		return ForecastOut{}, err
	}
	resp, err := forecastDaily(ctx, deps, place.Latitude, place.Longitude)
	if err != nil {
		return ForecastOut{}, err
	}
	idx, err := dayIndex(day, place.Timezone, resp.Daily.Time, deps.Now())
	if err != nil {
		return ForecastOut{}, err
	}
	if idx < 0 || idx >= len(resp.Daily.Time) {
		return ForecastOut{}, &tool.ToolError{Say: "That day is a bit too far out for my forecast."}
	}
	block := resp.Daily
	return ForecastOut{
		City:       place.Name,
		Day:        day,
		Condition:  condition(block.WeatherCode[idx]),
		HighC:      block.TemperatureMax[idx],
		LowC:       block.TemperatureMin[idx],
		RainChance: block.PrecipProbabilityMax[idx],
		Sunrise:    block.Sunrise[idx],
		Sunset:     block.Sunset[idx],
		Umbrella:   umbrella,
	}, nil
}

// fallbackForecastDaily is the wttr.in j1 path, used when Open-Meteo is
// unavailable. wttr.in's weather[] daily blocks carry the same fields.
func fallbackForecastDaily(ctx context.Context, deps Deps, c *cache, city, day string, umbrella bool) (ForecastOut, error) {
	var body struct {
		Weather []struct {
			Date      string `json:"date"`
			MaxTempC  string `json:"maxtempC"`
			MinTempC  string `json:"mintempC"`
			Astronomy []struct {
				Sunrise string `json:"sunrise"`
				Sunset  string `json:"sunset"`
			} `json:"astronomy"`
			Hourly []struct {
				Time        string `json:"time"`
				WeatherDesc []struct {
					Value string `json:"value"`
				} `json:"weatherDesc"`
				ChanceOfRain string `json:"chanceofrain"`
			} `json:"hourly"`
		} `json:"weather"`
	}
	target := strings.TrimRight(deps.FallbackURL, "/") + "/" + url.PathEscape(city) + "?format=j1&lang=en"
	if err := getJSON(ctx, deps.HTTP, target, &body); err != nil {
		return ForecastOut{}, err
	}
	if len(body.Weather) == 0 {
		return ForecastOut{}, fmt.Errorf("weather: fallback has no daily forecast")
	}
	place, err := cachedGeocode(ctx, deps, c, city)
	if err != nil {
		// The fallback resolves the city itself, so a geocode miss here means
		// wttr.in would have guessed; report the unknown place truthfully.
		return ForecastOut{}, err
	}
	times := make([]string, len(body.Weather))
	for i, d := range body.Weather {
		times[i] = d.Date
	}
	idx, err := dayIndex(day, place.Timezone, times, deps.Now())
	if err != nil {
		return ForecastOut{}, err
	}
	if idx < 0 || idx >= len(body.Weather) {
		return ForecastOut{}, &tool.ToolError{Say: "That day is a bit too far out for my forecast."}
	}
	block := body.Weather[idx]

	if len(block.Astronomy) == 0 {
		return ForecastOut{}, fmt.Errorf("weather: fallback has no astronomy for the day")
	}
	sunrise := convertClock(block.Astronomy[0].Sunrise)
	sunset := convertClock(block.Astronomy[0].Sunset)

	// The midday conditions are the day's headline; the max rain chance across
	// the hourly blocks is the day's rain probability.
	conditionText := ""
	maxRain := 0
	for _, h := range block.Hourly {
		if h.Time == "1200" && len(h.WeatherDesc) > 0 && conditionText == "" {
			conditionText = strings.ToLower(strings.TrimSpace(h.WeatherDesc[0].Value))
		}
		if r, perr := strconv.Atoi(h.ChanceOfRain); perr == nil && r > maxRain {
			maxRain = r
		}
	}
	if conditionText == "" {
		for _, h := range block.Hourly {
			if len(h.WeatherDesc) > 0 {
				conditionText = strings.ToLower(strings.TrimSpace(h.WeatherDesc[0].Value))
				break
			}
		}
	}
	high, err := strconv.ParseFloat(block.MaxTempC, 64)
	if err != nil {
		return ForecastOut{}, fmt.Errorf("weather: fallback has non-numeric high %q", block.MaxTempC)
	}
	low, err := strconv.ParseFloat(block.MinTempC, 64)
	if err != nil {
		return ForecastOut{}, fmt.Errorf("weather: fallback has non-numeric low %q", block.MinTempC)
	}
	if conditionText == "" {
		return ForecastOut{}, fmt.Errorf("weather: fallback has no condition for the day")
	}

	return ForecastOut{
		City:       city,
		Day:        day,
		Condition:  conditionText,
		HighC:      high,
		LowC:       low,
		RainChance: maxRain,
		Sunrise:    sunrise,
		Sunset:     sunset,
		Umbrella:   umbrella,
	}, nil
}

// forecastDaily calls Open-Meteo's forecast endpoint with the daily fields and
// forecast_days=3, returning the daily block.
func forecastDaily(ctx context.Context, deps Deps, latitude, longitude float64) (dailyResponse, error) {
	query := url.Values{}
	query.Set("latitude", strconv.FormatFloat(latitude, 'f', 4, 64))
	query.Set("longitude", strconv.FormatFloat(longitude, 'f', 4, 64))
	query.Set("daily", dailyFields)
	query.Set("timezone", "auto")
	query.Set("forecast_days", "3")
	var body dailyResponse
	if err := getJSON(ctx, deps.HTTP, deps.ForecastURL+"?"+query.Encode(), &body); err != nil {
		return dailyResponse{}, err
	}
	if len(body.Daily.Time) == 0 {
		return dailyResponse{}, fmt.Errorf("weather: forecast has no daily data")
	}
	return body, nil
}

// dailyResponse is the Open-Meteo daily payload we read.
type dailyResponse struct {
	Timezone string `json:"timezone"`
	Daily    struct {
		Time                 []string  `json:"time"`
		TemperatureMax       []float64 `json:"temperature_2m_max"`
		TemperatureMin       []float64 `json:"temperature_2m_min"`
		PrecipProbabilityMax []int     `json:"precipitation_probability_max"`
		WeatherCode          []int     `json:"weather_code"`
		Sunrise              []string  `json:"sunrise"`
		Sunset               []string  `json:"sunset"`
	} `json:"daily"`
}

// dayIndex maps today / tomorrow / a weekday name to the right daily index in
// the location's timezone. today and tomorrow are computed from now in that
// zone; a weekday name matches the first occurrence within the returned window.
func dayIndex(day, tz string, times []string, now time.Time) (int, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	switch day {
	case "today":
		return dateIndex(times, now.In(loc).Format("2006-01-02"))
	case "tomorrow":
		return dateIndex(times, now.In(loc).AddDate(0, 0, 1).Format("2006-01-02"))
	}
	wd, ok := weekdayNames[day]
	if !ok {
		return 0, &tool.ToolError{Say: "I didn't catch which day you mean."}
	}
	for i, ts := range times {
		d, perr := time.Parse("2006-01-02", ts)
		if perr != nil {
			continue
		}
		if d.Weekday() == wd {
			return i, nil
		}
	}
	return 0, &tool.ToolError{Say: "That day is a bit too far out for my forecast."}
}

// dateIndex returns the position of want in times, or an error when it is not
// in the returned forecast window.
func dateIndex(times []string, want string) (int, error) {
	for i, ts := range times {
		if ts == want {
			return i, nil
		}
	}
	return 0, &tool.ToolError{Say: "That day is a bit too far out for my forecast."}
}

// normalizeDay canonicalises the day token: empty and "today" collapse to
// "today", "tomorrow" stays, and a recognised weekday name is lower-cased.
func normalizeDay(day string) string {
	d := strings.ToLower(strings.TrimSpace(day))
	switch d {
	case "", "today":
		return "today"
	case "tomorrow":
		return "tomorrow"
	}
	if _, ok := weekdayNames[d]; ok {
		return d
	}
	return d
}

// forecastExtract fills the optional city, day and umbrella intent. It rejects
// multi-day ranges so they stay on the Agent path.
func forecastExtract(text string) (ForecastIn, bool) {
	lower := strings.ToLower(text)
	for _, token := range forecastRangeTokens {
		if strings.Contains(lower, token) {
			return ForecastIn{}, false
		}
	}
	in := ForecastIn{Day: "today"}
	if strings.Contains(lower, "tomorrow") {
		in.Day = "tomorrow"
	} else {
		for name := range weekdayNames {
			if forecastHasWord(lower, name) {
				in.Day = name
				break
			}
		}
	}
	in.Umbrella = strings.Contains(lower, "umbrella") || strings.Contains(lower, "raincoat")
	if idx := strings.Index(lower, " in "); idx >= 0 {
		// The original-cased text shares the ASCII " in " offset with lower.
		city := strings.TrimSpace(text[idx+len(" in "):])
		// Drop the day token (e.g. "Berlin tomorrow") and any leftover
		// preposition so the geocoder gets a clean city name.
		if in.Day != "today" {
			city = removeWordCI(city, in.Day)
			city = strings.TrimSpace(city)
			if strings.HasPrefix(strings.ToLower(city), "on ") {
				city = strings.TrimSpace(city[3:])
			}
			if strings.HasPrefix(strings.ToLower(city), "for ") {
				city = strings.TrimSpace(city[4:])
			}
		}
		city = strings.Trim(city, " ?.!,;:")
		in.City = city
	}
	return in, true
}

// removeWordCI drops the first case-insensitive occurrence of word from s.
func removeWordCI(s, word string) string {
	lower := strings.ToLower(s)
	word = strings.ToLower(word)
	if idx := strings.Index(lower, word); idx >= 0 {
		return strings.TrimSpace(s[:idx] + s[idx+len(word):])
	}
	return strings.TrimSpace(s)
}

// forecastHasWord reports whether word appears as a whole word in text.
func forecastHasWord(text, word string) bool {
	for _, part := range strings.Fields(text) {
		if strings.Trim(part, " ?.!,;:") == word {
			return true
		}
	}
	return false
}

// convertClock normalises a wttr.in clock string ("06:42 AM") to a 24-hour
// local time ("06:42").
func convertClock(s string) string {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("03:04 PM", s); err == nil {
		return t.Format("15:04")
	}
	if t, err := time.Parse("15:04", s); err == nil {
		return t.Format("15:04")
	}
	return s
}

// forecastSpeak renders the Fast path reply. An umbrella question answers yes or
// no from the rain probability; otherwise it spells out the day's forecast.
func forecastSpeak(out ForecastOut) string {
	if out.Umbrella {
		if out.RainChance >= umbrellaThreshold {
			return fmt.Sprintf("Yes, you'll likely need an umbrella in %s %s — there's a %d percent chance of rain.", out.City, dayPhrase(out.Day), out.RainChance)
		}
		return fmt.Sprintf("No, you shouldn't need an umbrella in %s %s.", out.City, dayPhrase(out.Day))
	}
	return fmt.Sprintf("%s in %s: %s, high of %.0f, low of %.0f, %d percent chance of rain. Sunrise %s, sunset %s.",
		capitalize(out.Day), out.City, out.Condition, out.HighC, out.LowC, out.RainChance, out.Sunrise, out.Sunset)
}

// dayPhrase turns a day token into a spoken phrase fragment.
func dayPhrase(day string) string {
	switch normalizeDay(day) {
	case "today":
		return "today"
	case "tomorrow":
		return "tomorrow"
	}
	return "on " + strings.Title(day) // weekday name
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// raceFetch starts the providers in order, keeping the first successful answer
// and falling back after a delay or on a non-definitive failure. It is the
// generic form of weather.current's race helper. A ToolError (for example an
// unknown place) is definitive: no further provider starts, and it is returned
// if nothing else succeeds.
func raceFetch[T any](ctx context.Context, deps Deps, providers []func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	results := make(chan result, len(providers))
	started := 0
	startNext := func() {
		provider := providers[started]
		started++
		go func() {
			value, err := provider()
			results <- result{value: value, err: err}
		}()
	}
	startNext()
	delay := time.NewTimer(deps.FallbackDelay)
	defer delay.Stop()
	var lastErr, sayErr error
	for completed := 0; completed < started; {
		select {
		case <-ctx.Done():
			if sayErr != nil {
				return zero[T](), sayErr
			}
			return zero[T](), ctx.Err()
		case reply := <-results:
			completed++
			if reply.err == nil {
				return reply.value, nil
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
		return zero[T](), sayErr
	}
	return zero[T](), lastErr
}

func zero[T any]() T {
	var z T
	return z
}
