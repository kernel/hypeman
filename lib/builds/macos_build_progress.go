package builds

import (
	"context"
	"errors"
	"fmt"
)

type machineProgressKey struct{}
type machineProgress struct {
	phase       machinePhase
	step, total int
}
type machinePhase int

const (
	machineStarting machinePhase = iota
	machineProvisioning
	machineSanitizing
	machineStopping
	machineExporting
	machinePublishing
	machinePublished
	machineFailed
	machineStepStarting
	machineStepCompleted
)

// Only host-authored messages and bounded step indexes enter the shared log/SSE
// path. Never include recipe commands, env, guest stdout/stderr or error text.
// At most 64 step messages are emitted per attempt; lifecycle messages are fixed.
func (p machineProgress) line() string {
	switch p.phase {
	case machineStarting:
		return "machine build: starting builder"
	case machineProvisioning:
		return "machine build: provisioning"
	case machineSanitizing:
		return "machine build: sanitizing"
	case machineStopping:
		return "machine build: stopping builder"
	case machineExporting:
		return "machine build: exporting"
	case machinePublishing:
		return "machine build: publishing"
	case machinePublished:
		return "machine build: publication verified"
	case machineFailed:
		return "machine build: failed"
	case machineStepStarting, machineStepCompleted:
		if p.step < 1 || p.step > p.total || p.total > 32 {
			return ""
		}
		state := "starting"
		if p.phase == machineStepCompleted {
			state = "completed"
		}
		return fmt.Sprintf("machine build: recipe step %d/%d %s", p.step, p.total, state)
	default:
		return ""
	}
}

func reportMachineProgress(ctx context.Context, phase machinePhase) {
	emitMachineProgress(ctx, machineProgress{phase: phase})
}

func emitMachineProgress(ctx context.Context, p machineProgress) {
	if emit, ok := ctx.Value(machineProgressKey{}).(func(machineProgress)); ok && p.line() != "" {
		emit(p)
	}
}

func runMachineRecipe(ctx context.Context, recipe *machineRecipe, exec func(context.Context, machineRecipeStep) error) error {
	for i, step := range recipe.Steps {
		emitMachineProgress(ctx, machineProgress{phase: machineStepStarting, step: i + 1, total: len(recipe.Steps)})
		if err := exec(ctx, step); err != nil {
			if exitErr := (*machineExitError)(nil); errors.As(err, &exitErr) {
				return &machineStepError{step: i + 1, code: exitErr.code}
			}
			return err
		}
		emitMachineProgress(ctx, machineProgress{phase: machineStepCompleted, step: i + 1, total: len(recipe.Steps)})
	}
	return nil
}
