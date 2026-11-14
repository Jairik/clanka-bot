package weather

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

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
		// Transport errors can include the query-string key; never return them verbatim.
		return Out{}, fmt.Errorf("weather: WeatherAPI request failed")
	}
	current := body.Current
	if strings.TrimSpace(body.Location.Name) == "" || strings.TrimSpace(current.Condition.Text) == "" ||
		current.Temperature == nil || current.Apparent == nil || current.Wind == nil || current.Humidity == nil {
		return Out{}, fmt.Errorf("weather: WeatherAPI returned incomplete current conditions")
	}
	if *current.Wind < 0 || *current.Humidity < 0 || *current.Humidity > 100 {
		return Out{}, fmt.Errorf("weather: WeatherAPI returned invalid current conditions")
	}
	return Out{
		City:         strings.TrimSpace(body.Location.Name),
		Condition:    strings.ToLower(strings.TrimSpace(current.Condition.Text)),
		TemperatureC: *current.Temperature,
		ApparentC:    *current.Apparent,
		WindKph:      *current.Wind,
		Humidity:     *current.Humidity,
	}, nil
}
