package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/eruca/goagents/memorykit"
)

type memoryWorkerStatusResponse struct {
	Backlog            int64                      `json:"backlog"`
	SuccessTotal       uint64                     `json:"success_total"`
	FailureTotal       uint64                     `json:"failure_total"`
	LastErrorClass     memorykit.WorkerErrorClass `json:"last_error_class,omitempty"`
	LastTransitionTime *time.Time                 `json:"last_transition_time,omitempty"`
}

type memoryWorkersResponse struct {
	Embedding  memoryWorkerStatusResponse `json:"embedding"`
	Extraction memoryWorkerStatusResponse `json:"extraction"`
}

// handleMemoryWorkers returns the host-wide, content-free status of the
// embedding and extraction workers. The workers serve every tenant, so the
// snapshot is not scoped; the caller authorizes like the other memory routes,
// with memory.review on the project named by project_id.
func (s *Server) handleMemoryWorkers(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if _, _, ok := s.authorizeWorkflowMemory(w, r, projectID, memoryReview); !ok {
		return
	}
	embedding, err := s.memory.EmbeddingWorker.Snapshot(r.Context())
	if err != nil {
		writeMemoryError(w, http.StatusServiceUnavailable, "memory_unavailable", "project memory is temporarily unavailable")
		return
	}
	extraction, err := s.memory.ExtractionWorker.Snapshot(r.Context())
	if err != nil {
		writeMemoryError(w, http.StatusServiceUnavailable, "memory_unavailable", "project memory is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, memoryWorkersResponse{
		Embedding:  memoryWorkerStatusToResponse(embedding),
		Extraction: memoryWorkerStatusToResponse(extraction),
	})
}

func memoryWorkerStatusToResponse(snapshot memorykit.WorkerSnapshot) memoryWorkerStatusResponse {
	response := memoryWorkerStatusResponse{
		Backlog:        snapshot.Backlog,
		SuccessTotal:   snapshot.SuccessTotal,
		FailureTotal:   snapshot.FailureTotal,
		LastErrorClass: snapshot.LastErrorClass,
	}
	if !snapshot.LastTransitionTime.IsZero() {
		transition := snapshot.LastTransitionTime.UTC()
		response.LastTransitionTime = &transition
	}
	return response
}
