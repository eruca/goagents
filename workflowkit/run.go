package workflowkit

import "time"

type Status string

const (
	StatusPending         Status = "pending"
	StatusRunning         Status = "running"
	StatusWaitingApproval Status = "waiting_approval"
	StatusSucceeded       Status = "succeeded"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
)

func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusWaitingApproval, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

type WorkflowRun struct {
	ID             string
	Status         Status
	InputRef       string
	OutputRef      string
	AgentRunID     string
	AuditRef       string
	Error          string
	ApprovalRef    string
	WaitingReason  string
	CurrentStep    string
	CompletedSteps []string
	StepAttempts   map[string]int
	StepRecords    []StepRecord
	Metadata       map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type StepRecord struct {
	Name          string
	Status        Status
	Attempt       int
	OutputRef     string
	AgentRunID    string
	AuditRef      string
	Error         string
	ApprovalRef   string
	WaitingReason string
	StartedAt     time.Time
	EndedAt       time.Time
	Metadata      map[string]any
}

func cloneRun(run WorkflowRun) WorkflowRun {
	run.CompletedSteps = append([]string(nil), run.CompletedSteps...)
	run.StepAttempts = cloneStepAttempts(run.StepAttempts)
	run.StepRecords = cloneStepRecords(run.StepRecords)
	run.Metadata = cloneMetadata(run.Metadata)
	return run
}

func cloneStepRecords(records []StepRecord) []StepRecord {
	if len(records) == 0 {
		return nil
	}
	copied := make([]StepRecord, 0, len(records))
	for _, record := range records {
		record.Metadata = cloneMetadata(record.Metadata)
		copied = append(copied, record)
	}
	return copied
}

func cloneStepAttempts(attempts map[string]int) map[string]int {
	if len(attempts) == 0 {
		return nil
	}
	copied := make(map[string]int, len(attempts))
	for key, value := range attempts {
		copied[key] = value
	}
	return copied
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]any, len(metadata))
	for key, value := range metadata {
		copied[key] = value
	}
	return copied
}
