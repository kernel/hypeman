package builds

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type machineRunnerFixture struct {
	MachineBuildBackend
	WorkDir        string
	MaxSourceBytes int64
}

// Run is test-only: production invokes the prepared machine phase from runBuild.
// Existing failure fixtures still exercise resolution/staging before that phase.
func (r *machineRunnerFixture) Run(ctx context.Context, req MachineBuildRequest, source io.Reader) (*MachineBuildResult, error) {
	if source == nil || !filepath.IsAbs(r.WorkDir) {
		return nil, fmt.Errorf("workspace/source required")
	}
	timeout := req.Policy.TimeoutSeconds
	if timeout == 0 {
		timeout = 600
	}
	if timeout < 0 || timeout > 86400 {
		return nil, fmt.Errorf("invalid timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	base, err := r.Driver.(*machineFixture).ResolveBase(ctx, req.BaseImage)
	if err != nil {
		return nil, machineFailure("resolve_base", err)
	}
	job, err := os.MkdirTemp(r.WorkDir, "prepared-job-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(job)
	limit := r.MaxSourceBytes
	if limit == 0 {
		limit = 64 << 20
	}
	if limit < 1 || limit > 1<<30 {
		return nil, fmt.Errorf("invalid source limit")
	}
	sourcePath := filepath.Join(job, "source.tar.gz")
	hash, err := stageBuildSource(ctx, source, sourcePath, limit)
	if err != nil {
		return nil, machineFailure("stage_source", err)
	}
	if req.SourceHash != "" && req.SourceHash != hash {
		return nil, ErrSourceHashMismatch
	}
	req.SourceHash = hash
	return r.runPrepared(ctx, req, base, sourcePath, r.WorkDir)
}
