package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/store"
)

const repo = "Bibzor/demo-api"

var t0 = time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTransitionsOutboxAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "foreman.db")
	s := open(t, path)

	is := store.Issue{Repo: repo, Number: 7, State: store.Qualifying, Author: "alice", StateEnteredAt: t0}
	if _, err := s.Transition(ctx, store.None, is, "opt-in", store.Effect{Comment: "qualifying"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, store.None, is, "opt-in again", store.Effect{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second opt-in: err = %v, want ErrConflict", err)
	}
	is.State, is.StateEnteredAt, is.Snapshot, is.SnapshotSHA = store.AwaitingGo, t0.Add(time.Minute), `{"title":"t"}`, "abc"
	if _, err := s.Transition(ctx, store.Qualifying, is, "ready", store.Effect{Comment: "awaiting", RemoveLabels: []string{"go", "no-go"}}); err != nil {
		t.Fatal(err)
	}

	pending, err := s.Pending(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[1].State != store.AwaitingGo || !pending[1].SyncLabel || len(pending[1].RemoveLabels) != 2 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := s.MarkCommentPosted(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCommentPosted(ctx, pending[1].ID); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Everything survives a restart: state, history, the half-done outbox.
	s = open(t, path)
	got, ok, err := s.Get(ctx, repo, 7)
	if err != nil || !ok {
		t.Fatalf("Get: %v %v", ok, err)
	}
	if got.State != store.AwaitingGo || !got.StateEnteredAt.Equal(t0.Add(time.Minute)) || got.Author != "alice" {
		t.Fatalf("issue = %+v", got)
	}
	snap, sha, err := s.Snapshot(ctx, repo, 7)
	if err != nil || snap != `{"title":"t"}` || sha != "abc" {
		t.Fatalf("Snapshot = %q %q %v", snap, sha, err)
	}
	pending, err = s.Pending(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !pending[0].CommentPosted || pending[0].Comment != "awaiting" {
		t.Fatalf("pending after restart = %+v", pending)
	}
	hist, err := s.History(ctx, repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].From != store.None || hist[1].Reason != "ready" || !hist[1].At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("history = %+v", hist)
	}
	tracked, err := s.Tracked(ctx, repo)
	if err != nil || len(tracked) != 1 {
		t.Fatalf("Tracked = %v %v", tracked, err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %v", fi.Mode().Perm())
	}
}

func TestNoticeOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "foreman.db"))
	for i, want := range []bool{true, false} {
		added, err := s.Notice(ctx, repo, 3, 99, "not authorized")
		if err != nil || added != want {
			t.Fatalf("call %d: added = %v, %v", i, added, err)
		}
	}
	if added, _ := s.Notice(ctx, repo, 3, 100, "not authorized"); !added {
		t.Fatal("a new event must be noticed")
	}
	pending, _ := s.Pending(ctx, repo)
	if len(pending) != 2 || pending[0].SyncLabel {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestClosedNotTracked(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "foreman.db"))
	is := store.Issue{Repo: repo, Number: 1, State: store.Closed, Author: "a", StateEnteredAt: t0}
	if _, err := s.Transition(ctx, store.None, is, "x", store.Effect{}); err != nil {
		t.Fatal(err)
	}
	tracked, _ := s.Tracked(ctx, repo)
	if len(tracked) != 0 {
		t.Fatalf("tracked = %+v", tracked)
	}
	if store.Closed.Label() != "" || store.AwaitingGo.Label() != "foreman:awaiting-go" {
		t.Fatal("Label is wrong")
	}
}

// A database created by the first migration is upgraded in place.
func TestMigrateFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "foreman.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		store.MigrationsForTest()[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO issues (repo, number, state, author, state_entered_at) VALUES ('Bibzor/demo-api', 4, 'queued', 'alice', '2026-10-28T09:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	s := open(t, path)
	got, ok, err := s.Get(ctx, repo, 4)
	if err != nil || !ok {
		t.Fatalf("Get: %v %v", ok, err)
	}
	if got.State != store.Queued || got.PRNumber != 0 || got.PrepFailures != 0 || got.BaseSHA != "" {
		t.Fatalf("migrated issue = %+v", got)
	}
}

func TestDevelopmentFields(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "foreman.db"))
	is := store.Issue{Repo: repo, Number: 7, State: store.InReview, Author: "alice", StateEnteredAt: t0,
		BaseBranch: "main", BaseSHA: "b", DevSHA: "d", HeadSHA: "h", RevisionRounds: 1, LastReviewID: 42, PrereviewDone: true}
	pr := &store.PullRequest{Head: "foreman/7", Base: "main", Title: "t", Body: "Refs #7"}
	stored, err := s.Transition(ctx, store.None, is, "x", store.Effect{OpenPR: pr, Comment: "c", PR: 9, AddLabels: []string{"foreman:high-risk"}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx, repo)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	o := pending[0]
	if stored.StateOutboxID != o.ID || o.OpenPR == nil || *o.OpenPR != *pr || o.PR != 9 || len(o.AddLabels) != 1 {
		t.Fatalf("outbox = %+v, stored = %+v", o, stored)
	}
	if err := s.SetPR(ctx, repo, 7, 9); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		if n, err := s.PrepFailed(ctx, repo, 7); err != nil || n != want {
			t.Fatalf("PrepFailed = %d %v, want %d", n, err, want)
		}
	}
	got, _, _ := s.Get(ctx, repo, 7)
	is.StateOutboxID, is.PRNumber, is.PrepFailures = o.ID, 9, 2
	if got != is {
		t.Fatalf("issue = %+v\nwant    %+v", got, is)
	}

	other := store.Issue{Repo: "Bibzor/other", Number: 1, State: store.InProgress, Author: "a", StateEnteredAt: t0}
	if _, err := s.Transition(ctx, store.None, other, "x", store.Effect{}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountInStates(ctx, store.InProgress, store.InReview); err != nil || n != 2 {
		t.Fatalf("CountInStates = %d %v, want 2 across repos", n, err)
	}
	if n, _ := s.CountInStates(ctx, store.Queued); n != 0 {
		t.Fatalf("CountInStates(queued) = %d", n)
	}
}
