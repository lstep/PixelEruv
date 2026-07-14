package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFlagCache_NoReader_ReturnsNeutral(t *testing.T) {
	fc, err := NewFlagCache("")
	if err != nil {
		t.Fatalf("NewFlagCache: %v", err)
	}
	defer fc.Close()

	if got := fc.Lookup("8.8.8.8"); got != "neutral" {
		t.Errorf("Lookup with no reader = %q, want %q", got, "neutral")
	}
}

func TestFlagCache_EmptyIP_ReturnsNeutral(t *testing.T) {
	fc, _ := NewFlagCache("")
	defer fc.Close()

	if got := fc.Lookup(""); got != "neutral" {
		t.Errorf("Lookup(\"\") = %q, want %q", got, "neutral")
	}
}

func TestFlagCache_InvalidIP_ReturnsNeutral(t *testing.T) {
	fc, _ := NewFlagCache("")
	defer fc.Close()

	if got := fc.Lookup("not-an-ip"); got != "neutral" {
		t.Errorf("Lookup(\"not-an-ip\") = %q, want %q", got, "neutral")
	}
}

func TestFlagCache_CachesResult(t *testing.T) {
	fc, _ := NewFlagCache("")
	defer fc.Close()

	// First call populates the cache.
	first := fc.Lookup("1.2.3.4")
	// Second call must return the same value (from cache, not re-resolved).
	second := fc.Lookup("1.2.3.4")
	if first != second {
		t.Errorf("cached result mismatch: first=%q second=%q", first, second)
	}
}

func TestFlagCache_NonExistentDB_ReturnsNeutral(t *testing.T) {
	fc, err := NewFlagCache("/nonexistent/path/to/mmdb")
	if err != nil {
		t.Fatalf("NewFlagCache with nonexistent path: %v", err)
	}
	defer fc.Close()

	if fc.reader != nil {
		t.Error("expected nil reader for nonexistent DB path")
	}
	if got := fc.Lookup("8.8.8.8"); got != "neutral" {
		t.Errorf("Lookup with nonexistent DB = %q, want %q", got, "neutral")
	}
}

func TestFlagCache_WithBundledDB(t *testing.T) {
	// Use absolute path so the test works regardless of working directory.
	dbPath := filepath.Join(t.TempDir(), "test.mmdb")
	src, err := os.Open("data/ip-to-country.mmdb")
	if err != nil {
		t.Skipf("bundled MMDB not found: %v — skipping GeoIP lookup test", err)
	}
	defer src.Close()
	dst, err := os.Create(dbPath)
	if err != nil {
		t.Fatalf("copy MMDB: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("copy MMDB: %v", err)
	}
	dst.Close()

	fc, err := NewFlagCache(dbPath)
	if err != nil {
		t.Fatalf("NewFlagCache: %v", err)
	}
	defer fc.Close()

	if fc.reader == nil {
		t.Fatal("expected non-nil reader for bundled DB")
	}

	// 8.8.8.8 is Google's public DNS, geolocated to the US.
	got := fc.Lookup("8.8.8.8")
	if got == "neutral" {
		t.Log("8.8.8.8 returned neutral — MMDB may not cover this IP")
	}
	if got != "neutral" && len(got) != 2 {
		t.Errorf("Lookup(\"8.8.8.8\") = %q, want a 2-letter country code or neutral", got)
	}
	t.Logf("8.8.8.8 -> %q", got)
}

func TestFlagClassFor_NeutralFallback(t *testing.T) {
	old := flagCache
	flagCache = nil
	defer func() { flagCache = old }()

	if got := flagClassFor(""); got != "flag flag-neutral" {
		t.Errorf("flagClassFor(\"\") = %q, want %q", got, "flag flag-neutral")
	}
}

func TestFlagClassFor_RealCode(t *testing.T) {
	old := flagCache
	flagCache = &FlagCache{cache: map[string]string{"1.2.3.4": "us"}}
	defer func() { flagCache = old }()

	if got := flagClassFor("1.2.3.4"); got != "fi fi-us" {
		t.Errorf("flagClassFor(\"1.2.3.4\") = %q, want %q", got, "fi fi-us")
	}
}
