package monster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCloud replays the four hosts the app uses on one local server. It
// records the login count so a test can prove the token is cached.
type fakeCloud struct {
	mu         sync.Mutex
	logins     int
	devices    int
	authFails  int
	setBodies  []map[string]any
	properties map[string]map[string]any
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v4/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AuthenticationDetails struct {
				Email    string `json:"email"`
				Password string `json:"password"`
			} `json:"authenticationDetails"`
			DeviceDetails struct {
				DeviceID string `json:"deviceId"`
			} `json:"deviceDetails"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		f.mu.Lock()
		f.logins++
		fail := body.AuthenticationDetails.Password == "wrong"
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"errorCode":401,"reason":"auth.unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if body.DeviceDetails.DeviceID == "" {
			http.Error(w, "missing device id", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "copilot-token"})
	})
	mux.HandleFunc("POST /v2/partner/"+partnerID+"/acquire_ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer copilot-token" {
			http.Error(w, "bad bearer", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"partnerTicket": "partner-ticket"})
	})
	mux.HandleFunc("POST /api/v1/token_sign_in", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["token"] != "partner-ticket" || body["app_id"] != aylaAppID || body["app_secret"] != aylaAppSecret {
			http.Error(w, "bad sign in", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ayla-token", "expires_in": 86400})
	})
	mux.HandleFunc("GET /apiv1/devices.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "auth_token ayla-token" {
			http.Error(w, "bad token", 401)
			return
		}
		f.mu.Lock()
		f.devices++
		fail := f.authFails > 0
		if fail {
			f.authFails--
		}
		f.mu.Unlock()
		if fail {
			http.Error(w, "expired", 401)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"device": map[string]any{"dsn": "DSN1", "product_name": "Duo+ Monitor Light Bar", "oem_model": "MNW7-2027-ICW"}},
			{"device": map[string]any{"dsn": "DSN2", "oem_model": "MLB7-2027-RGB"}},
			{"device": map[string]any{"product_name": "no dsn"}},
		})
	})
	mux.HandleFunc("GET /apiv1/dsns/{dsn}/properties.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "auth_token ayla-token" {
			http.Error(w, "bad token", 401)
			return
		}
		props := f.properties[r.PathValue("dsn")]
		if props == nil {
			props = map[string]any{
				"power":        json.Number("1"),
				"mode":         "color",
				"color_bright": "35",
				"color_select": json.Number("6579300"),
				"st00":         `{"n":"Candle"}`,
			}
		}
		items := make([]map[string]any, 0, len(props))
		for name, value := range props {
			items = append(items, map[string]any{"property": map[string]any{"name": name, "value": value}})
		}
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("POST /apiv1/dsns/{dsn}/properties/{name}/datapoints.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "auth_token ayla-token" {
			http.Error(w, "bad token", 401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["_dsn"] = r.PathValue("dsn")
		body["_name"] = r.PathValue("name")
		f.mu.Lock()
		f.setBodies = append(f.setBodies, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"datapoint": body["datapoint"]})
	})
	return mux
}

func newTestClient(t *testing.T) (*Client, *fakeCloud) {
	t.Helper()
	fake := &fakeCloud{properties: map[string]map[string]any{}}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	return &Client{
		URLs:     URLs{Copilot: server.URL, Sphere: server.URL, AylaUser: server.URL, AylaAds: server.URL},
		HTTP:     server.Client(),
		Now:      time.Now,
		deviceID: "test-device",
	}, fake
}

// TestDevicesFollowsTheSignInChain proves the three-step login, the device
// envelope, and that a second call reuses the Ayla token.
func TestDevicesFollowsTheSignInChain(t *testing.T) {
	c, fake := newTestClient(t)
	devices, err := c.Devices(context.Background(), "owner@example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	if devices[0].Name != "Duo+ Monitor Light Bar" || devices[0].Model != "MNW7-2027-ICW" {
		t.Fatalf("first = %+v", devices[0])
	}
	if devices[1].Name != "MLB7-2027-RGB" || devices[1].Model != "MLB7-2027-RGB" {
		t.Fatalf("fallback name = %+v", devices[1])
	}
	if _, err := c.Devices(context.Background(), "owner@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	if fake.logins != 1 {
		t.Fatalf("logins = %d, want one cached token", fake.logins)
	}
}

func TestPropertiesKeepsTheDeviceValues(t *testing.T) {
	c, _ := newTestClient(t)
	props, err := c.Properties(context.Background(), "owner@example.com", "secret", "DSN1")
	if err != nil {
		t.Fatal(err)
	}
	if intProp(props, "power") != 1 || stringProp(props, "mode") != "color" {
		t.Fatalf("props = %+v", props)
	}
	if intProp(props, "color_select") != 6579300 {
		t.Fatalf("color_select = %v", props["color_select"])
	}
}

func TestSetWritesOneDatapoint(t *testing.T) {
	c, fake := newTestClient(t)
	if err := c.Set(context.Background(), "owner@example.com", "secret", "DSN1", "power", 1); err != nil {
		t.Fatal(err)
	}
	if len(fake.setBodies) != 1 {
		t.Fatalf("bodies = %+v", fake.setBodies)
	}
	body := fake.setBodies[0]
	if body["_name"] != "power" || body["_dsn"] != "DSN1" {
		t.Fatalf("body = %+v", body)
	}
	dp, _ := body["datapoint"].(map[string]any)
	if dp["value"] != float64(1) {
		t.Fatalf("value = %#v", dp["value"])
	}
}

// TestExpiredTokenSignsInAgain proves one 401 is retried with a fresh login
// instead of surfacing as a page error.
func TestExpiredTokenSignsInAgain(t *testing.T) {
	c, fake := newTestClient(t)
	fake.authFails = 1
	devices, err := c.Devices(context.Background(), "owner@example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	if fake.logins != 2 {
		t.Fatalf("logins = %d, want a second login after the 401", fake.logins)
	}
}

func TestBadPasswordIsACredentialError(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.Devices(context.Background(), "owner@example.com", "wrong")
	if !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error leaked the password: %v", err)
	}
}

func TestFriendlyNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrCredentials, "Monster rejected the email or password"},
		{ErrNotFound, "does not know that light"},
		{context.DeadlineExceeded, "did not answer in time"},
		{&StatusError{Code: 503}, "HTTP 503"},
		{ErrUpstream, "did not answer"},
	} {
		if got := Friendly(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("Friendly(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
