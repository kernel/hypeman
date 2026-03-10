package guestmemory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPolicyKernelArgs(t *testing.T) {
	p := DefaultPolicy()
	assert.Empty(t, p.KernelArgs())

	performance := p
	performance.Enabled = true
	performance.KernelPageInitMode = KernelPageInitPerformance
	assert.Equal(t, []string{"init_on_alloc=0", "init_on_free=0"}, performance.KernelArgs())

	hardened := p
	hardened.Enabled = true
	hardened.KernelPageInitMode = KernelPageInitHardened
	assert.Equal(t, []string{"init_on_alloc=1", "init_on_free=1"}, hardened.KernelArgs())

	disabled := p
	disabled.Enabled = false
	assert.Empty(t, disabled.KernelArgs())
}

func TestFeaturesForHypervisor(t *testing.T) {
	p := DefaultPolicy()
	p.Enabled = true

	f := p.FeaturesForHypervisor()
	assert.True(t, f.EnableBalloon)
	assert.True(t, f.FreePageReporting)
	assert.True(t, f.DeflateOnOOM)
	assert.True(t, f.FreePageHinting)
	assert.True(t, f.RequireBalloon)

	p.ReclaimEnabled = false
	assert.Equal(t, Features{}, p.FeaturesForHypervisor())
}
