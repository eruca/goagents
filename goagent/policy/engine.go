package policy

import "slices"

type Engine struct{}

func NewEngine() *Engine {
	return &Engine{}
}

func (e *Engine) Decide(req Request) Decision {
	if req.Permission == PermissionRead {
		return Decision{Allowed: true, Reason: "read allowed"}
	}
	if slices.Contains(req.Allowed, req.Permission) {
		return Decision{Allowed: true, Reason: "permission allowed by request"}
	}
	return Decision{Allowed: false, Reason: "permission denied by default"}
}
