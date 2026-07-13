//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart(t *testing.T) {
	provider := newOIDCTestProvider(t)
	binary := buildHostBinary(t)
	runtimeHome := t.TempDir()
	skillRoot, err := filepath.Abs("skills")
	if err != nil {
		t.Fatalf("resolve bundled Skill root: %v", err)
	}
	extraEnvironment := map[string]string{hostAPISkillRootEnv: skillRoot}

	first := startHostProcessWithEnv(t, binary, runtimeHome, provider.issuer, "", "", extraEnvironment)
	listed, status := processJSON[skillListPayload](t, first, http.MethodGet, "/skills", nil, "")
	if status != http.StatusOK || len(listed.Skills) != 1 || listed.Skills[0].Name != "workflow-review" || len(listed.Skills[0].Digest) != 64 {
		t.Fatalf("first GET /skills status=%d skills=%#v", status, listed.Skills)
	}
	digest := listed.Skills[0].Digest
	firstWorkflow, status := processJSON[workflowResponse](t, first, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-process-skill-first",
		"input": "Review the first process workflow.",
		"skill_refs": []map[string]string{{
			"name": "workflow-review",
		}},
	}, "")
	if status != http.StatusAccepted || len(firstWorkflow.SkillRefs) != 1 || firstWorkflow.SkillRefs[0].Digest != digest {
		t.Fatalf("first workflow status=%d refs=%#v, want persisted digest", status, firstWorkflow.SkillRefs)
	}
	stopHostProcess(t, first)

	second := startHostProcessWithEnv(t, binary, runtimeHome, provider.issuer, "", "", extraEnvironment)
	listedAfterRestart, status := processJSON[skillListPayload](t, second, http.MethodGet, "/skills", nil, "")
	if status != http.StatusOK || len(listedAfterRestart.Skills) != 1 || listedAfterRestart.Skills[0].Digest != digest {
		t.Fatalf("second GET /skills status=%d skills=%#v, want stable digest %q", status, listedAfterRestart.Skills, digest)
	}
	secondWorkflow, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-process-skill-second",
		"input": "Review the second process workflow.",
		"skill_refs": []map[string]string{{
			"name":   "workflow-review",
			"digest": digest,
		}},
	}, "")
	if status != http.StatusAccepted || len(secondWorkflow.SkillRefs) != 1 || secondWorkflow.SkillRefs[0].Digest != digest {
		t.Fatalf("second workflow status=%d refs=%#v, want exact digest replay", status, secondWorkflow.SkillRefs)
	}
	stopHostProcess(t, second)
}
