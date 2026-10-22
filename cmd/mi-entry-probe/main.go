package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/micah123321/mi-node/internal/entryprobe"
)

func run(input io.Reader, output, stderr io.Writer) int {
	cfg, err := entryprobe.Decode(input)
	if err != nil {
		fmt.Fprintln(stderr, "invalid probe configuration")
		return 1
	}
	result, err := entryprobe.Probe(context.Background(), cfg)
	if err != nil {
		fmt.Fprintln(stderr, "probe failed")
		return 1
	}
	if json.NewEncoder(output).Encode(result) != nil {
		fmt.Fprintln(stderr, "cannot write probe result")
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}
