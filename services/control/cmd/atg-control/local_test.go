package main

import (
	"context"
	"strings"
	"testing"
)

func TestLocalDevelopmentConfigurationIsLoopbackOnly(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8081", "[::1]:8081"} {
		_, actual, err := serverConfiguration("local-development", address)
		if err != nil || actual != address {
			t.Fatalf("local address %q: actual=%q err=%v", address, actual, err)
		}
	}
	for _, address := range []string{":8081", "0.0.0.0:8081", "localhost:8081", "127.0.0.1:0"} {
		if _, _, err := serverConfiguration("local-development", address); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
}

func TestLocalDevelopmentRequiresExplicitDependenciesBeforeStartup(t *testing.T) {
	settings := map[string]string{
		"ATG_DATABASE_URL":             "postgres://unused",
		"ATG_ARTIFACT_ROOT":            "unused",
		"ATG_LOCAL_PROFILE":            "unused",
		"ATG_DEVELOPMENT_BEARER_TOKEN": strings.Repeat("x", 32),
	}
	for _, key := range []string{"ATG_DATABASE_URL", "ATG_ARTIFACT_ROOT", "ATG_LOCAL_PROFILE", "ATG_DEVELOPMENT_BEARER_TOKEN"} {
		t.Run(key, func(t *testing.T) {
			_, err := newLocalRuntime(context.Background(), func(name string) string {
				if name == key {
					return ""
				}
				return settings[name]
			}, nil)
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("missing %s: %v", key, err)
			}
		})
	}
	settings["ATG_DEVELOPMENT_BEARER_TOKEN"] = "too-short"
	if _, err := newLocalRuntime(context.Background(), func(name string) string { return settings[name] }, nil); err == nil || !strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("weak token: %v", err)
	}
}
