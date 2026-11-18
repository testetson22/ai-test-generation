package httpapi

import (
	"encoding/json"
	"net/http"
)

type Dependencies struct {
	Postgres      bool
	ArtifactStore bool
	Provider      bool
	Execution     *ExecutionAPI
}

func NewHandler(dependencies Dependencies) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", live)
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		ready(w, dependencies)
	})
	if dependencies.Execution != nil {
		dependencies.Execution.Register(mux)
	} else {
		mux.HandleFunc("POST /execution-pairs", unavailable)
		mux.HandleFunc("GET /execution-pairs/{pairID}", unavailable)
		mux.HandleFunc("GET /executions/{taskID}", unavailable)
		mux.HandleFunc("POST /execution-pairs/{pairID}/cancel", unavailable)
	}
	return mux
}

func unavailable(w http.ResponseWriter, _ *http.Request) {
	writeAPIError(w, http.StatusServiceUnavailable, "execution_unconfigured", "execution API dependencies are not configured")
}

func live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func ready(w http.ResponseWriter, dependencies Dependencies) {
	checks := map[string]string{
		"postgres":       dependencyStatus(dependencies.Postgres),
		"artifact_store": dependencyStatus(dependencies.ArtifactStore),
		"provider":       dependencyStatus(dependencies.Provider),
		"execution_api":  dependencyStatus(dependencies.Execution.Ready()),
	}
	status := http.StatusOK
	state := "ready"
	for _, result := range checks {
		if result != "ready" {
			status = http.StatusServiceUnavailable
			state = "not_ready"
			break
		}
	}
	writeJSON(w, status, struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}{Status: state, Checks: checks})
}

func dependencyStatus(ready bool) string {
	if ready {
		return "ready"
	}
	return "unconfigured"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
