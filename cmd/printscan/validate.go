package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/config"
)

// validate checks each file with the same check serve uses and prints
// file:line:col: message per problem. It returns 1 on any error.
func validate(files []string, stdout, stderr io.Writer) int {
	if len(files) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: printscan validate <file>...")
		return 2
	}
	known := catalog.Known()
	code := 0
	for _, name := range files {
		b, err := os.ReadFile(name) //nolint:gosec // the user names the files to check
		if err == nil {
			_, err = config.Check(b, known)
		}
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "%s: ok\n", name)
			continue
		}
		code = 1
		var el *config.ErrorList
		if !errors.As(err, &el) {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			continue
		}
		for _, fe := range el.Errors {
			msg := fe.Message
			if fe.Path != "" {
				msg = fe.Path + ": " + msg
			}
			if fe.Line > 0 {
				_, _ = fmt.Fprintf(stderr, "%s:%d:%d: %s\n", name, fe.Line, fe.Column, msg)
			} else {
				_, _ = fmt.Fprintf(stderr, "%s: %s\n", name, msg)
			}
		}
	}
	return code
}
