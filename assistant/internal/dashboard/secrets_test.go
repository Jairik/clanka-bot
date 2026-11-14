package dashboard_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/github"
)

// The declared names stand in for today's provider key and the future ntfy,
// GitHub and Spotify secrets. The page must discover them, not hardcode them.
const (
	secretOpenRouter = "OPENROUTER_API_KEY"
	secretSpotify    = "SPOTIFY_REFRESH_TOKEN"
	secretNtfy       = "NTFY_TOKEN"
	secretGitHub     = "GITHUB_TOKEN"
)

// Dummy values are long enough for last4 and unique so a page or log scan can
// prove they never leak.
const (
	dummyStoredValue = "dummy-stored-49-abcdefghijkl"
	dummyEnvValue    = "dummy-env-49-mnopqrstuvwx"
	dummyNewValue    = "dummy-new-49-zyxwvutsrqpo"
)

// lockedBuffer collects Dashboard logs from the test goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// secretsFixture wires the real store, secrets service and authenticated
// Dashboard, so the page exercises the same metadata and validation as the API.
type secretsFixture struct {
	t       *testing.T
	ctx     context.Context
	handler *dashboard.Handler
	secrets *secrets.Service
	store   *store.Store
	logs    *lockedBuffer
}

func newSecretsFixture(t *testing.T, names ...string) *secretsFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if len(names) == 0 {
		names = []string{secretOpenRouter, secretSpotify, secretNtfy, secretGitHub}
	}
	svc, err := secrets.New(ctx, st.DB(), names)
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	logs := &lockedBuffer{}
	handler := newHandler(t, dashboard.Options{
		Secrets: svc,
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	})
	return &secretsFixture{t: t, ctx: ctx, handler: handler, secrets: svc, store: st, logs: logs}
}

// secretsResp decodes the Secrets view (encoding/json default field names).
// There is deliberately no field that could carry a value: only metadata.
type secretsResp struct {
	Flash      string
	Error      string
	HasSecrets bool
	Secrets    []secretRow
}

type secretRow struct {
	Name, Anchor           string
	Set                    bool
	SetLabel               string
	Source, SourceLabel    string
	Last4, Updated         string
	UpdatedISO             string
	Stored                 bool
	SetAction, ClearAction string
}

func (v secretsResp) secret(t *testing.T, name string) secretRow {
	t.Helper()
	for _, row := range v.Secrets {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("view has no secret %s: %+v", name, v.Secrets)
	return secretRow{}
}

func (f *secretsFixture) get(target string) (secretsResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	var v secretsResp
	rec := getJSON(f.t, f.handler, target, &v)
	return v, rec
}

// view reads /api/ui/secrets, which must be a 200.
func (f *secretsFixture) view() (secretsResp, string) {
	f.t.Helper()
	v, rec := f.get("/api/ui/secrets")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET secrets = %d: %s", rec.Code, rec.Body.String())
	}
	return v, rec.Body.String()
}

// postView posts a write and decodes the answered view.
func (f *secretsFixture) postView(target string, values url.Values) (secretsResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.post(target, values)
	var v secretsResp
	decodeJSON(f.t, rec, &v)
	return v, rec
}

func (f *secretsFixture) form(method, target string, values url.Values) *http.Request {
	f.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func (f *secretsFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, f.form(http.MethodPost, target, values))
	return rec
}

func TestSecretsReadListsEveryDeclaredSecretDynamically(t *testing.T) {
	f := newSecretsFixture(t)
	v, rec := f.get("/api/ui/secrets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !v.HasSecrets {
		t.Error("HasSecrets = false with a service wired")
	}
	var names []string
	for _, row := range v.Secrets {
		names = append(names, row.Name)
	}
	want := []string{secretOpenRouter, secretSpotify, secretNtfy, secretGitHub}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("secrets = %v, want %v in declaration order", names, want)
	}
	for _, row := range v.Secrets {
		if row.Set || row.SetLabel != "not set" || row.SourceLabel != "none" || row.Source != "" && row.Source != "none" {
			t.Errorf("unset secret %s = %+v, want not set / none", row.Name, row)
		}
		if row.Stored {
			t.Errorf("unset secret %s claims a stored value", row.Name)
		}
		if row.SetAction != "/secrets/"+row.Name || row.ClearAction != "/secrets/"+row.Name+"/clear" {
			t.Errorf("actions for %s = %q / %q", row.Name, row.SetAction, row.ClearAction)
		}
	}
}

func TestSecretsReadRendersSetMetadataWithoutTheValue(t *testing.T) {
	t.Setenv(secretOpenRouter, dummyEnvValue)
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretSpotify, dummyStoredValue); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, body := f.view()

	env := v.secret(t, secretOpenRouter)
	if !env.Set || env.SetLabel != "set" || env.SourceLabel != ".env" || env.Last4 != "uvwx" || env.Stored {
		t.Errorf("env secret = %+v, want set from .env with last4 uvwx", env)
	}
	if env.Updated != "" || env.UpdatedISO != "" {
		t.Errorf("env secret claims an update time: %+v", env)
	}

	stored := v.secret(t, secretSpotify)
	if !stored.Set || stored.SourceLabel != "stored" || stored.Last4 != "ijkl" || !stored.Stored {
		t.Errorf("stored secret = %+v, want stored with last4 ijkl", stored)
	}
	if !strings.Contains(stored.Updated, "UTC") || stored.UpdatedISO == "" {
		t.Errorf("stored secret update time = %q / %q, want a UTC timestamp", stored.Updated, stored.UpdatedISO)
	}

	for _, value := range []string{dummyEnvValue, dummyStoredValue} {
		if strings.Contains(body, value) {
			t.Errorf("JSON carried the full secret value %q", value)
		}
	}
}

func TestSecretsReplaceWritesAndNeverExposesTheValue(t *testing.T) {
	t.Setenv(secretOpenRouter, dummyEnvValue)
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretOpenRouter, dummyStoredValue); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	saved, rec := f.postView("/secrets/"+secretOpenRouter, url.Values{"value": {dummyNewValue}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Errorf("Location = %q, want no redirect", rec.Header().Get("Location"))
	}
	if saved.Flash != "Stored. The new value applies on the next request." {
		t.Errorf("Flash = %q", saved.Flash)
	}
	// The replacement is the server-internal value the Agent provider reads.
	value, ok := f.secrets.Get(secretOpenRouter)
	if !ok || value != dummyNewValue {
		t.Fatalf("Get after replace = %q, %v; want the new stored value", value, ok)
	}

	// The write's own response already carries the refreshed metadata.
	row := saved.secret(t, secretOpenRouter)
	if !row.Set || !row.Stored || row.SourceLabel != "stored" || row.Last4 != "rqpo" || row.Updated == "" {
		t.Errorf("replaced secret = %+v, want stored with last4 rqpo and an update time", row)
	}
	// A plain read with the flash query shows the same notice.
	page, _ := f.get("/api/ui/secrets?flash=secret-saved")
	if page.Flash != saved.Flash {
		t.Errorf("?flash= read = %q, want %q", page.Flash, saved.Flash)
	}
	body := rec.Body.String()
	// Neither the replaced value nor the new one may appear anywhere.
	for _, value := range []string{dummyStoredValue, dummyEnvValue, dummyNewValue} {
		if strings.Contains(body, value) {
			t.Errorf("page rendered the full secret value %q", value)
		}
		if strings.Contains(f.logs.String(), value) {
			t.Errorf("logs contain the full secret value %q", value)
		}
	}
}

func TestSecretsClearFallsBackToEnvAndSaysSo(t *testing.T) {
	t.Setenv(secretOpenRouter, dummyEnvValue)
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretOpenRouter, dummyStoredValue); err != nil {
		t.Fatalf("Set: %v", err)
	}

	cleared, rec := f.postView("/secrets/"+secretOpenRouter+"/clear", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if cleared.Flash != "Stored value cleared. The .env value applies again." {
		t.Errorf("Flash = %q, want the .env-applies-again message", cleared.Flash)
	}
	// Clearing exposes the .env value immediately: no restart, no second cache.
	value, ok := f.secrets.Get(secretOpenRouter)
	if !ok || value != dummyEnvValue {
		t.Fatalf("Get after clear = %q, %v; want the .env fallback", value, ok)
	}

	row := cleared.secret(t, secretOpenRouter)
	if !row.Set || row.SourceLabel != ".env" || row.Last4 != "uvwx" || row.Stored || row.Updated != "" {
		t.Errorf("cleared secret = %+v, want set from .env, not stored, no update time", row)
	}
	body := rec.Body.String()
	for _, value := range []string{dummyEnvValue, dummyStoredValue} {
		if strings.Contains(body, value) {
			t.Errorf("page rendered the full secret value %q", value)
		}
		if strings.Contains(f.logs.String(), value) {
			t.Errorf("logs contain the full secret value %q", value)
		}
	}
}

func TestSecretsClearWithoutAnEnvValueLeavesTheSecretUnset(t *testing.T) {
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretSpotify, dummyStoredValue); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cleared, rec := f.postView("/secrets/"+secretSpotify+"/clear", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cleared.Flash != "Stored value cleared. The secret is now unset." {
		t.Errorf("Flash = %q, want the now-unset message", cleared.Flash)
	}
	if _, ok := f.secrets.Get(secretSpotify); ok {
		t.Fatal("secret still resolves after clearing without an env value")
	}
	row := cleared.secret(t, secretSpotify)
	if row.Set || row.SetLabel != "not set" || row.SourceLabel != "none" || row.Stored {
		t.Errorf("cleared secret = %+v, want not set / none", row)
	}
}

func decodeSecrets(t *testing.T, rec *httptest.ResponseRecorder) secretsResp {
	t.Helper()
	var v secretsResp
	decodeJSON(t, rec, &v)
	return v
}

func TestSecretsRejectsUnknownEmptyAndMalformedFormsWithoutEchoingTheValue(t *testing.T) {
	f := newSecretsFixture(t)

	unknown := f.post("/secrets/NOT_DECLARED", url.Values{"value": {dummyNewValue}})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404", unknown.Code)
	}
	if v := decodeSecrets(t, unknown); v.Error != "Unknown secret." {
		t.Errorf("unknown error = %q", v.Error)
	}

	empty := f.post("/secrets/"+secretOpenRouter, url.Values{"value": {""}})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty status = %d, want 400", empty.Code)
	}
	if v := decodeSecrets(t, empty); v.Error != "Enter a value." {
		t.Errorf("empty error = %q", v.Error)
	}

	// A whitespace-only value is empty after the service trims it; the page
	// must not repeat the submitted text.
	spaces := f.post("/secrets/"+secretOpenRouter, url.Values{"value": {"   "}})
	if spaces.Code != http.StatusBadRequest {
		t.Fatalf("whitespace status = %d, want 400", spaces.Code)
	}

	// A malformed form is rejected before the service sees anything.
	req := httptest.NewRequest(http.MethodPost, "/secrets/"+secretOpenRouter,
		strings.NewReader("value=%zz&other="+dummyNewValue))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", rec.Code)
	}
	if v := decodeSecrets(t, rec); v.Error != "Could not read the form." {
		t.Errorf("malformed error = %q", v.Error)
	}

	if _, ok := f.secrets.Get(secretOpenRouter); ok {
		t.Fatal("a rejected form changed the secret")
	}
	for _, response := range []*httptest.ResponseRecorder{unknown, empty, spaces, rec} {
		if strings.Contains(response.Body.String(), dummyNewValue) {
			t.Errorf("a rejected response echoed the submitted value: %s", response.Body.String())
		}
	}
	if strings.Contains(f.logs.String(), dummyNewValue) {
		t.Error("logs contain the submitted value")
	}
}

// TestSecretsFailuresAnswerTheRealStatusWithTheViewAndNoValue: an error is the
// metadata view with the real status and a fixed message, so the React app can
// show it without ever being handed the submitted value.
func TestSecretsFailuresAnswerTheRealStatusWithTheViewAndNoValue(t *testing.T) {
	f := newSecretsFixture(t)
	v, rec := f.postView("/secrets/"+secretOpenRouter, url.Values{"value": {""}})
	if rec.Code != http.StatusBadRequest || v.Error != "Enter a value." || v.Flash != "" {
		t.Fatalf("empty = %d %+v", rec.Code, v)
	}
	if !v.HasSecrets || len(v.Secrets) != 4 {
		t.Errorf("error view lost the metadata list: %+v", v)
	}

	v, rec = f.postView("/secrets/NOT_DECLARED", url.Values{"value": {dummyNewValue}})
	if rec.Code != http.StatusNotFound || v.Error != "Unknown secret." {
		t.Fatalf("unknown = %d %q", rec.Code, v.Error)
	}
	if strings.Contains(rec.Body.String(), dummyNewValue) {
		t.Error("unknown error echoed the submitted value")
	}
}

func TestSecretsStoreFailureIsReportedWithoutTheValue(t *testing.T) {
	f := newSecretsFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	set := f.post("/secrets/"+secretOpenRouter, url.Values{"value": {dummyNewValue}})
	if set.Code != http.StatusInternalServerError {
		t.Fatalf("set status = %d, want 500", set.Code)
	}
	if v := decodeSecrets(t, set); v.Error != "The secret could not be stored." {
		t.Errorf("set error = %q", v.Error)
	}
	clear := f.post("/secrets/"+secretOpenRouter+"/clear", nil)
	if clear.Code != http.StatusInternalServerError {
		t.Fatalf("clear status = %d, want 500", clear.Code)
	}
	if v := decodeSecrets(t, clear); v.Error != "The secret could not be cleared." {
		t.Errorf("clear error = %q", v.Error)
	}
	for _, response := range []*httptest.ResponseRecorder{set, clear} {
		if strings.Contains(response.Body.String(), dummyNewValue) {
			t.Error("a failed write echoed the submitted value")
		}
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "dashboard secrets") || !strings.Contains(logs, secretOpenRouter) {
		t.Errorf("logs do not name the failed operation:\n%s", logs)
	}
	if strings.Contains(logs, dummyNewValue) {
		t.Error("logs contain the submitted value")
	}
}

func TestSecretsRoutesRequireIdentityAndSameOrigin(t *testing.T) {
	f := newSecretsFixture(t)
	values := url.Values{"value": {dummyNewValue}}

	cases := []struct {
		name string
		req  *http.Request
	}{
		{
			name: "missing identity",
			req: func() *http.Request {
				req := f.form(http.MethodPost, "/secrets/"+secretOpenRouter, values)
				req.Header.Del("Tailscale-User-Login")
				return req
			}(),
		},
		{
			name: "wrong identity",
			req: func() *http.Request {
				req := f.form(http.MethodPost, "/secrets/"+secretOpenRouter, values)
				req.Header.Set("Tailscale-User-Login", "mallory@example.com")
				return req
			}(),
		},
		{
			name: "cross-site fetch metadata",
			req: func() *http.Request {
				req := f.form(http.MethodPost, "/secrets/"+secretOpenRouter, values)
				req.Header.Set("Sec-Fetch-Site", "cross-site")
				req.Header.Set("Origin", "https://evil.example")
				return req
			}(),
		},
		{
			name: "cross-origin clear",
			req: func() *http.Request {
				req := f.form(http.MethodPost, "/secrets/"+secretOpenRouter+"/clear", nil)
				req.Header.Set("Sec-Fetch-Site", "cross-site")
				req.Header.Set("Origin", "https://evil.example")
				return req
			}(),
		},
		{
			name: "missing origin",
			req: func() *http.Request {
				req := f.form(http.MethodPost, "/secrets/"+secretOpenRouter, values)
				req.Header.Del("Sec-Fetch-Site")
				return req
			}(),
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, tt.req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if _, ok := f.secrets.Get(secretOpenRouter); ok {
				t.Fatal("a rejected request changed the secret")
			}
		})
	}

	// A GET on a write path is only the app shell (every unknown page path
	// is a client route); it must not act on the secret.
	rec := serve(f.handler, request(http.MethodGet, "/secrets/"+secretOpenRouter, owner))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("GET on the write route = %d %q, want the app shell", rec.Code, rec.Header().Get("Content-Type"))
	}
	if _, ok := f.secrets.Get(secretOpenRouter); ok {
		t.Error("a GET changed the secret")
	}
}

func TestSecretsReadWithoutTheServiceIsReadOnly(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	var v secretsResp
	rec := getJSON(t, handler, "/api/ui/secrets", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if v.HasSecrets || len(v.Secrets) != 0 {
		t.Errorf("view without the service = %+v, want none", v)
	}
	req := request(http.MethodPost, "/secrets/"+secretOpenRouter, owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = serve(handler, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("write without the service = %d, want 404", rec.Code)
	}
}

func TestSecretsReadHidesShortSuffixes(t *testing.T) {
	const short = "short-49"
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretNtfy, short); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, body := f.view()
	row := v.secret(t, secretNtfy)
	if !row.Set || row.Last4 != "" {
		t.Errorf("short secret = %+v, want set with the last 4 hidden", row)
	}
	if strings.Contains(body, short) || strings.Contains(body, "t-49") {
		t.Errorf("short value or its suffix leaked:\n%s", body)
	}
}

// TestSecretsReadListsTheRealModuleDeclarations proves the view is fed by the
// production module list, not a hand-maintained test list: adding the GitHub
// Tool's GITHUB_TOKEN declaration is enough for its row to appear.
func TestSecretsReadListsTheRealModuleDeclarations(t *testing.T) {
	names := tools.NewSet().SecretNames()
	f := newSecretsFixture(t, names...)
	v, _ := f.view()
	v.secret(t, github.SecretName)
	for _, name := range names {
		v.secret(t, name)
	}
}
