package github_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/github"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

var repo = config.Repo{Owner: "Bibzor", Name: "demo-api"}

const installToken = "ghs_SECRETinstallationTOKEN"

// fakeAPI serves the App endpoints and lets each test add its own.
type fakeAPI struct {
	t           *testing.T
	mux         *http.ServeMux
	srv         *httptest.Server
	tokenCalls  atomic.Int32
	expiresAt   time.Time
	installCall atomic.Int32
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, mux: http.NewServeMux(), expiresAt: time.Date(2026, 10, 28, 13, 0, 0, 0, time.UTC)}
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/installation", func(w http.ResponseWriter, r *http.Request) {
		f.installCall.Add(1)
		f.checkJWT(r)
		_, _ = w.Write([]byte(`{"id": 77}`))
	})
	f.mux.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		f.checkJWT(r)
		_, _ = fmt.Fprintf(w, `{"token": %q, "expires_at": %q}`, installToken, f.expiresAt.Format(time.RFC3339))
	})
	f.srv = httptest.NewServer(f.mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client(now func() time.Time) *github.Client {
	return github.New(123, key(f.t), github.WithBaseURL(f.srv.URL), github.WithClock(now))
}

// checkJWT verifies the App JWT: RS256 signature with the App key, issuer
// and a lifetime GitHub accepts.
func (f *fakeAPI) checkJWT(r *http.Request) {
	t := f.t
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		t.Errorf("not a JWT: %q", tok)
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Error(err)
		return
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("JWT signature: %v", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		IAT, EXP int64
		ISS      string
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Error(err)
	}
	if c.ISS != "123" || c.EXP-c.IAT > 600 || c.EXP <= c.IAT {
		t.Errorf("bad claims %+v", c)
	}
}

func (f *fakeAPI) requireToken(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+installToken {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func TestTokenCachedAndRefreshed(t *testing.T) {
	f := newFakeAPI(t)
	now := f.expiresAt.Add(-time.Hour)
	c := f.client(func() time.Time { return now })
	ctx := context.Background()

	for range 3 {
		tok, err := c.Token(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if tok.Reveal() != installToken {
			t.Fatalf("token = %q", tok.Reveal())
		}
	}
	if got := f.tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1 (cached)", got)
	}

	now = f.expiresAt.Add(-6 * time.Minute)
	if _, err := c.Token(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if got := f.tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1 (6 min left)", got)
	}

	now = f.expiresAt.Add(-4 * time.Minute)
	if _, err := c.Token(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if got := f.tokenCalls.Load(); got != 2 {
		t.Fatalf("token calls = %d, want 2 (refreshed before expiry)", got)
	}
	if got := f.installCall.Load(); got != 1 {
		t.Fatalf("installation lookups = %d, want 1", got)
	}
}

func TestBotLogin(t *testing.T) {
	f := newFakeAPI(t)
	f.mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		f.checkJWT(r)
		_, _ = w.Write([]byte(`{"slug": "foreman-bot"}`))
	})
	got, err := f.client(time.Now).BotLogin(context.Background())
	if err != nil || got != "foreman-bot[bot]" {
		t.Fatalf("BotLogin = %q, %v", got, err)
	}
}

func TestPaginationAndForeignLink(t *testing.T) {
	f := newFakeAPI(t)
	var created []string
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/labels", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireToken(w, r) {
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"name":"go"}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/Bibzor/demo-api/labels?per_page=100&page=2>; rel="next"`, f.srv.URL))
		_, _ = w.Write([]byte(`[{"name":"Foreman"}]`))
	})
	f.mux.HandleFunc("POST /repos/Bibzor/demo-api/labels", func(w http.ResponseWriter, r *http.Request) {
		var l github.Label
		_ = json.NewDecoder(r.Body).Decode(&l)
		created = append(created, l.Name)
		w.WriteHeader(http.StatusCreated)
	})
	c := f.client(func() time.Time { return f.expiresAt.Add(-time.Hour) })
	err := c.EnsureLabels(context.Background(), repo, []github.Label{{Name: "foreman"}, {Name: "go"}, {Name: "no-go"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(created, ",") != "no-go" {
		t.Fatalf("created %v, want only no-go", created)
	}

	// A Link header pointing at another host must not receive the token.
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("token sent to a foreign host: %s", r.URL)
	}))
	defer other.Close()
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/issues/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, other.URL))
		_, _ = w.Write([]byte(`[]`))
	})
	_, err = c.Comments(context.Background(), repo, 1)
	if err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Fatalf("err = %v", err)
	}
}

func TestIssuesAndEvents(t *testing.T) {
	f := newFakeAPI(t)
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labels") != "foreman" || r.URL.Query().Get("state") != "open" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"number": 1, "title": "t", "body": "b", "state": "open", "user": {"login": "alice"}, "labels": [{"name": "foreman"}]},
			{"number": 2, "title": "pr", "state": "open", "user": {"login": "bob"}, "pull_request": {}}
		]`))
	})
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/issues/1/timeline", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id": 5, "event": "labeled", "actor": {"login": "Bibzor"}, "label": {"name": "foreman"}, "created_at": "2026-10-28T10:00:00Z"},
			{"event": "commented", "actor": {"login": "x"}},
			{"id": 6, "event": "unlabeled", "actor": {"login": "x"}, "label": {"name": "foreman"}}
		]`))
	})
	f.mux.HandleFunc("DELETE /repos/Bibzor/demo-api/issues/1/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "foreman:awaiting-go" {
			t.Errorf("label = %q", r.PathValue("name"))
		}
		w.WriteHeader(http.StatusNotFound)
	})
	c := f.client(func() time.Time { return f.expiresAt.Add(-time.Hour) })
	ctx := context.Background()

	issues, err := c.OpenIssuesWithLabel(ctx, repo, "foreman")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Author != "alice" || !issues[0].Open || !issues[0].HasLabel("Foreman") {
		t.Fatalf("issues = %+v", issues)
	}
	events, err := c.LabelEvents(ctx, repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Actor != "Bibzor" || events[0].Label != "foreman" || events[0].ID != 5 {
		t.Fatalf("events = %+v", events)
	}
	if err := c.RemoveLabel(ctx, repo, 1, "foreman:awaiting-go"); err != nil {
		t.Fatalf("RemoveLabel of an absent label: %v", err)
	}
}

func TestCredentialsNeverPrinted(t *testing.T) {
	f := newFakeAPI(t)
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Resource not accessible by integration"}`))
	})
	c := f.client(func() time.Time { return f.expiresAt.Add(-time.Hour) })
	_, err := c.DefaultBranch(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	tok, err2 := c.Token(context.Background(), repo)
	if err2 != nil {
		t.Fatal(err2)
	}
	var logs strings.Builder
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "token", tok, "err", err)
	for _, s := range []string{err.Error(), fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok), logs.String()} {
		if strings.Contains(s, installToken) {
			t.Fatalf("token printed: %s", s)
		}
	}
}

func TestParseKey(t *testing.T) {
	k := key(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, b := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		if _, err := github.ParseKey(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	garbage := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("MIIsecretbytes")})
	_, err := github.ParseKey(garbage)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestPullRequests(t *testing.T) {
	f := newFakeAPI(t)
	f.mux.HandleFunc("POST /repos/Bibzor/demo-api/pulls", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireToken(w, r) {
			return
		}
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in["head"] != "foreman/7" || in["base"] != "main" || in["title"] != "Add a version endpoint" || in["body"] != "Refs #7" {
			t.Errorf("create = %v", in)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 12}`))
	})
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/pulls", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("head") != "Bibzor:foreman/7" || q.Get("state") != "all" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"number": 12, "state": "closed", "merged_at": "2026-10-28T10:00:00Z", "body": "x", "head": {"ref": "foreman/7"}}]`))
	})
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/pulls/12", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"number": 12, "state": "open", "merged": false, "body": null, "commits": 3, "head": {"ref": "foreman/7"}}`))
	})
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/pulls/12/reviews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id": 1, "user": {"login": "Bibzor"}, "state": "CHANGES_REQUESTED", "body": "fix it", "submitted_at": "2026-10-28T10:00:00Z"},
			{"id": 2, "user": null, "state": "APPROVED"}
		]`))
	})
	f.mux.HandleFunc("GET /repos/Bibzor/demo-api/pulls/12/reviews/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"path": "main.go", "line": 4, "body": "rename"}, {"path": "old.go", "line": null, "body": "outdated"}]`))
	})
	c := f.client(func() time.Time { return f.expiresAt.Add(-time.Hour) })
	ctx := context.Background()

	if n, err := c.CreatePullRequest(ctx, repo, "foreman/7", "main", "Add a version endpoint", "Refs #7"); err != nil || n != 12 {
		t.Fatalf("CreatePullRequest = %d %v", n, err)
	}
	prs, err := c.PullRequestsByHead(ctx, repo, "foreman/7")
	if err != nil || len(prs) != 1 || prs[0].Open || !prs[0].Merged || prs[0].Head != "foreman/7" {
		t.Fatalf("PullRequestsByHead = %+v %v", prs, err)
	}
	pr, err := c.PullRequest(ctx, repo, 12)
	if err != nil || !pr.Open || pr.Merged || pr.Commits != 3 || pr.Body != "" {
		t.Fatalf("PullRequest = %+v %v", pr, err)
	}
	reviews, err := c.Reviews(ctx, repo, 12)
	if err != nil || len(reviews) != 1 || reviews[0].State != "CHANGES_REQUESTED" || reviews[0].Author != "Bibzor" {
		t.Fatalf("Reviews = %+v %v", reviews, err)
	}
	comments, err := c.ReviewComments(ctx, repo, 12, 1)
	if err != nil || len(comments) != 2 || comments[0].Line != 4 || comments[1].Path != "old.go" {
		t.Fatalf("ReviewComments = %+v %v", comments, err)
	}
}
