// Package github is a small GitHub REST client authenticated as a GitHub
// App. It covers only the endpoints foreman needs.
//
// It uses net/http rather than an SDK: about a dozen endpoints and an
// RS256 JWT do not justify go-github and ghinstallation as dependencies.
//
// Credentials (the private key, JWTs, installation tokens) are held in
// memory only and never appear in errors or logs.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Bibzor/foreman/internal/config"
)

// DefaultBaseURL is the GitHub REST API.
const DefaultBaseURL = "https://api.github.com"

// tokenMargin is how long before expiry an installation token is replaced.
const tokenMargin = 5 * time.Minute

// Client calls the GitHub API as a GitHub App.
type Client struct {
	appID   int64
	key     *rsa.PrivateKey
	http    *http.Client
	baseURL *url.URL
	now     func() time.Time

	mu       sync.Mutex
	installs map[string]int64 // lowercase owner/name -> installation ID
	tokens   map[int64]token
	botLogin string
}

type token struct {
	value     Secret
	expiresAt time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another API root (tests).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if p, err := url.Parse(strings.TrimSuffix(u, "/")); err == nil {
			c.baseURL = p
		}
	}
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// New returns a client for the App appID signing with key.
func New(appID int64, key *rsa.PrivateKey, opts ...Option) *Client {
	base, _ := url.Parse(DefaultBaseURL) // constant, valid URL
	c := &Client{
		appID:    appID,
		key:      key,
		http:     &http.Client{Timeout: 30 * time.Second},
		baseURL:  base,
		now:      time.Now,
		installs: make(map[string]int64),
		tokens:   make(map[int64]token),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// APIError is a non-2xx response. It carries GitHub's message, never the
// request headers.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from GitHub.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// BotLogin returns the login the App comments as: "<slug>[bot]".
func (c *Client) BotLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	login := c.botLogin
	c.mu.Unlock()
	if login != "" {
		return login, nil
	}
	jwt, err := appJWT(c.key, c.appID, c.now())
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		return "", err
	}
	if app.Slug == "" {
		return "", errors.New("github: /app returned no slug")
	}
	c.mu.Lock()
	c.botLogin = app.Slug + "[bot]"
	c.mu.Unlock()
	return app.Slug + "[bot]", nil
}

// Token returns an installation token for repo, discovering the
// installation and refreshing the token when it is close to expiry.
func (c *Client) Token(ctx context.Context, repo config.Repo) (Secret, error) {
	id, err := c.installation(ctx, repo)
	if err != nil {
		return Secret{}, err
	}
	c.mu.Lock()
	t, ok := c.tokens[id]
	c.mu.Unlock()
	if ok && c.now().Add(tokenMargin).Before(t.expiresAt) {
		return t.value, nil
	}
	jwt, err := appJWT(c.key, c.appID, c.now())
	if err != nil {
		return Secret{}, err
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), jwt, nil, &resp); err != nil {
		return Secret{}, err
	}
	if resp.Token == "" {
		return Secret{}, errors.New("github: empty installation token")
	}
	t = token{value: NewSecret(resp.Token), expiresAt: resp.ExpiresAt}
	c.mu.Lock()
	c.tokens[id] = t
	c.mu.Unlock()
	return t.value, nil
}

func (c *Client) installation(ctx context.Context, repo config.Repo) (int64, error) {
	key := strings.ToLower(repo.String())
	c.mu.Lock()
	id, ok := c.installs[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	jwt, err := appJWT(c.key, c.appID, c.now())
	if err != nil {
		return 0, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if _, err := c.do(ctx, http.MethodGet, repoPath(repo, "installation"), jwt, nil, &inst); err != nil {
		return 0, fmt.Errorf("find the App installation for %s: %w", repo, err)
	}
	c.mu.Lock()
	c.installs[key] = inst.ID
	c.mu.Unlock()
	return inst.ID, nil
}

// repoPath builds /repos/{owner}/{name}/{parts...}, escaping each part.
func repoPath(repo config.Repo, parts ...string) string {
	p := "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// call is do with the installation token of repo.
func (c *Client) call(ctx context.Context, repo config.Repo, method, path string, body, out any) error {
	tok, err := c.Token(ctx, repo)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, method, path, tok, body, out)
	return err
}

// list fetches every page of a list endpoint, following the Link header.
func list[T any](ctx context.Context, c *Client, repo config.Repo, path string) ([]T, error) {
	tok, err := c.Token(ctx, repo)
	if err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := path + sep + "per_page=100"
	var all []T
	for next != "" {
		var page []T
		next, err = c.do(ctx, http.MethodGet, next, tok, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
	}
	return all, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// do sends one request. path is relative to the base URL, or an absolute
// URL from a Link header, which must point at the base URL's host: a
// credential is never sent anywhere else. It returns the next page path.
func (c *Client) do(ctx context.Context, method, path string, auth Secret, body, out any) (string, error) {
	u, err := c.resolve(path)
	if err != nil {
		return "", err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+auth.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error holds the URL, which never carries a credential.
		return "", fmt.Errorf("github: %s %s: %w", method, u.Path, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only body
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", fmt.Errorf("github: %s %s: %w", method, u.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m) // a body without a message still yields the status
		if len(m.Message) > 200 {
			m.Message = m.Message[:200]
		}
		return "", &APIError{Method: method, Path: u.Path, Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("github: %s %s: decode: %w", method, u.Path, err)
		}
	}
	next := ""
	if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		next = m[1]
	}
	return next, nil
}

func (c *Client) resolve(path string) (*url.URL, error) {
	if strings.HasPrefix(path, "/") {
		return url.Parse(c.baseURL.String() + path)
	}
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	if u.Scheme != c.baseURL.Scheme || u.Host != c.baseURL.Host {
		return nil, fmt.Errorf("github: refusing to follow a link to %s://%s", u.Scheme, u.Host)
	}
	return u, nil
}
