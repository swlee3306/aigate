// Command orch is the client of orchd's local API (unix socket).
package main

import (
	"os"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
