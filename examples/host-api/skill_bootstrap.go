package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eruca/skillkit"
)

const (
	hostAPISkillRootEnv  = "HOST_API_SKILL_ROOT"
	localUserSkillRootID = "local-user-skills"
)

func loadHostSkillConfig(getenv func(string) string) (*skillkit.Catalog, skillkit.GateContext, error) {
	root := strings.TrimSpace(getenv(hostAPISkillRootEnv))
	if root == "" {
		return nil, skillkit.GateContext{}, nil
	}
	if !filepath.IsAbs(root) {
		return nil, skillkit.GateContext{}, fmt.Errorf("%s must be an absolute directory", hostAPISkillRootEnv)
	}
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID:      localUserSkillRootID,
		Dir:     root,
		Scope:   skillkit.ScopeUser,
		Trusted: true,
		Enabled: true,
	}})
	if err != nil {
		return nil, skillkit.GateContext{}, fmt.Errorf("load %s: %w", hostAPISkillRootEnv, err)
	}
	return catalog, skillkit.GateContext{OS: runtime.GOOS}, nil
}
