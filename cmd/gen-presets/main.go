// Command gen-presets prints the role presets JSON (the SYSTEM_PRESETS_FILE
// contents) to stdout, so the deployment ConfigMap can be generated from the
// single source of truth in internal/presets.
package main

import (
	"fmt"
	"os"

	"github.com/abcp-sdk/workspace-gateway/internal/presets"
)

func main() {
	b, err := presets.JSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}
