package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLivenessDoesNotDependOnExternalServices(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(Dependencies{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode liveness response: %v", err)
	}
	if body["status"] != "alive" {
		t.Fatalf("status body = %q, want alive", body["status"])
	}
}

func TestReadinessFailsClosedForUnconfiguredDependencies(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(Dependencies{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if body.Status != "not_ready" {
		t.Fatalf("readiness status = %q, want not_ready", body.Status)
	}
	for _, name := range []string{"postgres", "artifact_store", "provider"} {
		if body.Checks[name] != "unconfigured" {
			t.Errorf("check %q = %q, want unconfigured", name, body.Checks[name])
		}
	}
}

func TestReadinessRequiresEveryDependency(t *testing.T) {
	tests := []struct {
		name string
		deps Dependencies
	}{
		{name: "missing postgres", deps: Dependencies{ArtifactStore: true, Provider: true}},
		{name: "missing artifact store", deps: Dependencies{Postgres: true, Provider: true}},
		{name: "missing provider", deps: Dependencies{Postgres: true, ArtifactStore: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			NewHandler(test.deps).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestReadinessSucceedsOnlyWhenEveryDependencyIsReady(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Postgres: true, ArtifactStore: true, Provider: true}).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/health/ready", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}
