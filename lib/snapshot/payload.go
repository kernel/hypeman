package snapshot

import (
	"errors"
	"fmt"
	"os"

	"github.com/kernel/hypeman/lib/forkvm"
)

func CopyPayload(srcDir, dstDir, operation string) error {
	if err := forkvm.CopyGuestDirectory(srcDir, dstDir); err != nil {
		if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
			return fmt.Errorf("%s requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", operation, err)
		}
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func ReplacePayload(srcDir, dstDir, operation string) error {
	if err := os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("clear destination directory: %w", err)
	}
	return CopyPayload(srcDir, dstDir, operation)
}
