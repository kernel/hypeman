package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

type failedOutputExecStream struct {
	stalledExecStream
	failure error
}

func (s *failedOutputExecStream) Send(*pb.ExecResponse) error { return s.failure }

func TestExecSendFailureCancelsProcessInBothModes(t *testing.T) {
	for _, tty := range []bool{false, true} {
		name := "stream"
		if tty {
			name = "tty"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release := make(chan struct{})
			defer close(release)
			pidFile := filepath.Join(t.TempDir(), "pid")
			failure := errors.New("synthetic send failure")
			stream := &failedOutputExecStream{stalledExecStream: stalledExecStream{ctx: ctx, release: release, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{Tty: tty, Command: []string{"/bin/sh", "-c", `echo $$ > "$1"; printf ready; exec /bin/sleep 30`, "sh", pidFile}}}}}, failure: failure}
			require.Error(t, (&guestServer{}).Exec(stream))
			require.NoError(t, ctx.Err(), "send failure must cancel without waiting for the caller deadline")
			data, err := os.ReadFile(pidFile)
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			require.NoError(t, err)
			require.Eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }, time.Second, 10*time.Millisecond, "failed output must not leave the command alive")
		})
	}
}
