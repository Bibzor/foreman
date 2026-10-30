// Package store is the orchestrator's SQLite database, the source of
// truth for every tracked issue. GitHub labels only mirror it.
//
// It uses modernc.org/sqlite, a pure-Go SQLite: no cgo, so the race
// detector and cross-builds keep working.
//
// A transition is one database transaction: the issue row, the history
// row and an outbox row holding the GitHub side effects (comment, labels,
// closing). The side effects run afterwards from the outbox, so a crash
// between the two loses nothing and a restart replays what is pending.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// State is the state of a tracked issue.
type State string

// States.
const (
	None         State = "" // not tracked yet
	Qualifying   State = "qualifying"
	NeedsInfo    State = "needs-info"
	AwaitingGo   State = "awaiting-go"
	Queued       State = "queued"
	Blocked      State = "blocked"
	Suspect      State = "suspect"
	InProgress   State = "in-progress"
	InReview     State = "in-review"
	Merged       State = "merged"
	Deploying    State = "deploying"
	InAcceptance State = "in-acceptance"
	Done         State = "done" // accepted, the issue closed: no longer tracked
	Closed       State = "closed"
	labelPrefix        = "foreman:"
)

// Label returns the GitHub label mirroring s, or "" for closed.
func (s State) Label() string {
	if s == Closed || s == None {
		return ""
	}
	return labelPrefix + string(s)
}

// ErrConflict is returned when an issue is no longer in the state a
// transition starts from.
var ErrConflict = errors.New("state changed concurrently")

// Issue is a tracked issue.
type Issue struct {
	Repo            string
	Number          int
	State           State
	Author          string
	NeedsInfoRounds int
	StateEnteredAt  time.Time
	// Qualification is the validated agent result, as JSON.
	Qualification string
	// Snapshot is the issue content the qualification was made on, as
	// canonical JSON, and SnapshotSHA its sha256. Later steps read the
	// snapshot, never the live issue.
	Snapshot    string
	SnapshotSHA string
	// PrepFailures counts consecutive failures to prepare a workspace in
	// the current state; every transition resets it.
	PrepFailures int
	// StateOutboxID is the outbox entry of the transition into the current
	// state: its comment, on GitHub's clock, marks when the state began.
	StateOutboxID int64
	// BaseBranch and BaseSHA are the default branch and the commit the
	// work started from; DevSHA is the tip pushed by the development run
	// and HeadSHA the last tip foreman pushed.
	BaseBranch string
	BaseSHA    string
	DevSHA     string
	HeadSHA    string
	PRNumber   int
	// RevisionRounds counts the revisions made after a review.
	RevisionRounds int
	// LastReviewID is the newest PR review already handled.
	LastReviewID  int64
	PrereviewDone bool
	// MergeSHA is the merge commit of the PR, the commit acceptance
	// deploys.
	MergeSHA string
	// AcceptanceRounds counts the developments restarted after an
	// acceptance-ko; AcceptanceFeedback and AcceptanceFeedbackAuthor are
	// the comment explaining the last one. The feedback is untrusted.
	AcceptanceRounds         int
	AcceptanceFeedback       string
	AcceptanceFeedbackAuthor string
}

// Transition is a history entry.
type Transition struct {
	ID     int64
	Repo   string
	Number int
	From   State
	To     State
	Reason string
	At     time.Time
}

// Effect is what a transition or notice does on GitHub.
type Effect struct {
	OpenPR       *PullRequest // opened (or adopted) before anything else
	Comment      string
	PR           int      // when set, Comment and AddLabels go to this pull request
	RemoveLabels []string // e.g. a consumed go label
	SyncLabel    bool     // mirror the state as a foreman:* label on the issue
	State        State    // the state to mirror when SyncLabel is set
	AddLabels    []string
	Close        bool
}

// PullRequest is a pull request to open.
type PullRequest struct {
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Outbox is a pending Effect.
type Outbox struct {
	ID            int64
	Repo          string
	Number        int
	CommentPosted bool
	Effect
}

// Store is the database.
type Store struct{ db *sql.DB }

// Open opens or creates the database at path, creating its directory
// (mode 0700) and applying migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; avoids SQLITE_BUSY between our own connections
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close() // the migration error is the one to report
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close() // the chmod error is the one to report
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE issues (
		repo TEXT NOT NULL,
		number INTEGER NOT NULL,
		state TEXT NOT NULL,
		author TEXT NOT NULL,
		needs_info_rounds INTEGER NOT NULL DEFAULT 0,
		state_entered_at TEXT NOT NULL,
		qualification TEXT NOT NULL DEFAULT '',
		snapshot TEXT NOT NULL DEFAULT '',
		snapshot_sha256 TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (repo, number)
	);
	CREATE TABLE transitions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo TEXT NOT NULL,
		number INTEGER NOT NULL,
		from_state TEXT NOT NULL,
		to_state TEXT NOT NULL,
		reason TEXT NOT NULL,
		at TEXT NOT NULL
	);
	CREATE TABLE outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo TEXT NOT NULL,
		number INTEGER NOT NULL,
		comment TEXT NOT NULL,
		remove_labels TEXT NOT NULL,
		sync_label INTEGER NOT NULL,
		state TEXT NOT NULL,
		close_issue INTEGER NOT NULL,
		comment_posted INTEGER NOT NULL DEFAULT 0,
		done INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE notices (
		repo TEXT NOT NULL,
		number INTEGER NOT NULL,
		event_id INTEGER NOT NULL,
		PRIMARY KEY (repo, number, event_id)
	);`,
	`ALTER TABLE issues ADD COLUMN prep_failures INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE issues ADD COLUMN state_outbox_id INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE issues ADD COLUMN base_branch TEXT NOT NULL DEFAULT '';
	ALTER TABLE issues ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';
	ALTER TABLE issues ADD COLUMN dev_sha TEXT NOT NULL DEFAULT '';
	ALTER TABLE issues ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
	ALTER TABLE issues ADD COLUMN pr_number INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE issues ADD COLUMN revision_rounds INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE issues ADD COLUMN last_review_id INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE issues ADD COLUMN prereview_done INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE outbox ADD COLUMN open_pr TEXT NOT NULL DEFAULT '';
	ALTER TABLE outbox ADD COLUMN pr INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE outbox ADD COLUMN add_labels TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE issues ADD COLUMN merge_sha TEXT NOT NULL DEFAULT '';
	ALTER TABLE issues ADD COLUMN acceptance_rounds INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE issues ADD COLUMN acceptance_feedback TEXT NOT NULL DEFAULT '';
	ALTER TABLE issues ADD COLUMN acceptance_feedback_author TEXT NOT NULL DEFAULT '';`,
}

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback() // f's error is the one to report
		return err
	}
	return tx.Commit()
}

const issueCols = `repo, number, state, author, needs_info_rounds, state_entered_at, qualification, snapshot, snapshot_sha256,
	prep_failures, state_outbox_id, base_branch, base_sha, dev_sha, head_sha, pr_number, revision_rounds, last_review_id, prereview_done,
	merge_sha, acceptance_rounds, acceptance_feedback, acceptance_feedback_author`

func scanIssue(row interface{ Scan(...any) error }) (Issue, error) {
	var is Issue
	var at string
	err := row.Scan(&is.Repo, &is.Number, &is.State, &is.Author, &is.NeedsInfoRounds, &at, &is.Qualification, &is.Snapshot, &is.SnapshotSHA,
		&is.PrepFailures, &is.StateOutboxID, &is.BaseBranch, &is.BaseSHA, &is.DevSHA, &is.HeadSHA, &is.PRNumber, &is.RevisionRounds, &is.LastReviewID, &is.PrereviewDone,
		&is.MergeSHA, &is.AcceptanceRounds, &is.AcceptanceFeedback, &is.AcceptanceFeedbackAuthor)
	if err != nil {
		return Issue{}, err
	}
	is.StateEnteredAt, err = time.Parse(time.RFC3339Nano, at)
	return is, err
}

// Get returns a tracked issue. ok is false when it is not tracked.
func (s *Store) Get(ctx context.Context, repo string, number int) (is Issue, ok bool, err error) {
	is, err = scanIssue(s.db.QueryRowContext(ctx, `SELECT `+issueCols+` FROM issues WHERE repo = ? AND number = ?`, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, false, nil
	}
	return is, err == nil, err
}

// Tracked returns the tracked issues of repo that are neither closed nor
// done, by number.
func (s *Store) Tracked(ctx context.Context, repo string) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+issueCols+` FROM issues WHERE repo = ? AND state NOT IN (?, ?) ORDER BY number`,
		repo, Closed, Done)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only
	var out []Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, is)
	}
	return out, rows.Err()
}

// Snapshot returns the issue content the qualification was made on and
// its sha256. This is the text later steps work from.
func (s *Store) Snapshot(ctx context.Context, repo string, number int) (snapshot, sha string, err error) {
	is, ok, err := s.Get(ctx, repo, number)
	if err != nil {
		return "", "", err
	}
	if !ok || is.SnapshotSHA == "" {
		return "", "", fmt.Errorf("%s#%d has no snapshot", repo, number)
	}
	return is.Snapshot, is.SnapshotSHA, nil
}

// Transition moves an issue from state from to next.State, records the
// history entry and queues effect, in one database transaction. next is
// the full new issue row; the stored row is returned, with StateOutboxID
// set to the queued entry. It fails with ErrConflict if the stored state is not from (None: the
// issue must not be tracked yet).
func (s *Store) Transition(ctx context.Context, from State, next Issue, reason string, effect Effect) (Issue, error) {
	at := next.StateEnteredAt.UTC().Format(time.RFC3339Nano)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var cur State
		err := tx.QueryRowContext(ctx, `SELECT state FROM issues WHERE repo = ? AND number = ?`, next.Repo, next.Number).Scan(&cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			cur = None
		case err != nil:
			return err
		}
		if cur != from {
			return fmt.Errorf("%s#%d is %q, not %q: %w", next.Repo, next.Number, cur, from, ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transitions (repo, number, from_state, to_state, reason, at) VALUES (?, ?, ?, ?, ?, ?)`,
			next.Repo, next.Number, from, next.State, reason, at); err != nil {
			return err
		}
		effect.SyncLabel, effect.State = true, next.State
		id, err := queue(ctx, tx, next.Repo, next.Number, effect)
		if err != nil {
			return err
		}
		next.StateOutboxID = id
		_, err = tx.ExecContext(ctx, `INSERT INTO issues (`+issueCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (repo, number) DO UPDATE SET state = excluded.state, author = excluded.author,
			needs_info_rounds = excluded.needs_info_rounds, state_entered_at = excluded.state_entered_at,
			qualification = excluded.qualification, snapshot = excluded.snapshot, snapshot_sha256 = excluded.snapshot_sha256,
			prep_failures = excluded.prep_failures, state_outbox_id = excluded.state_outbox_id,
			base_branch = excluded.base_branch, base_sha = excluded.base_sha, dev_sha = excluded.dev_sha, head_sha = excluded.head_sha,
			pr_number = excluded.pr_number, revision_rounds = excluded.revision_rounds,
			last_review_id = excluded.last_review_id, prereview_done = excluded.prereview_done,
			merge_sha = excluded.merge_sha, acceptance_rounds = excluded.acceptance_rounds,
			acceptance_feedback = excluded.acceptance_feedback, acceptance_feedback_author = excluded.acceptance_feedback_author`,
			next.Repo, next.Number, next.State, next.Author, next.NeedsInfoRounds, at, next.Qualification, next.Snapshot, next.SnapshotSHA,
			next.PrepFailures, next.StateOutboxID, next.BaseBranch, next.BaseSHA, next.DevSHA, next.HeadSHA,
			next.PRNumber, next.RevisionRounds, next.LastReviewID, next.PrereviewDone,
			next.MergeSHA, next.AcceptanceRounds, next.AcceptanceFeedback, next.AcceptanceFeedbackAuthor)
		return err
	})
	if err != nil {
		return Issue{}, err
	}
	return next, nil
}

// PrepFailed records one more failure to prepare a workspace for the
// issue and returns the count of consecutive failures.
func (s *Store) PrepFailed(ctx context.Context, repo string, number int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `UPDATE issues SET prep_failures = prep_failures + 1 WHERE repo = ? AND number = ? RETURNING prep_failures`,
		repo, number).Scan(&n)
	return n, err
}

// SetPR records the pull request opened for an issue.
func (s *Store) SetPR(ctx context.Context, repo string, number, pr int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE issues SET pr_number = ? WHERE repo = ? AND number = ?`, pr, repo, number)
	return err
}

// CountInStates counts the tracked issues of every repository that are in
// one of states.
func (s *Store) CountInStates(ctx context.Context, states ...State) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE state IN (?`+strings.Repeat(", ?", len(states)-1)+`)`, args...).Scan(&n)
	return n, err
}

// Notice queues a one-off comment keyed by a GitHub event. It returns
// false, and queues nothing, if that event was already noticed. GitHub
// event IDs are positive: foreman keys its own one-off notices with
// negative IDs (see the orchestrator).
func (s *Store) Notice(ctx context.Context, repo string, number int, eventID int64, comment string) (bool, error) {
	added := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notices (repo, number, event_id) VALUES (?, ?, ?)`, repo, number, eventID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 { // the sqlite driver always reports it
			return nil
		}
		added = true
		_, err = queue(ctx, tx, repo, number, Effect{Comment: comment})
		return err
	})
	return added, err
}

func queue(ctx context.Context, tx *sql.Tx, repo string, number int, e Effect) (int64, error) {
	var openPR string
	if e.OpenPR != nil {
		b, err := json.Marshal(e.OpenPR)
		if err != nil {
			return 0, err
		}
		openPR = string(b)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO outbox (repo, number, comment, remove_labels, sync_label, state, close_issue, open_pr, pr, add_labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repo, number, e.Comment, strings.Join(e.RemoveLabels, "\n"), e.SyncLabel, e.State, e.Close, openPR, e.PR, strings.Join(e.AddLabels, "\n"))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Pending returns the outbox entries not done yet, oldest first.
func (s *Store) Pending(ctx context.Context, repo string) ([]Outbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo, number, comment, remove_labels, sync_label, state, close_issue, comment_posted,
		open_pr, pr, add_labels FROM outbox WHERE repo = ? AND done = 0 ORDER BY id`, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only
	var out []Outbox
	for rows.Next() {
		var o Outbox
		var remove, add, openPR string
		if err := rows.Scan(&o.ID, &o.Repo, &o.Number, &o.Comment, &remove, &o.SyncLabel, &o.State, &o.Close, &o.CommentPosted,
			&openPR, &o.PR, &add); err != nil {
			return nil, err
		}
		if remove != "" {
			o.RemoveLabels = strings.Split(remove, "\n")
		}
		if add != "" {
			o.AddLabels = strings.Split(add, "\n")
		}
		if openPR != "" {
			o.OpenPR = new(PullRequest)
			if err := json.Unmarshal([]byte(openPR), o.OpenPR); err != nil {
				return nil, err
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkCommentPosted records that the outbox entry's comment is on GitHub.
func (s *Store) MarkCommentPosted(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET comment_posted = 1 WHERE id = ?`, id)
	return err
}

// MarkDone records that every effect of the outbox entry is applied.
func (s *Store) MarkDone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET done = 1 WHERE id = ?`, id)
	return err
}

// History returns the transitions of an issue, oldest first.
func (s *Store) History(ctx context.Context, repo string, number int) ([]Transition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo, number, from_state, to_state, reason, at FROM transitions
		WHERE repo = ? AND number = ? ORDER BY id`, repo, number)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only
	var out []Transition
	for rows.Next() {
		var t Transition
		var at string
		if err := rows.Scan(&t.ID, &t.Repo, &t.Number, &t.From, &t.To, &t.Reason, &at); err != nil {
			return nil, err
		}
		if t.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
