//go:build e2e

// The client actor is a real, independent HTTP client process for the
// e2e lane. It never shares a process with the orchestrator or with
// Statute: the harness starts one container per client identity and
// drives it through subcommand modes — readiness waiting, negative
// probing, plan execution, streaming and upgrade checks, and fetching a
// CA root from inside the network.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: client <get|wait|probe-negative|fetch-roots|run|stream|upgrade> [flags]")
	}
	var err error
	switch mode := os.Args[1]; mode {
	case "get":
		err = runGet(os.Args[2:])
	case "wait":
		err = runWait(os.Args[2:])
	case "probe-negative":
		err = runProbeNegative(os.Args[2:])
	case "fetch-roots":
		err = runFetchRoots(os.Args[2:])
	case "run":
		err = runPlan(os.Args[2:])
	case "stream":
		err = runStream(os.Args[2:])
	case "upgrade":
		err = runUpgrade(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "client: "+msg)
	os.Exit(1)
}
