package cmd

import (
	"os"
	"testing"
)

func TestStreamOutput(t *testing.T) {
	t.Setenv("PAGER", "cat")
	err := streamOutput("stream test", func(w *os.File) error {
		_, writeErr := w.WriteString("Hello world\n")
		return writeErr
	})
	if err != nil {
		t.Errorf("streamOutput failed: %v", err)
	}
}
