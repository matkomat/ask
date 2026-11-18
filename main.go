// Command ask sends a question to a Claude Code agent on another host and
// prints its answer.
//
// The same binary runs both sides:
//
//	ask serve   runs on the answering host and answers each question with a local
//	            headless Claude Code process started in a prepared workspace
//	ask "..."   runs on a person's computer and talks to that server
package main

import (
	"fmt"
	"os"
)

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		err = runServe(os.Args[2:])
	} else {
		err = runAsk(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask:", err)
		os.Exit(1)
	}
}
