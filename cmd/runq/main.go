package main

import (
	"fmt"
	"os"

	"github.com/gliese129/runq/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
