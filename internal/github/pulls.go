package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Bibzor/foreman/internal/config"
)

// PullRequest is a pull request as foreman sees it.
type PullRequest struct {
	Number  int
	Open    bool
	Merged  bool
	Head    string // branch name
	Body    string
	Commits int
}

// Review is a pull request review.
type Review struct {
	ID          int64
	Author      string
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	Body        string
	SubmittedAt time.Time
}

// ReviewComment is an inline comment of a review.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

type apiPull struct {
	Number int     `json:"number"`
	State  string  `json:"state"`
	Merged bool    `json:"merged"`
	Body   *string `json:"body"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
	MergedAt *time.Time `json:"merged_at"`
	Commits  int        `json:"commits"`
}

func (a apiPull) pull() PullRequest {
	pr := PullRequest{Number: a.Number, Open: a.State == "open", Merged: a.Merged || a.MergedAt != nil, Head: a.Head.Ref, Commits: a.Commits}
	if a.Body != nil {
		pr.Body = *a.Body
	}
	return pr
}

// CreatePullRequest opens a pull request from head to base and returns
// its number.
func (c *Client) CreatePullRequest(ctx context.Context, repo config.Repo, head, base, title, body string) (int, error) {
	in := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}{title, head, base, body}
	var out apiPull
	if err := c.call(ctx, repo, http.MethodPost, repoPath(repo, "pulls"), in, &out); err != nil {
		return 0, err
	}
	return out.Number, nil
}

// PullRequestsByHead lists the pull requests, open or closed, whose head
// is the branch of repo itself.
func (c *Client) PullRequestsByHead(ctx context.Context, repo config.Repo, branch string) ([]PullRequest, error) {
	q := url.Values{"state": {"all"}, "head": {repo.Owner + ":" + branch}}
	raw, err := list[apiPull](ctx, c, repo, repoPath(repo, "pulls")+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(raw))
	for _, a := range raw {
		out = append(out, a.pull())
	}
	return out, nil
}

// PullRequest returns one pull request.
func (c *Client) PullRequest(ctx context.Context, repo config.Repo, number int) (PullRequest, error) {
	var a apiPull
	if err := c.call(ctx, repo, http.MethodGet, repoPath(repo, "pulls", strconv.Itoa(number)), nil, &a); err != nil {
		return PullRequest{}, err
	}
	return a.pull(), nil
}

// Reviews lists the reviews of a pull request, oldest first.
func (c *Client) Reviews(ctx context.Context, repo config.Repo, number int) ([]Review, error) {
	raw, err := list[struct {
		ID          int64     `json:"id"`
		User        *apiUser  `json:"user"`
		State       string    `json:"state"`
		Body        string    `json:"body"`
		SubmittedAt time.Time `json:"submitted_at"`
	}](ctx, c, repo, repoPath(repo, "pulls", strconv.Itoa(number), "reviews"))
	if err != nil {
		return nil, err
	}
	out := make([]Review, 0, len(raw))
	for _, r := range raw {
		if r.User == nil {
			continue // deleted account
		}
		out = append(out, Review{ID: r.ID, Author: r.User.Login, State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt})
	}
	return out, nil
}

// ReviewComments lists the inline comments of one review.
func (c *Client) ReviewComments(ctx context.Context, repo config.Repo, number int, reviewID int64) ([]ReviewComment, error) {
	raw, err := list[struct {
		Path string `json:"path"`
		Line *int   `json:"line"`
		Body string `json:"body"`
	}](ctx, c, repo, repoPath(repo, "pulls", strconv.Itoa(number), "reviews", strconv.FormatInt(reviewID, 10), "comments"))
	if err != nil {
		return nil, err
	}
	out := make([]ReviewComment, 0, len(raw))
	for _, r := range raw {
		rc := ReviewComment{Path: r.Path, Body: r.Body}
		if r.Line != nil {
			rc.Line = *r.Line
		}
		out = append(out, rc)
	}
	return out, nil
}
