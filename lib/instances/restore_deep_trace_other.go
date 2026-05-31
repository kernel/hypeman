//go:build !linux

package instances

import (
	"context"
)

type restoreDeepTrace struct{}

func newRestoreDeepTrace(context.Context, *StoredMetadata, int, string) (*restoreDeepTrace, error) {
	return nil, nil
}

func withRestoreDeepTrace(ctx context.Context, _ *restoreDeepTrace) context.Context {
	return ctx
}

func restoreDeepTraceFromContext(context.Context) *restoreDeepTrace {
	return nil
}

func (t *restoreDeepTrace) Mark(string, string) {}
func (t *restoreDeepTrace) Sample(string)       {}
func (t *restoreDeepTrace) Close(string, error) {}
func (t *restoreDeepTrace) Dir() string         { return "" }
