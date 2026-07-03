package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/eruca/workflowkit"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const SchemaVersion = 2

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Save(ctx context.Context, run workflowkit.WorkflowRun) error {
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now()
	}
	run.UpdatedAt = time.Now()
	row, err := encodeRun(run)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, status, input_ref, output_ref, agent_run_id, audit_ref, error,
	approval_ref, waiting_reason, current_step, completed_steps_json,
	step_attempts_json, step_records_json, metadata_json, lease_owner, lease_until,
	created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	status=excluded.status,
	input_ref=excluded.input_ref,
	output_ref=excluded.output_ref,
	agent_run_id=excluded.agent_run_id,
	audit_ref=excluded.audit_ref,
	error=excluded.error,
	approval_ref=excluded.approval_ref,
	waiting_reason=excluded.waiting_reason,
	current_step=excluded.current_step,
	completed_steps_json=excluded.completed_steps_json,
	step_attempts_json=excluded.step_attempts_json,
	step_records_json=excluded.step_records_json,
	metadata_json=excluded.metadata_json,
	lease_owner=excluded.lease_owner,
	lease_until=excluded.lease_until,
	created_at=excluded.created_at,
	updated_at=excluded.updated_at
`, row.ID, row.Status, row.InputRef, row.OutputRef, row.AgentRunID, row.AuditRef, row.Error,
		row.ApprovalRef, row.WaitingReason, row.CurrentStep, row.CompletedStepsJSON,
		row.StepAttemptsJSON, row.StepRecordsJSON, row.MetadataJSON, row.LeaseOwner, row.LeaseUntil,
		row.CreatedAt, row.UpdatedAt)
	return err
}

func (s *Store) Get(ctx context.Context, id string) (workflowkit.WorkflowRun, error) {
	row := runRow{}
	err := s.db.QueryRowContext(ctx, `
SELECT id, status, input_ref, output_ref, agent_run_id, audit_ref, error,
	approval_ref, waiting_reason, current_step, completed_steps_json,
	step_attempts_json, step_records_json, metadata_json, lease_owner, lease_until,
	created_at, updated_at
FROM workflow_runs
WHERE id = ?
`, id).Scan(&row.ID, &row.Status, &row.InputRef, &row.OutputRef, &row.AgentRunID, &row.AuditRef, &row.Error,
		&row.ApprovalRef, &row.WaitingReason, &row.CurrentStep, &row.CompletedStepsJSON,
		&row.StepAttemptsJSON, &row.StepRecordsJSON, &row.MetadataJSON, &row.LeaseOwner, &row.LeaseUntil,
		&row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return workflowkit.WorkflowRun{}, workflowkit.ErrRunNotFound
	}
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return decodeRun(row)
}

func (s *Store) Update(ctx context.Context, id string, mutate func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error)) (workflowkit.WorkflowRun, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	updated, err := mutate(current)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if updated.CreatedAt.IsZero() {
		updated.CreatedAt = current.CreatedAt
	}
	if err := s.Save(ctx, updated); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return s.Get(ctx, updated.ID)
}

func (s *Store) ListWorkflows(ctx context.Context, query workflowkit.WorkflowQuery) ([]workflowkit.WorkflowRun, error) {
	if query.Status != "" && !query.Status.IsValid() {
		return nil, fmt.Errorf("invalid workflow status: %s", query.Status)
	}
	if !query.Order.IsValid() {
		return nil, fmt.Errorf("invalid workflow order: %s", query.Order)
	}
	order := "ASC"
	if query.Order == workflowkit.WorkflowOrderDesc {
		order = "DESC"
	}

	var rows *sql.Rows
	var err error
	if query.Status == "" {
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, status, input_ref, output_ref, agent_run_id, audit_ref, error,
	approval_ref, waiting_reason, current_step, completed_steps_json,
	step_attempts_json, step_records_json, metadata_json, lease_owner, lease_until,
	created_at, updated_at
FROM workflow_runs
ORDER BY created_at %s, id %s
`, order, order))
	} else {
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, status, input_ref, output_ref, agent_run_id, audit_ref, error,
	approval_ref, waiting_reason, current_step, completed_steps_json,
	step_attempts_json, step_records_json, metadata_json, lease_owner, lease_until,
	created_at, updated_at
FROM workflow_runs
WHERE status = ?
ORDER BY created_at %s, id %s
`, order, order), string(query.Status))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []workflowkit.WorkflowRun{}
	for rows.Next() {
		row := runRow{}
		if err := rows.Scan(&row.ID, &row.Status, &row.InputRef, &row.OutputRef, &row.AgentRunID, &row.AuditRef, &row.Error,
			&row.ApprovalRef, &row.WaitingReason, &row.CurrentStep, &row.CompletedStepsJSON,
			&row.StepAttemptsJSON, &row.StepRecordsJSON, &row.MetadataJSON, &row.LeaseOwner, &row.LeaseUntil,
			&row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		run, err := decodeRun(row)
		if err != nil {
			return nil, err
		}
		if !workflowMetadataMatches(run, query.MetadataEquals) {
			continue
		}
		runs = append(runs, run)
		if query.Limit > 0 && len(runs) >= query.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return runs, nil
}

func workflowMetadataMatches(run workflowkit.WorkflowRun, equals map[string]string) bool {
	for key, want := range equals {
		got, ok := run.Metadata[key]
		if !ok {
			return false
		}
		if fmt.Sprint(got) != want {
			return false
		}
	}
	return true
}

func (s *Store) ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (workflowkit.WorkflowRun, error) {
	if workerID == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		return workflowkit.WorkflowRun{}, fmt.Errorf("lease must be greater than zero")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	var id string
	err = tx.QueryRowContext(ctx, `
SELECT id
FROM workflow_runs
WHERE status = ?
  AND (lease_owner = '' OR lease_until <= ?)
ORDER BY created_at ASC, id ASC
LIMIT 1
`, string(workflowkit.StatusPending), now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return workflowkit.WorkflowRun{}, workflowkit.ErrNoRunnableWorkflow
	}
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}

	result, err := tx.ExecContext(ctx, `
UPDATE workflow_runs
SET lease_owner = ?, lease_until = ?, updated_at = ?
WHERE id = ?
  AND status = ?
  AND (lease_owner = '' OR lease_until <= ?)
`, workerID, leaseUntil, now, id, string(workflowkit.StatusPending), now)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if affected == 0 {
		return workflowkit.WorkflowRun{}, workflowkit.ErrNoRunnableWorkflow
	}
	if err := tx.Commit(); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return s.Get(ctx, id)
}

func (s *Store) ExtendLease(ctx context.Context, id string, workerID string, lease time.Duration) (workflowkit.WorkflowRun, error) {
	if id == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("workflow id is required")
	}
	if workerID == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		return workflowkit.WorkflowRun{}, fmt.Errorf("lease must be greater than zero")
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	result, err := s.db.ExecContext(ctx, `
UPDATE workflow_runs
SET lease_until = ?, updated_at = ?
WHERE id = ?
  AND lease_owner = ?
  AND lease_until > ?
`, leaseUntil, now, id, workerID, now)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if affected == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return workflowkit.WorkflowRun{}, err
		}
		return workflowkit.WorkflowRun{}, workflowkit.ErrWorkflowLeaseNotOwned
	}
	return s.Get(ctx, id)
}

func (s *Store) ReleaseLease(ctx context.Context, id string, workerID string) (workflowkit.WorkflowRun, error) {
	if id == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("workflow id is required")
	}
	if workerID == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE workflow_runs
SET lease_owner = '', lease_until = ?, updated_at = ?
WHERE id = ?
  AND lease_owner = ?
`, time.Time{}, now, id, workerID)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if affected == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return workflowkit.WorkflowRun{}, err
		}
		return workflowkit.WorkflowRun{}, workflowkit.ErrWorkflowLeaseNotOwned
	}
	return s.Get(ctx, id)
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS workflowkit_schema (
	id TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

INSERT INTO workflowkit_schema (id, version, updated_at)
VALUES ('sqlitestore', ?, CURRENT_TIMESTAMP)
ON CONFLICT(id) DO UPDATE SET
	version=excluded.version,
	updated_at=excluded.updated_at;

CREATE TABLE IF NOT EXISTS workflow_runs (
	id TEXT PRIMARY KEY,
	status TEXT NOT NULL,
	input_ref TEXT NOT NULL DEFAULT '',
	output_ref TEXT NOT NULL DEFAULT '',
	agent_run_id TEXT NOT NULL DEFAULT '',
	audit_ref TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	approval_ref TEXT NOT NULL DEFAULT '',
	waiting_reason TEXT NOT NULL DEFAULT '',
	current_step TEXT NOT NULL DEFAULT '',
	completed_steps_json TEXT NOT NULL DEFAULT '[]',
	step_attempts_json TEXT NOT NULL DEFAULT '{}',
	step_records_json TEXT NOT NULL DEFAULT '[]',
	metadata_json TEXT NOT NULL DEFAULT '{}',
	lease_owner TEXT NOT NULL DEFAULT '',
	lease_until TIMESTAMP NOT NULL DEFAULT '0001-01-01T00:00:00Z',
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
)`, SchemaVersion)
	if err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "lease_owner", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return s.addColumnIfMissing(ctx, "lease_until", "TIMESTAMP NOT NULL DEFAULT '0001-01-01T00:00:00Z'")
}

func (s *Store) addColumnIfMissing(ctx context.Context, name string, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(workflow_runs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var columnName string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if columnName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE workflow_runs ADD COLUMN %s %s`, name, definition))
	return err
}

type runRow struct {
	ID                 string
	Status             string
	InputRef           string
	OutputRef          string
	AgentRunID         string
	AuditRef           string
	Error              string
	ApprovalRef        string
	WaitingReason      string
	CurrentStep        string
	CompletedStepsJSON string
	StepAttemptsJSON   string
	StepRecordsJSON    string
	MetadataJSON       string
	LeaseOwner         string
	LeaseUntil         time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func encodeRun(run workflowkit.WorkflowRun) (runRow, error) {
	completedSteps, err := marshalJSON(run.CompletedSteps)
	if err != nil {
		return runRow{}, err
	}
	stepAttempts, err := marshalJSON(run.StepAttempts)
	if err != nil {
		return runRow{}, err
	}
	stepRecords, err := marshalJSON(run.StepRecords)
	if err != nil {
		return runRow{}, err
	}
	metadata, err := marshalJSON(run.Metadata)
	if err != nil {
		return runRow{}, err
	}
	return runRow{
		ID:                 run.ID,
		Status:             string(run.Status),
		InputRef:           run.InputRef,
		OutputRef:          run.OutputRef,
		AgentRunID:         run.AgentRunID,
		AuditRef:           run.AuditRef,
		Error:              run.Error,
		ApprovalRef:        run.ApprovalRef,
		WaitingReason:      run.WaitingReason,
		CurrentStep:        run.CurrentStep,
		CompletedStepsJSON: completedSteps,
		StepAttemptsJSON:   stepAttempts,
		StepRecordsJSON:    stepRecords,
		MetadataJSON:       metadata,
		LeaseOwner:         run.LeaseOwner,
		LeaseUntil:         run.LeaseUntil,
		CreatedAt:          run.CreatedAt,
		UpdatedAt:          run.UpdatedAt,
	}, nil
}

func decodeRun(row runRow) (workflowkit.WorkflowRun, error) {
	var completedSteps []string
	if err := unmarshalJSON(row.CompletedStepsJSON, &completedSteps); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	var stepAttempts map[string]int
	if err := unmarshalJSON(row.StepAttemptsJSON, &stepAttempts); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	var stepRecords []workflowkit.StepRecord
	if err := unmarshalJSON(row.StepRecordsJSON, &stepRecords); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	var metadata map[string]any
	if err := unmarshalJSON(row.MetadataJSON, &metadata); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return workflowkit.WorkflowRun{
		ID:             row.ID,
		Status:         workflowkit.Status(row.Status),
		InputRef:       row.InputRef,
		OutputRef:      row.OutputRef,
		AgentRunID:     row.AgentRunID,
		AuditRef:       row.AuditRef,
		Error:          row.Error,
		ApprovalRef:    row.ApprovalRef,
		WaitingReason:  row.WaitingReason,
		CurrentStep:    row.CurrentStep,
		CompletedSteps: completedSteps,
		StepAttempts:   stepAttempts,
		StepRecords:    stepRecords,
		Metadata:       metadata,
		LeaseOwner:     row.LeaseOwner,
		LeaseUntil:     row.LeaseUntil,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}

func marshalJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal workflow run field: %w", err)
	}
	return string(data), nil
}

func unmarshalJSON[T any](data string, out *T) error {
	if data == "" {
		data = "null"
	}
	if err := json.Unmarshal([]byte(data), out); err != nil {
		return fmt.Errorf("unmarshal workflow run field: %w", err)
	}
	return nil
}
