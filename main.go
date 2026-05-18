package main

import (
	"fmt"
	"os"

	"github.com/widdlab/krapow/cmd"
)

func main() {
	if err := cmd.Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "krapow:", err)
		os.Exit(1)
	}
}
