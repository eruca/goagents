package policy

import "github.com/eruca/goagents/goagent/ports"

type Permission = ports.Permission

const (
	PermissionRead  = ports.PermissionRead
	PermissionWrite = ports.PermissionWrite
	PermissionExec  = ports.PermissionExec
)

type Decision = ports.PolicyDecision

type Request = ports.PolicyRequest
