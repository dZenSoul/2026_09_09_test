package main

import (
	"os"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret")
	t.Setenv("STACKTEST_BASE_URL", "http://127.0.0.1:9090/")
	cfg, err := parseConfig([]string{"-ready-timeout=2s", "-request-timeout=3s"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.baseURL != "http://127.0.0.1:9090" || cfg.adminToken != "secret" || cfg.readyTimeout != 2*time.Second || cfg.requestTimeout != 3*time.Second {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestParseConfigRejectsUnsafeInputs(t *testing.T) {
	old, present := os.LookupEnv("ADMIN_TOKEN")
	_ = os.Unsetenv("ADMIN_TOKEN")
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("ADMIN_TOKEN", old)
		}
	})
	for _, args := range [][]string{
		{},
		{"-admin-token=x", "-base-url=localhost:8080"},
		{"-admin-token=x", "-base-url=http://localhost:8080?token=x"},
		{"-admin-token=x", "-ready-timeout=0s"},
	} {
		if _, err := parseConfig(args); err == nil {
			t.Fatalf("parseConfig(%q) unexpectedly succeeded", args)
		}
	}
}

func TestHelpers(t *testing.T) {
	if !jsonEqual([]byte(`{"b":2,"a":1}`), []byte(`{"a":1,"b":2}`)) {
		t.Fatal("equivalent JSON was not recognized")
	}
	if jsonEqual([]byte(`{"a":1}`), []byte(`{"a":2}`)) {
		t.Fatal("different JSON was recognized as equal")
	}
	items := []listItem{{ID: "a"}, {ID: "b"}}
	if !containsIDs(items, "b", "a") || containsIDs(items, "c") {
		t.Fatal("containsIDs returned an unexpected result")
	}
}
