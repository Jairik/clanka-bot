package weather

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// fallbackForecast resolves the city independently when either Open-Meteo endpoint fails.
func fallbackForecast(ctx context.Context, deps Deps, city string) (Out, error) {
	var body struct {
		Places []struct {
			Names []struct {
				Value string `json:"value"`
			} `json:"areaName"`
		} `json:"nearest_area"`
		Current []struct {
			Temperature *float64 `json:"temp_C,string"`
			Apparent    *float64 `json:"FeelsLikeC,string"`
			Humidity    *int     `json:"humidity,string"`
			Wind        *float64 `json:"windspeedKmph,string"`
			Description []struct {
				Value string `json:"value"`
			} `json:"weatherDesc"`
		} `json:"current_condition"`
	}
	// An explicit city prevents wttr.in from falling back to the caller's IP location.
	target := strings.TrimRight(deps.FallbackURL, "/") + "/" + url.PathEscape(city) + "?format=j1&lang=en"
	if err := getJSON(ctx, deps.HTTP, target, &body); err != nil {
		return Out{}, err
	}
	if len(body.Current) == 0 || len(body.Places) == 0 || len(body.Places[0].Names) == 0 || strings.TrimSpace(body.Places[0].Names[0].Value) == "" {
		return Out{}, fmt.Errorf("weather: fallback has no current conditions or place")
	}
	current := body.Current[0]
	// Missing fields must fail instead of becoming a plausible zero-degree answer.
	if current.Temperature == nil || current.Apparent == nil || current.Humidity == nil || current.Wind == nil ||
		len(current.Description) == 0 || strings.TrimSpace(current.Description[0].Value) == "" {
		return Out{}, fmt.Errorf("weather: fallback has incomplete current conditions")
	}
	for _, value := range []float64{*current.Temperature, *current.Apparent, *current.Wind} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return Out{}, fmt.Errorf("weather: fallback has non-finite current conditions")
		}
	}
	if *current.Humidity < 0 || *current.Humidity > 100 || *current.Wind < 0 {
		return Out{}, fmt.Errorf("weather: fallback has invalid current conditions")
	}
	return Out{
		City:         strings.TrimSpace(body.Places[0].Names[0].Value),
		TemperatureC: *current.Temperature, ApparentC: *current.Apparent,
		Humidity: *current.Humidity, WindKph: *current.Wind,
		Condition: strings.ToLower(strings.TrimSpace(current.Description[0].Value)),
	}, nil
}
