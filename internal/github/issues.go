package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/config"
)

// Issue is an issue as foreman sees it.
type Issue struct {
	Number int
	Title  string
	Body   string
	Author string
	Open   bool
	Labels []string
}

// HasLabel reports whether the issue carries the label name.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// Comment is an issue comment.
type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
}

// LabelEvent is a "labeled" event of an issue timeline.
type LabelEvent struct {
	ID        int64
	Label     string
	Actor     string
	CreatedAt time.Time
}

// Label is a repository label.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type apiUser struct {
	Login string `json:"login"`
}

type apiIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        apiUser   `json:"user"`
	Labels      []Label   `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
}

func (a apiIssue) issue() Issue {
	is := Issue{Number: a.Number, Title: a.Title, Body: a.Body, Author: a.User.Login, Open: a.State == "open"}
	for _, l := range a.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is
}

// DefaultBranch returns the default branch of repo.
func (c *Client) DefaultBranch(ctx context.Context, repo config.Repo) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.call(ctx, repo, http.MethodGet, repoPath(repo), nil, &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("github: %s has no default branch", repo)
	}
	return r.DefaultBranch, nil
}

// EnsureLabels creates the labels missing from repo. Existing labels are
// left as they are.
func (c *Client) EnsureLabels(ctx context.Context, repo config.Repo, labels []Label) error {
	existing, err := list[Label](ctx, c, repo, repoPath(repo, "labels"))
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[strings.ToLower(l.Name)] = true
	}
	for _, l := range labels {
		if have[strings.ToLower(l.Name)] {
			continue
		}
		err := c.call(ctx, repo, http.MethodPost, repoPath(repo, "labels"), l, nil)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
			continue // created concurrently
		}
		if err != nil {
			return fmt.Errorf("create label %q: %w", l.Name, err)
		}
	}
	return nil
}

// OpenIssuesWithLabel lists the open issues of repo carrying label. Pull
// requests are left out.
func (c *Client) OpenIssuesWithLabel(ctx context.Context, repo config.Repo, label string) ([]Issue, error) {
	raw, err := list[apiIssue](ctx, c, repo, repoPath(repo, "issues")+"?state=open&labels="+url.QueryEscape(label))
	if err != nil {
		return nil, err
	}
	var out []Issue
	for _, a := range raw {
		if a.PullRequest == nil {
			out = append(out, a.issue())
		}
	}
	return out, nil
}

// Issue returns one issue.
func (c *Client) Issue(ctx context.Context, repo config.Repo, number int) (Issue, error) {
	var a apiIssue
	if err := c.call(ctx, repo, http.MethodGet, repoPath(repo, "issues", strconv.Itoa(number)), nil, &a); err != nil {
		return Issue{}, err
	}
	return a.issue(), nil
}

// Comments lists every comment of an issue, oldest first.
func (c *Client) Comments(ctx context.Context, repo config.Repo, number int) ([]Comment, error) {
	raw, err := list[struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		User      apiUser   `json:"user"`
		CreatedAt time.Time `json:"created_at"`
	}](ctx, c, repo, repoPath(repo, "issues", strconv.Itoa(number), "comments"))
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(raw))
	for _, r := range raw {
		out = append(out, Comment{ID: r.ID, Author: r.User.Login, Body: r.Body, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// LabelEvents lists the "labeled" events of an issue timeline, oldest
// first.
func (c *Client) LabelEvents(ctx context.Context, repo config.Repo, number int) ([]LabelEvent, error) {
	raw, err := list[struct {
		ID        int64     `json:"id"`
		Event     string    `json:"event"`
		Actor     *apiUser  `json:"actor"`
		Label     *Label    `json:"label"`
		CreatedAt time.Time `json:"created_at"`
	}](ctx, c, repo, repoPath(repo, "issues", strconv.Itoa(number), "timeline"))
	if err != nil {
		return nil, err
	}
	var out []LabelEvent
	for _, r := range raw {
		if r.Event != "labeled" || r.Label == nil || r.Actor == nil {
			continue
		}
		out = append(out, LabelEvent{ID: r.ID, Label: r.Label.Name, Actor: r.Actor.Login, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// AddLabel adds a label to an issue.
func (c *Client) AddLabel(ctx context.Context, repo config.Repo, number int, label string) error {
	body := struct {
		Labels []string `json:"labels"`
	}{[]string{label}}
	return c.call(ctx, repo, http.MethodPost, repoPath(repo, "issues", strconv.Itoa(number), "labels"), body, nil)
}

// RemoveLabel removes a label from an issue. A label that is not there is
// not an error.
func (c *Client) RemoveLabel(ctx context.Context, repo config.Repo, number int, label string) error {
	err := c.call(ctx, repo, http.MethodDelete, repoPath(repo, "issues", strconv.Itoa(number), "labels", label), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// CreateComment posts a comment and returns its ID.
func (c *Client) CreateComment(ctx context.Context, repo config.Repo, number int, body string) (int64, error) {
	var r struct {
		ID int64 `json:"id"`
	}
	in := struct {
		Body string `json:"body"`
	}{body}
	if err := c.call(ctx, repo, http.MethodPost, repoPath(repo, "issues", strconv.Itoa(number), "comments"), in, &r); err != nil {
		return 0, err
	}
	return r.ID, nil
}

// CloseIssue closes an issue as not planned.
func (c *Client) CloseIssue(ctx context.Context, repo config.Repo, number int) error {
	in := struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
	}{"closed", "not_planned"}
	return c.call(ctx, repo, http.MethodPatch, repoPath(repo, "issues", strconv.Itoa(number)), in, nil)
}
