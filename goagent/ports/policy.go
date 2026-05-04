package ports

import "encoding/json"

type Permission string

const (
	PermissionRead  Permission = "read"
	PermissionWrite Permission = "write"
	PermissionExec  Permission = "exec"
)

type PolicyDecision struct {
	Allowed bool
	Reason  string
}

type PolicyContext struct {
	TenantID  string
	RequestID string
	TraceID   string
	Labels    map[string]string
}

type PolicyRequest struct {
	RunID      string
	UserID     string
	SessionID  string
	Tool       string
	Permission Permission
	Input      json.RawMessage
	Allowed    []Permission
	Context    PolicyContext
	Metadata   map[string]any
}

type PolicyEngine interface {
	Decide(req PolicyRequest) PolicyDecision
}
