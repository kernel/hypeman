package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGPUInitFailureMessage(t *testing.T) {
	msg, ok := gpuInitFailureMessage("3,1042,8462102,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n")
	assert.True(t, ok)
	assert.Equal(t, "NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)", msg)

	// Tuple values are driver-build-specific; the match must not depend on them.
	_, ok = gpuInitFailureMessage("3,1042,8462102,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x26:0xffff:1482)\n")
	assert.True(t, ok)

	for _, record := range []string{
		"6,1041,8462100,-;NVRM: loading NVIDIA UNIX Open Kernel Module for x86_64\n",
		"6,1043,8462110,-;nvidia-gridd: RmInitAdapter failed mentioned in userspace\n",
		"no separator RmInitAdapter failed!\n",
		" continuation line of a multi-line record\n",
	} {
		_, ok := gpuInitFailureMessage(record)
		assert.False(t, ok, "record %q must not match", record)
	}
}

func TestScanKmsgReportsEachFailureRecord(t *testing.T) {
	records := strings.Join([]string{
		"6,1,100,-;booting",
		"3,2,200,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)",
		"6,3,300,-;unrelated",
		"3,4,400,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)",
	}, "\n") + "\n"

	var got []string
	scanKmsg(strings.NewReader(records), func(msg string) { got = append(got, msg) })
	assert.Len(t, got, 2)
}
