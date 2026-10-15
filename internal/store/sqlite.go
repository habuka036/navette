// Package store provides SQLite persistence for NAVETTE's Control Plane. SQLite
// is the source of truth for task state and the event/approval history. State
// transitions and their event records are written atomically within a single
// transaction so state and history never diverge.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/habuka036/navette/internal/task"
	_ "modernc.org/sqlite"
)

//go:embed migrations/001_initial_schema.sql
var migration001 string

// ErrNotFound is returned when a task does not exist.
var ErrNotFound = errors.New("store: task not found")

// ErrUnexpectedState is returned when an atomic approval+transition finds the
// task in a different state than expected (for example a concurrent run already
// advanced it).
type ErrUnexpectedState struct {
	Want task.State
	Got  task.State
}

func (e ErrUnexpectedState) Error() string {
	return fmt.Sprintf("store: unexpected state: want %s, got %s", e.Want, e.Got)
}

// ErrDuplicateApproval is returned when a decision of the same kind already
// exists for the task.
type ErrDuplicateApproval struct {
	TaskID string
	Kind   task.ApprovalKind
}

func (e ErrDuplicateApproval) Error() string {
	return fmt.Sprintf("store: duplicate %s approval for task %s", e.Kind, e.TaskID)
}

const timeFormat = time.RFC3339Nano

// Store is a SQLite-backed Control Plane store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) a file-based SQLite store at path, applies
// PRAGMAs, and runs migrations. In-memory databases are rejected to match the
// durable-persistence requirement.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == ":memory:" || strings.Contains(strings.ToLower(path), "mode=memory") {
		return nil, fmt.Errorf("store.Open: in-memory databases are not supported")
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store.Open: ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, migration001); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store.Open: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

// CreateTask inserts a new task in its initial state atomically with a creation
// event.
func (s *Store) CreateTask(ctx context.Context, t task.Task, ev task.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		inputJSON, _ := json.Marshal(t.Input)
		consJSON, _ := json.Marshal(constraintsDTO{
			TimeoutNS:     int64(t.Constraints.Timeout),
			MaxIterations: t.Constraints.MaxIterations,
			WorkDir:       t.Constraints.WorkDir,
		})
		_, err := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, goal, input, constraints, state, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.Goal, string(inputJSON), string(consJSON), string(t.State),
			t.CreatedAt.UTC().Format(timeFormat), t.UpdatedAt.UTC().Format(timeFormat),
		)
		if err != nil {
			return fmt.Errorf("insert task: %w", err)
		}
		return insertEvent(ctx, tx, ev)
	})
}

// Transition atomically validates and applies a state transition and records the
// transition event in the same transaction. It returns task.ErrInvalidTransition
// (without modifying state) if the transition is not allowed, and ErrNotFound if
// the task does not exist.
func (s *Store) Transition(ctx context.Context, taskID string, to task.State, reason string, ev *task.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := applyTransition(ctx, tx, taskID, to, reason, ev)
		return err
	})
}

// ApproveAndTransition atomically, in a single transaction:
//   - validates the task is currently in expectFrom and the approval kind matches,
//   - rejects duplicate decisions of the same kind for the task,
//   - records the approval row and an approval event,
//   - applies the resulting state transition and records its transition event.
//
// The resulting target state is chosen by the caller: toApproved when the
// decision approves, toRejected when it rejects. If the task is not in
// expectFrom, ErrUnexpectedState is returned and nothing is written. If a
// decision of the same kind already exists, ErrDuplicateApproval is returned.
// On any failure the whole transaction rolls back (no partial records).
func (s *Store) ApproveAndTransition(
	ctx context.Context,
	taskID string,
	expectFrom task.State,
	d task.Decision,
	toApproved task.State,
	toRejected task.State,
) (task.State, error) {
	var result task.State
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Validate current state within the transaction.
		var fromStr string
		err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&fromStr)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read state: %w", err)
		}
		from := task.State(fromStr)
		if from != expectFrom {
			return ErrUnexpectedState{Want: expectFrom, Got: from}
		}

		// Reject duplicate decisions of the same kind for this task.
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM approvals WHERE task_id = ? AND kind = ?`,
			taskID, string(d.Kind)).Scan(&n); err != nil {
			return fmt.Errorf("check duplicate approval: %w", err)
		}
		if n > 0 {
			return ErrDuplicateApproval{TaskID: taskID, Kind: d.Kind}
		}

		// Record the approval row.
		approved := 0
		if d.Approved {
			approved = 1
		}
		at := d.At
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO approvals (task_id, kind, approved, decided_by, decided_at, reason)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			taskID, string(d.Kind), approved, d.By, at.UTC().Format(timeFormat), d.Reason); err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}

		// Record the approval event.
		detail, _ := json.Marshal(map[string]any{"kind": d.Kind, "approved": d.Approved, "by": d.By})
		if err := insertEvent(ctx, tx, task.Event{
			TaskID: taskID, Kind: task.EventApproval, Reason: d.Reason, Detail: detail,
		}); err != nil {
			return err
		}

		// Apply the resulting transition (+ transition event) in the same tx.
		to := toApproved
		if !d.Approved {
			to = toRejected
		}
		if _, err := applyTransition(ctx, tx, taskID, to, approvalReason(d), nil); err != nil {
			return err
		}
		result = to
		return nil
	})
	return result, err
}

func approvalReason(d task.Decision) string {
	verb := "approved"
	if !d.Approved {
		verb = "rejected"
	}
	return fmt.Sprintf("%s approval %s", d.Kind, verb)
}

// applyTransition validates and applies a transition within an existing tx and
// records the transition event. It returns the resolved "from" state.
func applyTransition(ctx context.Context, tx *sql.Tx, taskID string, to task.State, reason string, ev *task.Event) (task.State, error) {
	var fromStr string
	err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&fromStr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read state: %w", err)
	}
	from := task.State(fromStr)
	if !task.CanTransition(from, to) {
		return from, task.ErrInvalidTransition{From: from, To: to}
	}
	now := time.Now().UTC().Format(timeFormat)
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		string(to), now, taskID); err != nil {
		return from, fmt.Errorf("update state: %w", err)
	}
	te := task.Event{TaskID: taskID, Kind: task.EventStateTransition, From: from, To: to, Reason: reason}
	if ev != nil {
		te.RunID = ev.RunID
		te.Detail = ev.Detail
	}
	if err := insertEvent(ctx, tx, te); err != nil {
		return from, err
	}
	return from, nil
}

// AppendEvent records a standalone event (for example runtime output or notes).
func (s *Store) AppendEvent(ctx context.Context, ev task.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return insertEvent(ctx, tx, ev) })
}

// CreateRun records a new run for a task.
func (s *Store) CreateRun(ctx context.Context, runID, taskID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (id, task_id, started_at) VALUES (?, ?, ?)`,
		runID, taskID, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("store.CreateRun: %w", err)
	}
	return nil
}

// SetRunArtifacts records the durable artifact directory and its content
// fingerprint for a run. The mapping lets a human locate preserved artifacts and
// lets NAVETTE verify that approved artifacts match what was saved. It returns
// ErrNotFound if no run with runID exists (UPDATE affected 0 rows).
func (s *Store) SetRunArtifacts(ctx context.Context, runID, artifactDir, artifactHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET artifact_dir = ?, artifact_hash = ? WHERE id = ?`,
		artifactDir, artifactHash, runID)
	if err != nil {
		return fmt.Errorf("store.SetRunArtifacts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.SetRunArtifacts: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.SetRunArtifacts: run %q: %w", runID, ErrNotFound)
	}
	return nil
}

// FinishRun records the run outcome and end time. It returns ErrNotFound if no
// run with runID exists (UPDATE affected 0 rows).
func (s *Store) FinishRun(ctx context.Context, runID string, outcome task.Outcome) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET ended_at = ?, outcome = ? WHERE id = ?`,
		time.Now().UTC().Format(timeFormat), string(outcome), runID)
	if err != nil {
		return fmt.Errorf("store.FinishRun: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.FinishRun: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.FinishRun: run %q: %w", runID, ErrNotFound)
	}
	return nil
}

// RunArtifacts returns the artifact directory and fingerprint for a run.
func (s *Store) RunArtifacts(ctx context.Context, runID string) (dir, hash string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT artifact_dir, artifact_hash FROM runs WHERE id = ?`, runID).Scan(&dir, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return dir, hash, err
}

// GetTask returns a task by id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id string) (task.Task, error) {
	var (
		t                     task.Task
		inputJSON, consJSON   string
		stateStr              string
		createdStr, updateStr string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, goal, input, constraints, state, created_at, updated_at FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.Goal, &inputJSON, &consJSON, &stateStr, &createdStr, &updateStr)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, ErrNotFound
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("store.GetTask: %w", err)
	}
	t.State = task.State(stateStr)
	_ = json.Unmarshal([]byte(inputJSON), &t.Input)
	var dto constraintsDTO
	_ = json.Unmarshal([]byte(consJSON), &dto)
	t.Constraints = task.Constraints{
		Timeout:       time.Duration(dto.TimeoutNS),
		MaxIterations: dto.MaxIterations,
		WorkDir:       dto.WorkDir,
	}
	t.CreatedAt, _ = time.Parse(timeFormat, createdStr)
	t.UpdatedAt, _ = time.Parse(timeFormat, updateStr)
	return t, nil
}

// ListTasks returns all tasks ordered by creation time ascending.
func (s *Store) ListTasks(ctx context.Context) ([]task.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store.ListTasks: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]task.Task, 0, len(ids))
	for _, id := range ids {
		t, err := s.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Events returns all events for a task in chronological (id ascending) order.
func (s *Store) Events(ctx context.Context, taskID string) ([]task.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, run_id, at, kind, from_state, to_state, reason, detail
		 FROM events WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store.Events: %w", err)
	}
	defer rows.Close()
	var out []task.Event
	for rows.Next() {
		var (
			ev             task.Event
			atStr, kind    string
			fromStr, toStr string
			detail         string
		)
		if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.RunID, &atStr, &kind, &fromStr, &toStr, &ev.Reason, &detail); err != nil {
			return nil, err
		}
		ev.At, _ = time.Parse(timeFormat, atStr)
		ev.Kind = task.EventKind(kind)
		ev.From = task.State(fromStr)
		ev.To = task.State(toStr)
		if detail != "" {
			ev.Detail = json.RawMessage(detail)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// RecoverInterrupted moves any task left mid-execution (Running) or
// mid-finalization (Finalizing) — for example after a crash or restart — to
// Indeterminate, recording the transition. It never auto-transitions to a
// success/failure state and never auto-resumes:
//
//   - A task interrupted while Running has an unknown execution outcome.
//   - A task interrupted while Finalizing may have partially saved artifacts
//     whose consistency with the approved snapshot is unverified. It must NOT
//     become Succeeded automatically; the (partial) artifacts are preserved and
//     a human must review before any further action.
//
// Tasks already in an approval-waiting state are left untouched (their
// unapproved state is preserved).
//
// Scope: RecoverInterrupted is responsible ONLY for Control Plane state recovery
// (reconciling in-memory/process state after a restart). It does NOT reclaim OS
// resources such as orphaned child/grandchild processes spawned by a runtime;
// process-tree/sandbox teardown is the responsibility of the Milestone C
// isolation implementation (see docs/specs/harness/notes/milestone-a-experiments.md).
func (s *Store) RecoverInterrupted(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, state FROM tasks WHERE state IN (?, ?)`,
		string(task.Running), string(task.Finalizing))
	if err != nil {
		return 0, fmt.Errorf("store.RecoverInterrupted: %w", err)
	}
	type rec struct{ id, state string }
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.state); err != nil {
			rows.Close()
			return 0, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range recs {
		reason := "process restart: run interrupted"
		if r.state == string(task.Finalizing) {
			reason = "process restart: interrupted during result finalization; " +
				"artifacts unverified, human review required (no auto-success)"
		}
		if err := s.Transition(ctx, r.id, task.Indeterminate, reason, nil); err != nil {
			return 0, err
		}
	}
	return len(recs), nil
}

// --- helpers ---

type constraintsDTO struct {
	TimeoutNS     int64  `json:"timeout_ns"`
	MaxIterations int    `json:"max_iterations"`
	WorkDir       string `json:"work_dir"`
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertEvent(ctx context.Context, tx *sql.Tx, ev task.Event) error {
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (task_id, run_id, at, kind, from_state, to_state, reason, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.TaskID, ev.RunID, at.UTC().Format(timeFormat), string(ev.Kind),
		string(ev.From), string(ev.To), ev.Reason, string(ev.Detail))
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}
