package main

import (
	"fmt"
	"os"

	"github.com/hjhsamuel/cagent/pkg/localtool"
)

func main() {
	if err := localtool.Serve(os.Stdin, os.Stdout, Handle); err != nil {
		fmt.Fprintln(os.Stderr, "read_skill invocation failed")
		os.Exit(1)
	}
}
