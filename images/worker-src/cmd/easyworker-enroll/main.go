// Command easyworker-enroll performs the one-time exclusive claim against an
// unclaimed easyworker and prints the worker-issued bearer token. Operator use
// for externally-registered sandboxes / host runners:
//
//	easyworker-enroll --addr http://host:8080 --code <code> [--owner me]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/easylab-platform/easyworker/client"
)

func main() {
	addr := flag.String("addr", "", "worker base URL (e.g. http://127.0.0.1:8080)")
	code := flag.String("code", "", "one-time enrollment code printed by the worker at startup")
	owner := flag.String("owner", "", "optional caller identity recorded for audit")
	status := flag.Bool("status", false, "only print enrollment status")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "usage: easyworker-enroll --addr URL (--code CODE | --status)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if *status {
		st, err := client.Status(ctx, *addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			os.Exit(1)
		}
		fmt.Printf("claimed=%v needs_code=%v preauthorized=%v boot_id=%s\n",
			st.Claimed, st.NeedsCode, st.Preauthorized, st.BootId)
		return
	}
	if *code == "" {
		fmt.Fprintln(os.Stderr, "--code required (or use --status)")
		os.Exit(2)
	}
	tok, err := client.Enroll(ctx, *addr, *code, *owner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
