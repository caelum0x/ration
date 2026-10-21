package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caelum0x/ration/services/audit-log/internal/store"
)

func TestResolvePath_PrefersExplicitArg(t *testing.T) {
	t.Setenv("RATION_AUDIT_LOG_DATA_DIR", "/should/not/be/used")
	got := resolvePath([]string{"/explicit/audit.log"})
	if got != "/explicit/audit.log" {
		t.Errorf("resolvePath() = %q, want /explicit/audit.log", got)
	}
}

func TestResolvePath_FallsBackToDataDirEnv(t *testing.T) {
	t.Setenv("RATION_AUDIT_LOG_DATA_DIR", "/var/ration/audit")
	got := resolvePath(nil)
	want := filepath.Join("/var/ration/audit", "audit.log")
	if got != want {
		t.Errorf("resolvePath() = %q, want %q", got, want)
	}
}

func TestResolvePath_DefaultsToDotDataWhenEnvUnset(t *testing.T) {
	t.Setenv("RATION_AUDIT_LOG_DATA_DIR", "")
	got := resolvePath(nil)
	want := filepath.Join("./data", "audit.log")
	if got != want {
		t.Errorf("resolvePath() = %q, want %q", got, want)
	}
}

func TestRun_IntactChainExitsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Append(store.AppendInput{SourceService: "ledger-core", Kind: store.SourceKindLedgerEntry, RefID: "entry-1", PayloadJSON: "{}"}, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stdout, stderr := newCapture(t), newCapture(t)
	code := run([]string{path}, stdout, stderr)
	if code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}
}

func TestRun_MissingFileExitsNonZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.log")
	stdout, stderr := newCapture(t), newCapture(t)
	code := run([]string{path}, stdout, stderr)
	if code == 0 {
		t.Fatalf("run() exit code = 0, want nonzero for a missing file")
	}
}

func TestRun_TamperedChainExitsNonZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Append(store.AppendInput{SourceService: "ledger-core", Kind: store.SourceKindLedgerEntry, RefID: "entry-1", PayloadJSON: "{}"}, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	tampered := []byte(string(raw[:len(raw)-2]) + "XX\n") // corrupt the trailing JSON
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}

	stdout, stderr := newCapture(t), newCapture(t)
	code := run([]string{path}, stdout, stderr)
	if code == 0 {
		t.Fatalf("run() exit code = 0, want nonzero for a tampered chain")
	}
}

// newCapture returns a writable temp *os.File standing in for
// os.Stdout/os.Stderr in run()'s signature, so tests don't pollute the
// real test runner's stdout/stderr.
func newCapture(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "capture-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
