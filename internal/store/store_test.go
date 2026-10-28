package store_test

import (
	"context"
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
	if err := s.Transition(ctx, store.None, is, "opt-in", store.Effect{Comment: "qualifying"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, store.None, is, "opt-in again", store.Effect{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second opt-in: err = %v, want ErrConflict", err)
	}
	is.State, is.StateEnteredAt, is.Snapshot, is.SnapshotSHA = store.AwaitingGo, t0.Add(time.Minute), `{"title":"t"}`, "abc"
	if err := s.Transition(ctx, store.Qualifying, is, "ready", store.Effect{Comment: "awaiting", RemoveLabels: []string{"go", "no-go"}}); err != nil {
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
	if err := s.Transition(ctx, store.None, is, "x", store.Effect{}); err != nil {
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
