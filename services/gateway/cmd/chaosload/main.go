// Command chaosload is the load generator + post-mortem verifier for
// scripts/chaos/kill-ledger-mid-write.sh (docs/SCALE-EXPANSION.md section
// 5's chaos test). It is NOT part of any production deployable — it
// exists purely to drive ApplyEntry traffic against a running ledger-core
// while the shell script SIGKILLs and restarts the process out from
// under it, then to prove via ListEntries/GetBalance that nothing the
// load generator believed was applied was actually lost, and nothing
// ledger-core's own WAL-replay-into-SQLite recovery path double-applied.
//
// Subcommands:
//
//	chaosload ping   --addr HOST:PORT
//	chaosload setup  --addr HOST:PORT --reserve-floor N --fund N
//	chaosload load   --addr HOST:PORT --tenant ID --duration 30s --concurrency 8 --log PATH
//	chaosload verify --addr HOST:PORT --tenant ID --log PATH --reserve-floor N
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "chaosload: usage: chaosload <ping|setup|load|verify> [flags]")
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "ping":
		err = runPing(args)
	case "setup":
		err = runSetup(args)
	case "load":
		err = runLoad(args)
	case "verify":
		err = runVerify(args)
	default:
		fmt.Fprintf(os.Stderr, "chaosload: unknown subcommand %q\n", cmd)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "chaosload %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
