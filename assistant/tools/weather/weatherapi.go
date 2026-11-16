package weather

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// weatherAPIError is the truthful, credential-free message a failed WeatherAPI
// request contributes. It is not a ToolError, so the provider chain still falls
// back to the keyless providers; the module records the HTTP status separately
// for AuxHealth. status carries only the HTTP status code, never the URL or key.
type weatherAPIError struct {
	status int
}

func (e *weatherAPIError) Error() string {
	return "weather: WeatherAPI request failed"
}

// weatherAPICurrent resolves a city directly without a separate geocoding
// request. key is resolved per call so a rotated secret applies at once.
func weatherAPICurrent(ctx context.Context, deps Deps, key, city string) (Out, error) {
	query := url.Values{"key": {key}, "q": {city}, "aqi": {"no"}, "lang": {"en"}}
	var body struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
		Current struct {
			Temperature *float64 `json:"temp_c"`
			Apparent    *float64 `json:"feelslike_c"`
			Wind        *float64 `json:"wind_kph"`
			Humidity    *int     `json:"humidity"`
			Condition   struct {
				Text string `json:"text"`
			} `json:"condition"`
		} `json:"current"`
	}
	// Credential-bearing requests must not follow redirects to another endpoint.
	client := *deps.HTTP
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if err := getJSON(ctx, &client, deps.WeatherAPIURL+"?"+query.Encode(), &body); err != nil {
		// Record the outcome so a dead or rate-limited key shows up in
		// AuxHealth; the chain still falls back, so the answer stays green.
		if deps.RecordWeatherAPIOutcome != nil {
			var se *statusError
			if errors.As(err, &se) {
				deps.RecordWeatherAPIOutcome(se.status)
			} else {
				deps.RecordWeatherAPIOutcome(0)
			}
		}
		// Transport errors can include the query-string key; never return them verbatim.
		return Out{}, &weatherAPIError{status: statusOf(err)}
	}
	current := body.Current
	if strings.TrimSpace(body.Location.Name) == "" || strings.TrimSpace(current.Condition.Text) == "" ||
		current.Temperature == nil || current.Apparent == nil || current.Wind == nil || current.Humidity == nil {
		return Out{}, fmt.Errorf("weather: WeatherAPI returned incomplete current conditions")
	}
	if *current.Wind < 0 || *current.Humidity < 0 || *current.Humidity > 100 {
		return Out{}, fmt.Errorf("weather: WeatherAPI returned invalid current conditions")
	}
	if deps.RecordWeatherAPIOutcome != nil {
		deps.RecordWeatherAPIOutcome(http.StatusOK)
	}
	return withConversions(Out{
		City:         strings.TrimSpace(body.Location.Name),
		Condition:    strings.ToLower(strings.TrimSpace(current.Condition.Text)),
		TemperatureC: *current.Temperature,
		ApparentC:    *current.Apparent,
		WindKph:      *current.Wind,
		Humidity:     *current.Humidity,
	}), nil
}

// statusOf returns the HTTP status a WeatherAPI error carries, or 0 when the
// failure was a transport error or decode failure rather than an upstream status.
func statusOf(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.status
	}
	return 0
}
