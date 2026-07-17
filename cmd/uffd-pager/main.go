package main

import (
	"log/slog"
	"os"

	"github.com/kernel/hypeman/lib/uffdpager"
)

func main() {
	if err := uffdpager.Main(os.Args[1:]); err != nil {
		slog.Error("uffd pager terminated", "error", err)
		os.Exit(1)
	}
}
