package guestmemory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPolicyKernelArgs(t *testing.T) {
	p := DefaultPolicy()
	assert.Equal(t, []string{"init_on_alloc=0", "init_on_free=0"}, p.KernelArgs())

	hardened := p
	hardened.KernelPageInitMode = KernelPageInitHardened
	assert.Equal(t, []string{"init_on_alloc=1", "init_on_free=1"}, hardened.KernelArgs())

	disabled := p
	disabled.Enabled = false
	assert.Empty(t, disabled.KernelArgs())
}

func TestFeaturesForHypervisor(t *testing.T) {
	f := DefaultPolicy().FeaturesForHypervisor()
	assert.True(t, f.EnableBalloon)
	assert.True(t, f.FreePageReporting)
	assert.True(t, f.DeflateOnOOM)
	assert.True(t, f.FreePageHinting)
	assert.True(t, f.RequireBalloon)

	p := DefaultPolicy()
	p.ReclaimEnabled = false
	assert.Equal(t, Features{}, p.FeaturesForHypervisor())
}
