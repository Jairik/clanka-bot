package monster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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
	// loginDelay makes the login endpoint slow, so a test can expire the
	// caller's deadline while the sign-in is still running.
	loginDelay time.Duration
	// loginStatus, when non-zero, makes the login endpoint answer with that
	// HTTP status instead of signing in.
	loginStatus int
	attempts    []loginAttempt
}

// loginAttempt is one body the fake login endpoint received, so a test can
// prove what reached the cloud.
type loginAttempt struct {
	email    string
	password string
}

func (f *fakeCloud) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeCloud) loginAttempts() []loginAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]loginAttempt(nil), f.attempts...)
}

func (f *fakeCloud) setLoginDelay(delay time.Duration) {
	f.mu.Lock()
	f.loginDelay = delay
	f.mu.Unlock()
}

func (f *fakeCloud) setLoginStatus(status int) {
	f.mu.Lock()
	f.loginStatus = status
	f.mu.Unlock()
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
		f.attempts = append(f.attempts, loginAttempt{
			email:    body.AuthenticationDetails.Email,
			password: body.AuthenticationDetails.Password,
		})
		fail := strings.HasPrefix(body.AuthenticationDetails.Password, "wrong")
		delay := f.loginDelay
		status := f.loginStatus
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			http.Error(w, "upstream is down", status)
			return
		}
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

// newClientFor points a client at one fake server for all four hosts. A nil
// clock uses the real one.
func newClientFor(server *httptest.Server, now func() time.Time) *Client {
	if now == nil {
		now = time.Now
	}
	return &Client{
		URLs:     URLs{Copilot: server.URL, Sphere: server.URL, AylaUser: server.URL, AylaAds: server.URL},
		HTTP:     server.Client(),
		Now:      now,
		deviceID: "test-device",
	}
}

func newTestClient(t *testing.T) (*Client, *fakeCloud) {
	t.Helper()
	fake := &fakeCloud{properties: map[string]map[string]any{}}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	return newClientFor(server, time.Now), fake
}

// newTLSTestClient points a client at a fake HTTPS cloud, matching the
// production scheme, and lets a test supply the clock the backoff reads.
func newTLSTestClient(t *testing.T, now func() time.Time) (*Client, *fakeCloud) {
	t.Helper()
	fake := &fakeCloud{properties: map[string]map[string]any{}}
	server := httptest.NewTLSServer(fake.handler())
	t.Cleanup(server.Close)
	return newClientFor(server, now), fake
}

// fakeClock is a hand-wound clock for the backoff tests. The login goroutine
// and the test read it at once, so it carries its own lock.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.Unix(1_700_000_000, 0)}
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.at = f.at.Add(d)
	f.mu.Unlock()
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
	if got := fake.loginCount(); got != 1 {
		t.Fatalf("logins = %d, want one cached token", got)
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
	if got := fake.loginCount(); got != 2 {
		t.Fatalf("logins = %d, want a second login after the 401", got)
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

func TestControlPathsEscapeTheDSN(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v4/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "copilot"})
		case "/v2/partner/" + partnerID + "/acquire_ticket":
			_ = json.NewEncoder(w).Encode(map[string]string{"partnerTicket": "ticket"})
		case "/api/v1/token_sign_in":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ayla", "expires_in": 3600})
		default:
			got = r.RequestURI
			_ = json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer server.Close()
	c := &Client{
		URLs:     URLs{Copilot: server.URL, Sphere: server.URL, AylaUser: server.URL, AylaAds: server.URL},
		HTTP:     server.Client(),
		Now:      time.Now,
		deviceID: "test-device",
	}
	if err := c.Set(context.Background(), "owner@example.com", "secret", "A/B %", "power", 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/apiv1/dsns/A%2FB%20%25/properties/power/datapoints.json") {
		t.Fatalf("request path = %q", got)
	}
}

func TestOversizedResponseIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v4/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "copilot"})
		case "/v2/partner/" + partnerID + "/acquire_ticket":
			_ = json.NewEncoder(w).Encode(map[string]string{"partnerTicket": "ticket"})
		case "/api/v1/token_sign_in":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ayla", "expires_in": 3600})
		default:
			_, _ = w.Write(bytes.Repeat([]byte("a"), maxBody+1))
		}
	}))
	defer server.Close()
	c := &Client{
		URLs:     URLs{Copilot: server.URL, Sphere: server.URL, AylaUser: server.URL, AylaAds: server.URL},
		HTTP:     server.Client(),
		Now:      time.Now,
		deviceID: "test-device",
	}
	if _, err := c.Properties(context.Background(), "owner@example.com", "secret", "DSN1"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
}

// TestFailedLoginBacksOff proves a rejected password is not retried on every
// call: three reads inside the backoff window cause one login, a read after the
// window causes a second, and replacing the password retries at once.
func TestFailedLoginBacksOff(t *testing.T) {
	clock := newFakeClock()
	c, fake := newTLSTestClient(t, clock.now)

	for i := 0; i < 3; i++ {
		if _, err := c.Devices(context.Background(), "owner@example.com", "wrong-password"); !errors.Is(err, ErrCredentials) {
			t.Fatalf("call %d err = %v", i, err)
		}
		// A page load and a Health poll both land inside the same window.
		clock.advance(30 * time.Second)
	}
	if got := fake.loginCount(); got != 1 {
		t.Fatalf("logins = %d, want one login inside the backoff", got)
	}

	// Past the credential backoff the next call tries the cloud again.
	clock.advance(credentialBackoff)
	if _, err := c.Devices(context.Background(), "owner@example.com", "wrong-password"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("post-backoff err = %v", err)
	}
	if got := fake.loginCount(); got != 2 {
		t.Fatalf("logins = %d, want a retry after the backoff", got)
	}

	// A different password is a different credential: retry immediately
	// instead of serving the previous password's memo.
	if _, err := c.Devices(context.Background(), "owner@example.com", "wrong-other"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("changed-password err = %v", err)
	}
	if got := fake.loginCount(); got != 3 {
		t.Fatalf("logins = %d, want an immediate retry after the password change", got)
	}
	if attempts := fake.loginAttempts(); attempts[2].password != "wrong-other" {
		t.Fatalf("attempts = %+v", attempts)
	}
}

// TestTransientFailureBacksOffBriefly proves an upstream failure waits seconds,
// not minutes, before the next attempt.
func TestTransientFailureBacksOffBriefly(t *testing.T) {
	clock := newFakeClock()
	c, fake := newTLSTestClient(t, clock.now)
	fake.setLoginStatus(http.StatusServiceUnavailable)

	if _, err := c.Devices(context.Background(), "owner@example.com", "secret"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
	clock.advance(transientBackoff - time.Second)
	if _, err := c.Devices(context.Background(), "owner@example.com", "secret"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("inside-backoff err = %v", err)
	}
	if got := fake.loginCount(); got != 1 {
		t.Fatalf("logins = %d, want the transient failure memoised", got)
	}

	fake.setLoginStatus(0)
	clock.advance(2 * time.Second)
	if _, err := c.Devices(context.Background(), "owner@example.com", "secret"); err != nil {
		t.Fatalf("post-backoff err = %v", err)
	}
	if got := fake.loginCount(); got != 2 {
		t.Fatalf("logins = %d, want a retry after the transient backoff", got)
	}
}

// TestLoginOutlivesTheCallersDeadline proves a caller that gives up does not
// throw the sign-in away: Health sees its own deadline while the login finishes
// and caches the token for the next call.
func TestLoginOutlivesTheCallersDeadline(t *testing.T) {
	c, fake := newTLSTestClient(t, nil)
	fake.setLoginDelay(250 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := c.Devices(ctx, "owner@example.com", "secret"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's own deadline", err)
	}

	devices, err := c.Devices(context.Background(), "owner@example.com", "secret")
	if err != nil {
		t.Fatalf("second call err = %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	if got := fake.loginCount(); got != 1 {
		t.Fatalf("logins = %d, want the first sign-in's token reused", got)
	}
}

// TestPasswordIsSentUnchanged proves the module no longer trims a password: a
// value the app accepts with a leading or trailing space reaches the cloud.
func TestPasswordIsSentUnchanged(t *testing.T) {
	c, fake := newTLSTestClient(t, nil)
	if _, err := c.Devices(context.Background(), "owner@example.com", " hunter2-space "); err != nil {
		t.Fatal(err)
	}
	attempts := fake.loginAttempts()
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v", attempts)
	}
	if attempts[0].password != " hunter2-space " {
		t.Fatalf("password = %q, want the stored value unchanged", attempts[0].password)
	}
	if attempts[0].email != "owner@example.com" {
		t.Fatalf("email = %q", attempts[0].email)
	}
}

// TestSignInNeverLogsSecrets drives a rejected and a working sign-in with the
// standard logger captured, so a later log line that echoes the request or the
// token fails here instead of reaching the service log.
func TestSignInNeverLogsSecrets(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c, _ := newTLSTestClient(t, nil)
	const rejected = "wrong-hunter2-secret"
	if _, err := c.Devices(context.Background(), "owner@example.com", rejected); !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v", err)
	}
	const accepted = "hunter2-secret"
	if _, err := c.Devices(context.Background(), "owner@example.com", accepted); err != nil {
		t.Fatalf("err = %v", err)
	}

	output := logged.String()
	for _, secret := range []string{rejected, accepted, "owner@example.com", "ayla-token", "copilot-token", "partner-ticket"} {
		if strings.Contains(output, secret) {
			t.Fatalf("log line leaked %q: %q", secret, output)
		}
	}
}

// TestConcurrentCallsShareOneLogin proves the in-flight sign-in is shared, so a
// Dashboard page and a Health poll cannot run two logins at once.
func TestConcurrentCallsShareOneLogin(t *testing.T) {
	c, fake := newTLSTestClient(t, nil)
	fake.setLoginDelay(100 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Devices(context.Background(), "owner@example.com", "secret"); err != nil {
				t.Errorf("err = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := fake.loginCount(); got != 1 {
		t.Fatalf("logins = %d, want one shared sign-in", got)
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
