package main

import (
	"errors"
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

func TestParseLoadConfig(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret")
	cfg, err := parseConfig([]string{
		"-mode=load", "-load-profile=mixed", "-load-duration=5s", "-workers=8",
		"-max-ops=100", "-max-error-rate=0.02", "-max-p95=750ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "load" || cfg.loadProfile != "mixed" || cfg.loadDuration != 5*time.Second ||
		cfg.workers != 8 || cfg.maxOPS != 100 || cfg.maxErrorRate != 0.02 || cfg.maxP95 != 750*time.Millisecond {
		t.Fatalf("unexpected load config: %#v", cfg)
	}
}

func TestDefaultBaseURLUsesComposeIPv4Binding(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret")
	t.Setenv("STACKTEST_BASE_URL", "")
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.baseURL != "http://127.0.0.1:8080" {
		t.Fatalf("default base URL = %q", cfg.baseURL)
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
		{"-admin-token=x", "-mode=other"},
		{"-admin-token=x", "-mode=load", "-load-profile=write"},
		{"-admin-token=x", "-mode=load", "-workers=0"},
		{"-admin-token=x", "-mode=load", "-max-error-rate=1.1"},
	} {
		if _, err := parseConfig(args); err == nil {
			t.Fatalf("parseConfig(%q) unexpectedly succeeded", args)
		}
	}
}

func TestLoadOperationMix(t *testing.T) {
	r := runner{cfg: config{loadProfile: "read"}}
	counts := make(map[string]int)
	for sequence := uint64(0); sequence < 100; sequence++ {
		counts[r.pickLoadOperation(sequence)]++
	}
	if counts["get"] != 70 || counts["head"] != 15 || counts["list"] != 15 || counts["mutation"] != 0 {
		t.Fatalf("unexpected read mix: %#v", counts)
	}

	r.cfg.loadProfile = "mixed"
	counts = make(map[string]int)
	for sequence := uint64(0); sequence < 100; sequence++ {
		counts[r.pickLoadOperation(sequence)]++
	}
	if counts["get"] != 55 || counts["head"] != 15 || counts["list"] != 15 || counts["mutation"] != 15 {
		t.Fatalf("unexpected mixed mix: %#v", counts)
	}
}

func TestLoadStats(t *testing.T) {
	stats := newLoadStats(2 * time.Second)
	for range 9 {
		stats.record("get", 10*time.Millisecond, nil)
	}
	stats.record("get", 3*time.Second, errors.New("boom"))
	result := stats.snapshot()
	if result.total != 10 || result.success != 9 || result.errors != 1 || result.errorRate() != 0.1 || result.p95Duration != 3*time.Second || !result.p95OverLimit {
		t.Fatalf("unexpected load result: %#v", result)
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
