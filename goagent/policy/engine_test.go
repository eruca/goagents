package policy

import "testing"

func TestEngineAllowsReadByDefault(t *testing.T) {
	engine := NewEngine()
	decision := engine.Decide(Request{Permission: PermissionRead})
	if !decision.Allowed {
		t.Fatalf("Allowed = false, reason = %q", decision.Reason)
	}
}

func TestEngineDeniesWriteAndExecByDefaultUnlessAllowed(t *testing.T) {
	engine := NewEngine()
	for _, permission := range []Permission{PermissionWrite, PermissionExec} {
		denied := engine.Decide(Request{Permission: permission})
		if denied.Allowed {
			t.Fatalf("%s permission allowed by default", permission)
		}

		allowed := engine.Decide(Request{
			Permission: permission,
			Allowed:    []Permission{permission},
		})
		if !allowed.Allowed {
			t.Fatalf("%s not allowed by request: %q", permission, allowed.Reason)
		}
	}
}

func TestEngineDeniesEmptyAndUnknownPermissions(t *testing.T) {
	engine := NewEngine()
	for _, permission := range []Permission{"", "unknown"} {
		decision := engine.Decide(Request{Permission: permission})
		if decision.Allowed {
			t.Fatalf("%q permission allowed", permission)
		}
	}
}
