package main

import (
	"fmt"
	"os"

	"ledger/internal/app"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger: determine current directory: %v\n", err)
		os.Exit(1)
	}
	os.Exit(app.Run(os.Args[1:], cwd, os.Stdout, os.Stderr))
}
