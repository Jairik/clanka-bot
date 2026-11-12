package dashboard_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
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

var passwordInput = regexp.MustCompile(`<input[^>]*type="password"[^>]*>`)

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

func (f *secretsFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, target, owner))
	return rec
}

func (f *secretsFixture) form(method, target string, values url.Values) *http.Request {
	f.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
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

func (f *secretsFixture) postHTMX(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := f.form(http.MethodPost, target, values)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// secretArticle returns one rendered secret card so assertions cannot pass on
// text belonging to a different secret.
func secretArticle(t *testing.T, body, name string) string {
	t.Helper()
	marker := `id="secret-` + name + `"`
	index := strings.Index(body, marker)
	if index < 0 {
		t.Fatalf("page has no card for %s", name)
	}
	start := strings.LastIndex(body[:index], "<article")
	end := strings.Index(body[index:], "</article>")
	if start < 0 || end < 0 {
		t.Fatalf("card for %s is not a complete article", name)
	}
	return body[start : index+end+len("</article>")]
}

func TestSecretsPageListsEveryDeclaredSecretDynamically(t *testing.T) {
	f := newSecretsFixture(t)
	rec := f.get("/secrets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Secrets", "Write-only", "OPENROUTER_API_KEY", "SPOTIFY_REFRESH_TOKEN", "NTFY_TOKEN", "GITHUB_TOKEN"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(body, `href="/secrets"`) || !strings.Contains(body, `aria-current="page"`) {
		t.Error("nav does not mark the Secrets page current")
	}
	inputs := passwordInput.FindAllString(body, -1)
	if len(inputs) != 4 {
		t.Fatalf("password inputs = %d, want one per secret", len(inputs))
	}
	for _, tag := range inputs {
		if !strings.Contains(tag, `autocomplete="new-password"`) {
			t.Errorf("password input lacks autocomplete=new-password: %s", tag)
		}
		if strings.Contains(tag, "value=") {
			t.Errorf("password input is prefilled: %s", tag)
		}
	}
	for _, name := range []string{secretOpenRouter, secretSpotify, secretNtfy, secretGitHub} {
		article := secretArticle(t, body, name)
		if !strings.Contains(article, "not set") || !strings.Contains(article, "none") {
			t.Errorf("unset card for %s does not say not set/none:\n%s", name, article)
		}
		if strings.Contains(article, "/clear") {
			t.Errorf("unset card for %s offers a clear action", name)
		}
	}
}

func TestSecretsPageRendersSetMetadataWithoutTheValue(t *testing.T) {
	t.Setenv(secretOpenRouter, dummyEnvValue)
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretSpotify, dummyStoredValue); err != nil {
		t.Fatalf("Set: %v", err)
	}
	rec := f.get("/secrets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	env := secretArticle(t, body, secretOpenRouter)
	for _, want := range []string{"set", ".env", "uvwx"} {
		if !strings.Contains(env, want) {
			t.Errorf("env card lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "updated") {
		t.Errorf("env card claims an update time:\n%s", env)
	}

	stored := secretArticle(t, body, secretSpotify)
	for _, want := range []string{"set", "stored", "ijkl", "updated", "UTC"} {
		if !strings.Contains(stored, want) {
			t.Errorf("stored card lacks %q:\n%s", want, stored)
		}
	}
	if !strings.Contains(stored, "Clear stored value") {
		t.Errorf("stored card has no clear action:\n%s", stored)
	}

	for _, value := range []string{dummyEnvValue, dummyStoredValue} {
		if strings.Contains(body, value) {
			t.Errorf("page rendered the full secret value %q", value)
		}
	}
}

func TestSecretsReplaceWritesAndNeverExposesTheValue(t *testing.T) {
	t.Setenv(secretOpenRouter, dummyEnvValue)
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretOpenRouter, dummyStoredValue); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	rec := f.post("/secrets/"+secretOpenRouter, url.Values{"value": {dummyNewValue}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/secrets?flash=secret-saved" {
		t.Errorf("Location = %q", got)
	}
	// The replacement is the server-internal value the Agent provider reads.
	value, ok := f.secrets.Get(secretOpenRouter)
	if !ok || value != dummyNewValue {
		t.Fatalf("Get after replace = %q, %v; want the new stored value", value, ok)
	}

	page := f.get("/secrets?flash=secret-saved")
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d", page.Code)
	}
	body := page.Body.String()
	article := secretArticle(t, body, secretOpenRouter)
	for _, want := range []string{"set", "stored", "rqpo", "updated"} {
		if !strings.Contains(article, want) {
			t.Errorf("card lacks %q:\n%s", want, article)
		}
	}
	if !strings.Contains(body, "Stored. The new value applies on the next request.") {
		t.Error("page lacks the saved flash message")
	}
	// Neither the replaced value nor the new one may appear anywhere.
	for _, value := range []string{dummyStoredValue, dummyEnvValue, dummyNewValue} {
		if strings.Contains(body, value) {
			t.Errorf("page rendered the full secret value %q", value)
		}
		if strings.Contains(f.logs.String(), value) {
			t.Errorf("logs contain the full secret value %q", value)
		}
	}
	for _, tag := range passwordInput.FindAllString(article, -1) {
		if strings.Contains(tag, "value=") {
			t.Errorf("replaced secret input is prefilled: %s", tag)
		}
	}
}

func TestSecretsReplaceWorksAsAnHTMXFragment(t *testing.T) {
	f := newSecretsFixture(t)
	rec := f.postHTMX("/secrets/"+secretOpenRouter, url.Values{"value": {dummyNewValue}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("htmx response is a full page, want a fragment")
	}
	if !strings.Contains(body, `id="secrets-content"`) {
		t.Error("fragment lacks the swap target id")
	}
	if !strings.Contains(body, "stored") || !strings.Contains(body, "rqpo") {
		t.Errorf("fragment lacks the new metadata:\n%s", body)
	}
	if strings.Contains(body, dummyNewValue) {
		t.Error("fragment rendered the full secret value")
	}
}

func TestSecretsClearFallsBackToEnvAndSaysSo(t *testing.T) {
	t.Setenv(secretOpenRouter, dummyEnvValue)
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretOpenRouter, dummyStoredValue); err != nil {
		t.Fatalf("Set: %v", err)
	}

	rec := f.post("/secrets/"+secretOpenRouter+"/clear", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/secrets?flash=secret-cleared-env" {
		t.Errorf("Location = %q", got)
	}
	// Clearing exposes the .env value immediately: no restart, no second cache.
	value, ok := f.secrets.Get(secretOpenRouter)
	if !ok || value != dummyEnvValue {
		t.Fatalf("Get after clear = %q, %v; want the .env fallback", value, ok)
	}

	page := f.get("/secrets?flash=secret-cleared-env")
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d", page.Code)
	}
	body := page.Body.String()
	article := secretArticle(t, body, secretOpenRouter)
	for _, want := range []string{"set", ".env", "uvwx"} {
		if !strings.Contains(article, want) {
			t.Errorf("card lacks %q:\n%s", want, article)
		}
	}
	for _, unwanted := range []string{"stored", "updated", "/clear"} {
		if strings.Contains(article, unwanted) {
			t.Errorf("cleared card still shows %q:\n%s", unwanted, article)
		}
	}
	if !strings.Contains(body, "Stored value cleared. The .env value applies again.") {
		t.Error("page does not say the .env value applies again")
	}
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
	rec := f.post("/secrets/"+secretSpotify+"/clear", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/secrets?flash=secret-cleared-none" {
		t.Errorf("Location = %q", got)
	}
	if _, ok := f.secrets.Get(secretSpotify); ok {
		t.Fatal("secret still resolves after clearing without an env value")
	}
	body := f.get("/secrets?flash=secret-cleared-none").Body.String()
	article := secretArticle(t, body, secretSpotify)
	if !strings.Contains(article, "not set") || !strings.Contains(article, "none") {
		t.Errorf("card does not say not set/none:\n%s", article)
	}
	if strings.Contains(article, "/clear") {
		t.Errorf("unset card still offers a clear action:\n%s", article)
	}
	if !strings.Contains(body, "Stored value cleared. The secret is now unset.") {
		t.Error("page does not say the secret is now unset")
	}
}

func TestSecretsRejectsUnknownEmptyAndMalformedFormsWithoutEchoingTheValue(t *testing.T) {
	f := newSecretsFixture(t)

	unknown := f.post("/secrets/NOT_DECLARED", url.Values{"value": {dummyNewValue}})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404", unknown.Code)
	}
	if !strings.Contains(unknown.Body.String(), "Unknown secret.") {
		t.Errorf("unknown body = %s", unknown.Body.String())
	}

	empty := f.post("/secrets/"+secretOpenRouter, url.Values{"value": {""}})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty status = %d, want 400", empty.Code)
	}
	if !strings.Contains(empty.Body.String(), "Enter a value.") {
		t.Errorf("empty body = %s", empty.Body.String())
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
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Could not read the form.") {
		t.Errorf("malformed body = %s", rec.Body.String())
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

func TestSecretsErrorsWorkAsHTMXFragments(t *testing.T) {
	f := newSecretsFixture(t)
	rec := f.postHTMX("/secrets/"+secretOpenRouter, url.Values{"value": {""}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want a swap-friendly 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("htmx error is a full page, want a fragment")
	}
	if !strings.Contains(body, `id="secrets-content"`) || !strings.Contains(body, `role="alert"`) {
		t.Errorf("fragment lacks the swap target or alert role:\n%s", body)
	}
	if !strings.Contains(body, "Enter a value.") {
		t.Errorf("fragment lacks the error message:\n%s", body)
	}

	unknown := f.postHTMX("/secrets/NOT_DECLARED", url.Values{"value": {dummyNewValue}})
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown htmx status = %d", unknown.Code)
	}
	if strings.Contains(unknown.Body.String(), dummyNewValue) {
		t.Error("unknown htmx error echoed the submitted value")
	}
	if !strings.Contains(unknown.Body.String(), "Unknown secret.") {
		t.Errorf("unknown htmx body = %s", unknown.Body.String())
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
	if !strings.Contains(set.Body.String(), "The secret could not be stored.") {
		t.Errorf("set body = %s", set.Body.String())
	}
	clear := f.post("/secrets/"+secretOpenRouter+"/clear", nil)
	if clear.Code != http.StatusInternalServerError {
		t.Fatalf("clear status = %d, want 500", clear.Code)
	}
	if !strings.Contains(clear.Body.String(), "The secret could not be cleared.") {
		t.Errorf("clear body = %s", clear.Body.String())
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

	rec := serve(f.handler, request(http.MethodGet, "/secrets/"+secretOpenRouter, owner))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET on the write route = %d, want 404", rec.Code)
	}
}

func TestSecretsPageWithoutTheServiceIsReadOnly(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/secrets", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Secrets are not configured") {
		t.Errorf("page does not explain the missing service:\n%s", rec.Body.String())
	}
	req := request(http.MethodPost, "/secrets/"+secretOpenRouter, owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = serve(handler, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("write without the service = %d, want 404", rec.Code)
	}
}

func TestSecretsPageHidesShortSuffixes(t *testing.T) {
	const short = "short-49"
	f := newSecretsFixture(t)
	if _, err := f.secrets.Set(f.ctx, secretNtfy, short); err != nil {
		t.Fatalf("Set: %v", err)
	}
	body := f.get("/secrets").Body.String()
	article := secretArticle(t, body, secretNtfy)
	if !strings.Contains(article, "last 4 hidden for a short value") {
		t.Errorf("short value card does not explain the hidden suffix:\n%s", article)
	}
	if strings.Contains(article, short) || strings.Contains(article, "t-49") {
		t.Errorf("short value or its suffix leaked:\n%s", article)
	}
}

// TestSecretsPageListsTheRealModuleDeclarations proves the page is fed by the
// production module list, not a hand-maintained test list: adding the GitHub
// Tool's GITHUB_TOKEN declaration is enough for its row to appear.
func TestSecretsPageListsTheRealModuleDeclarations(t *testing.T) {
	names := tools.NewSet().SecretNames()
	f := newSecretsFixture(t, names...)
	body := f.get("/secrets").Body.String()
	if !strings.Contains(body, github.SecretName) {
		t.Fatalf("the Secrets page does not show %s: %s", github.SecretName, body)
	}
	for _, name := range names {
		if !strings.Contains(body, name) {
			t.Errorf("the Secrets page does not show the declared secret %s", name)
		}
	}
}
