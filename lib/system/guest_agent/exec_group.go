package main

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"sync"
	"syscall"
)

// processGroup owns the kill of an exec's process group. The command's pid is the
// group id. While the leader is unreaped, the kernel keeps its pid reserved, so a kill
// reaches only this group. After the leader is reaped, the id can belong to another
// process group, so no kill is sent.
type processGroup struct {
	mu     sync.Mutex
	pid    int
	reaped bool
}

func newProcessGroup(cmd *exec.Cmd) *processGroup {
	return &processGroup{pid: cmd.Process.Pid}
}

// kill sends SIGKILL to the group unless the leader was already reaped.
func (g *processGroup) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reaped {
		return
	}
	if err := syscall.Kill(-g.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("[guest-agent] kill process group %d: %v", g.pid, err)
	}
}

// reap reaps the leader and returns cmd.Wait's error. No kill is sent after it.
func (g *processGroup) reap(cmd *exec.Cmd) error {
	g.mu.Lock()
	g.reaped = true
	g.mu.Unlock()
	return cmd.Wait()
}

// killGroupOnDone kills the group if ctx ends before done closes. done closes when
// the exec lifecycle finishes, so a normal exit leaves background children alone.
func killGroupOnDone(ctx context.Context, grp *processGroup, done <-chan struct{}) {
	select {
	case <-done:
	case <-ctx.Done():
		grp.kill()
	}
}
