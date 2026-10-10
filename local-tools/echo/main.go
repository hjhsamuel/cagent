package main

import (
	"fmt"
	"os"

	"github.com/hjhsamuel/cagent/pkg/localtool"
)

func main() {
	err := localtool.Serve(os.Stdin, os.Stdout, func(req localtool.Request) (localtool.Response, error) {
		var args struct {
			Text string `json:"text"`
		}
		if localtool.Decode(req.Arguments, &args) != nil {
			return localtool.Response{Error: "invalid echo arguments"}, nil
		}
		return localtool.Response{Text: args.Text}, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "echo invocation failed")
		os.Exit(1)
	}
}
