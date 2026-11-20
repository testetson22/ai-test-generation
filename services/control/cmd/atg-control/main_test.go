package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgensim"
)

func TestServerConfigurationKeepsDefaultModeUnconfigured(t *testing.T) {
	handler, address, err := serverConfiguration("", "")
	if err != nil {
		t.Fatal(err)
	}
	if address != ":8080" {
		t.Fatalf("default address = %q, want :8080", address)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/development/simulations/test-generation", strings.NewReader(`{"scenario":"success"}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("simulation route in default mode = %d, want 404", response.Code)
	}
}

func TestOfflineSimulationConfigurationIsLoopbackOnlyAndFailsClosed(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		if _, _, err := serverConfiguration("offline-simulation", address); err != nil {
			t.Errorf("serverConfiguration rejected loopback address %q: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:8080", ":8080", "localhost:8080", "127.0.0.1:0", "127.0.0.1:65536"} {
		if _, _, err := serverConfiguration("offline-simulation", address); err == nil {
			t.Errorf("serverConfiguration accepted non-loopback or invalid address %q", address)
		}
	}
	if _, _, err := serverConfiguration("production", "127.0.0.1:8080"); err == nil {
		t.Fatal("serverConfiguration accepted an unavailable production mode")
	}

	handler, address, err := serverConfiguration("offline-simulation", "")
	if err != nil {
		t.Fatal(err)
	}
	if address != "127.0.0.1:8080" {
		t.Fatalf("offline default address = %q, want loopback", address)
	}

	for _, scenario := range []testgensim.Scenario{
		testgensim.ScenarioSuccess,
		testgensim.ScenarioRetry,
		testgensim.ScenarioNoChanges,
		testgensim.ScenarioUnknownResult,
	} {
		request := httptest.NewRequest(http.MethodPost, "/development/simulations/test-generation", bytes.NewBufferString(`{"scenario":"`+string(scenario)+`"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s simulation status = %d, body %s", scenario, response.Code, response.Body.String())
		}
		var report testgensim.Report
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode %s simulation report: %v", scenario, err)
		}
		if !report.SimulationOnly || report.Scenario != scenario || report.Profile.ExternalEffects {
			t.Fatalf("%s simulation response is not explicitly offline: %#v", scenario, report)
		}
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline readiness = %d, want 503", ready.Code)
	}
	execution := httptest.NewRecorder()
	handler.ServeHTTP(execution, httptest.NewRequest(http.MethodPost, "/execution-pairs", nil))
	if execution.Code != http.StatusServiceUnavailable {
		t.Fatalf("execution route in offline mode = %d, want 503", execution.Code)
	}
}
