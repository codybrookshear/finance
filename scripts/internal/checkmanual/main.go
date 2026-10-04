// Command checkmanual validates a manual-accounts JSON file the way the sync
// will (used by scripts/prod-config-create.sh).
package main

import (
	"fmt"
	"os"

	"finance/internal/manual"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: checkmanual <manual-accounts.json>")
		os.Exit(2)
	}
	accts, err := manual.Load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d manual account(s) OK\n", len(accts))
}
