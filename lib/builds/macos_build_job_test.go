package builds

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"
)

type machineRunnerFixture struct {
	MachineBuildBackend
	WorkDir string
}

func (r *machineRunnerFixture) Run(ctx context.Context, req MachineBuildRequest, source io.Reader) (*MachineBuildResult, error) {
	timeout := req.Policy.TimeoutSeconds
	if timeout == 0 {
		timeout = 600
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	job, err := os.MkdirTemp(r.WorkDir, "prepared-job-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(job)
	sourcePath := filepath.Join(job, "source.tar.gz")
	hash, err := stageBuildSource(ctx, source, sourcePath, maxBuildSourceBytes)
	if err != nil {
		return nil, machineFailure("stage_source", err)
	}
	if req.SourceHash == "" {
		req.SourceHash = hash
	}
	return r.runPrepared(ctx, req, r.Driver.(*machineFixture).image, sourcePath, r.WorkDir)
}
