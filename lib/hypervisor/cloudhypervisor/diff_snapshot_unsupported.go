//go:build !linux

package cloudhypervisor

import "errors"

func mergeCloudHypervisorDiff(string) (diffMergeStats, error) {
	return diffMergeStats{}, errors.New("cloud hypervisor diff snapshots require Linux")
}
