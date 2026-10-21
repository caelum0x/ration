// Command verify-chain is the standalone, independent audit-log
// integrity checker docs/THREAT-MODEL.md's "Known gaps" item 9 asks
// for: "a small standalone verifier (reads
// RATION_AUDIT_LOG_DATA_DIR/audit.log directly, re-derives the chain,
// no dependency on the audit-log binary being trustworthy) for
// operators to run out-of-band — today VerifyChain is only exposed as
// audit-log's own RPC."
//
// This binary has no gRPC client, dials nothing, and never asks the
// live audit-log process anything — it opens the append-only log file
// read-only (internal/store.VerifyFile) and re-derives the hash chain
// itself, so the check is meaningful even if audit-log (or whatever
// compromised it) would lie about VerifyChain's RPC result. Intended
// to be run out-of-band by an operator or a monitoring cron job, not
// imported by any other service.
//
// Usage:
//
//	verify-chain [path/to/audit.log]
//
// path defaults to "$RATION_AUDIT_LOG_DATA_DIR/audit.log" (matching
// audit-log's own cmd/audit-log/main.go convention), falling back to
// "./data/audit.log" if that env var is unset. Exits 0 and prints
// "intact" with the record count when the chain verifies; exits 1 and
// prints the first broken sequence/detail otherwise (or on any error
// reading the file at all), so this is safe to wire into a health
// check or alert on a nonzero exit code.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/caelum0x/ration/services/audit-log/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	path := resolvePath(args)

	result, err := store.VerifyFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "verify-chain: %v\n", err)
		return 1
	}

	if !result.Intact {
		fmt.Fprintf(stderr, "verify-chain: CHAIN BROKEN at sequence %d (checked %d records before it): %s\n",
			result.FirstBrokenSequence, result.RecordsChecked, result.Detail)
		return 1
	}

	fmt.Fprintf(stdout, "verify-chain: intact (%d records checked) — %s\n", result.RecordsChecked, path)
	return 0
}

// resolvePath returns args[0] if given, else
// "$RATION_AUDIT_LOG_DATA_DIR/audit.log" (the same env var and file
// name audit-log's own cmd/audit-log/main.go uses), falling back to
// "./data/audit.log" if that env var is unset — matching
// services/audit-log/internal/config's own default for
// RATION_AUDIT_LOG_DATA_DIR.
func resolvePath(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	dataDir := os.Getenv("RATION_AUDIT_LOG_DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	return filepath.Join(dataDir, "audit.log")
}
