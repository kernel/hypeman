package guestmemory

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

func (c *controller) Start(ctx context.Context) error {
	if !c.policy.Enabled || !c.policy.ReclaimEnabled {
		<-ctx.Done()
		return nil
	}
	if !c.config.Enabled {
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := c.reconcile(ctx, reconcileRequest{}); err != nil {
				logFromContext(ctx, c.log).WarnContext(ctx, "active ballooning reconcile failed", "operation", "active_ballooning_reconcile", "trigger", "auto", "error", err)
			}
		}
	}
}

func (c *controller) TriggerReclaim(ctx context.Context, req ManualReclaimRequest) (ManualReclaimResponse, error) {
	if !c.policy.Enabled || !c.policy.ReclaimEnabled {
		return ManualReclaimResponse{}, ErrGuestMemoryDisabled
	}
	if !c.config.Enabled {
		return ManualReclaimResponse{}, ErrActiveBallooningDisabled
	}
	if req.ReclaimBytes < 0 {
		return ManualReclaimResponse{}, fmt.Errorf("reclaim_bytes must be non-negative")
	}
	return c.reconcile(ctx, reconcileRequest{
		force:            true,
		dryRun:           req.DryRun,
		requestedReclaim: req.ReclaimBytes,
		holdFor:          req.HoldFor,
		reason:           req.Reason,
	})
}

type reconcileRequest struct {
	force            bool
	dryRun           bool
	requestedReclaim int64
	holdFor          time.Duration
	reason           string
}

func (c *controller) reconcile(ctx context.Context, req reconcileRequest) (ManualReclaimResponse, error) {
	trigger := reconcileTrigger(req)
	start := time.Now()
	ctx, span := c.startReconcileSpan(ctx, req)
	defer span.End()

	state := &c.reconcileMu
	select {
	case <-ctx.Done():
		err := ctx.Err()
		c.recordReconcileError(ctx, trigger, start, span, err)
		return ManualReclaimResponse{}, err
	case <-state.mu:
	}
	defer func() { state.mu <- struct{}{} }()

	now := time.Now()
	sampleCtx, sampleSpan := c.startChildSpan(ctx, "guestmemory.sample_host_pressure")
	sample, err := c.sampler.Sample(sampleCtx)
	if err != nil {
		c.metrics.RecordSamplerError(ctx, "host_pressure")
		sampleSpan.RecordError(err)
		sampleSpan.SetStatus(codes.Error, err.Error())
		sampleSpan.End()
		c.recordReconcileError(ctx, trigger, start, span, err)
		return ManualReclaimResponse{}, fmt.Errorf("sample host pressure: %w", err)
	}
	sampleSpan.SetAttributes(
		attribute.Int64("host_available_bytes", sample.AvailableBytes),
		attribute.Float64("host_available_percent", sample.AvailablePercent),
		attribute.Bool("stressed", sample.Stressed),
	)
	sampleSpan.SetStatus(codes.Ok, "")
	sampleSpan.End()

	summary := reconcileSummary{
		hostAvailable:     sample.AvailableBytes,
		hostAvailablePerc: sample.AvailablePercent,
	}

	if state.manualHold != nil && !state.manualHold.until.IsZero() && now.After(state.manualHold.until) {
		logFromContext(ctx, c.log).InfoContext(ctx,
			"guest memory manual reclaim hold expired",
			"operation", "manual_reclaim",
		)
		state.manualHold = nil
	}

	if req.force && !req.dryRun {
		if req.holdFor <= 0 {
			if state.manualHold != nil {
				logFromContext(ctx, c.log).InfoContext(ctx,
					"guest memory manual reclaim hold cleared",
					"operation", "manual_reclaim",
				)
			}
			state.manualHold = nil
		} else {
			state.manualHold = &manualHold{
				reclaimBytes: req.requestedReclaim,
				until:        now.Add(req.holdFor),
			}
			logFromContext(ctx, c.log).InfoContext(ctx,
				"guest memory manual reclaim hold set",
				"operation", "manual_reclaim",
				"requested_reclaim_bytes", req.requestedReclaim,
				"hold_for_seconds", req.holdFor.Seconds(),
			)
		}
	}

	listCtx, listSpan := c.startChildSpan(ctx, "guestmemory.list_balloon_vms")
	vms, err := c.source.ListBalloonVMs(listCtx)
	if err != nil {
		listSpan.RecordError(err)
		listSpan.SetStatus(codes.Error, err.Error())
		listSpan.End()
		c.recordReconcileError(ctx, trigger, start, span, err)
		return ManualReclaimResponse{}, err
	}
	listSpan.SetAttributes(attribute.Int("vm_count", len(vms)))
	listSpan.SetStatus(codes.Ok, "")
	listSpan.End()

	candidates := make([]candidateState, 0, len(vms))
	actions := make([]ManualReclaimAction, 0, len(vms))
	var currentTotalReclaim int64
	for _, vm := range vms {
		hv, err := state.newClient(vm.HypervisorType, vm.SocketPath)
		if err != nil {
			actions = append(actions, skippedAction(vm, "error", fmt.Sprintf("create hypervisor client: %v", err)))
			continue
		}
		if !hv.Capabilities().SupportsBalloonControl {
			actions = append(actions, skippedAction(vm, "unsupported", "runtime balloon control is not supported"))
			continue
		}

		currentTarget, err := hv.GetTargetGuestMemoryBytes(ctx)
		if err != nil {
			actions = append(actions, skippedAction(vm, "error", fmt.Sprintf("read balloon target: %v", err)))
			continue
		}

		currentTarget = clampInt64(currentTarget, 0, vm.AssignedMemoryBytes)
		// The protected floor guards the guest's normal working size, so it is
		// anchored on the baseline rather than the ceiling. For ordinary VMs the
		// baseline equals the assigned size, so this is unchanged; for a ceiling VM
		// it keeps the floor at/below the baseline the guest is held at.
		protectedFloor := protectedFloorBytes(c.config, floorAnchorBytes(vm))
		if protectedFloor > vm.AssignedMemoryBytes {
			protectedFloor = vm.AssignedMemoryBytes
		}

		// currentTotalReclaim is the memory currently held back from the size the
		// host expects each guest to use, so it is measured below the baseline
		// anchor, not the ceiling. A ceiling VM idling at its baseline is reclaiming
		// nothing real (the ballooned pages were never resident) and must contribute
		// 0, otherwise its headroom would be counted as reclaim and squeeze its
		// co-tenants under the Stressed branch.
		currentReclaim := floorAnchorBytes(vm) - currentTarget
		if currentReclaim < 0 {
			currentReclaim = 0
		}
		currentTotalReclaim += currentReclaim

		candidates = append(candidates, candidateState{
			vm:                      vm,
			hv:                      hv,
			currentTargetGuestBytes: currentTarget,
			protectedFloorBytes:     protectedFloor,
			// Reclaim headroom is measured from the baseline anchor down to the
			// floor. A ceiling VM's ballooned headroom above its baseline is not
			// resident, so it is not reclaimable; counting it would overstate the
			// fleet's reclaimable memory and (with the ceiling anchor) inflate the
			// VM under pressure. For ordinary VMs the anchor is the assigned size.
			maxReclaimBytes: maxInt64(0, floorAnchorBytes(vm)-protectedFloor),
		})
	}
	summary.eligibleVMs = len(candidates)

	previousPressure := state.pressureState
	state.pressureState = nextPressureState(state.pressureState, c.config, sample)
	summary.previousPressure = previousPressure
	summary.currentPressure = state.pressureState
	summary.pressureChanged = previousPressure != state.pressureState
	if summary.pressureChanged {
		c.metrics.RecordPressureTransition(ctx, previousPressure, state.pressureState)
	}
	autoTarget := automaticTargetBytes(state.pressureState, c.config, sample, currentTotalReclaim)

	manualTarget := int64(0)
	if req.dryRun {
		manualTarget = req.requestedReclaim
	} else if req.force && req.requestedReclaim > 0 {
		manualTarget = req.requestedReclaim
	} else if state.manualHold != nil {
		manualTarget = state.manualHold.reclaimBytes
	}
	totalTarget := maxInt64(autoTarget, manualTarget)
	summary.autoTarget = autoTarget
	summary.manualTarget = manualTarget
	summary.effectiveTarget = totalTarget
	summary.manualHoldActive = state.manualHold != nil

	plannedTargets := planGuestTargets(c.config, candidates, totalTarget)
	// No reclaim demanded means the host is healthy: recover each guest toward its
	// baseline (baseline == assigned for ordinary VMs, so prior reclaim is undone
	// unchanged) but never below its current target, so a ceiling VM deliberately
	// grown above baseline (via the balloon API, or auto-grow once a signal exists)
	// is held there rather than reverted. growthTargetBytes only raises further when
	// grow-on-demand is enabled. This per-VM path has no aggregate host-RAM cap,
	// which is safe only because utilizationPercent() is 0 today; a real signal
	// (RFC milestone 4) must route grow through a host-aware planner.
	if totalTarget == 0 {
		for _, candidate := range candidates {
			hold := maxInt64(candidate.baselineGuestBytes(), candidate.currentTargetGuestBytes)
			plannedTargets[candidate.vm.ID] = growthTargetBytes(
				c.config,
				hold,
				candidate.vm.AssignedMemoryBytes,
				candidate.protectedFloorBytes,
				candidate.utilizationPercent(),
			)
		}
	}

	resp := ManualReclaimResponse{
		RequestedReclaimBytes: req.requestedReclaim,
		HoldUntil:             holdUntil(state.manualHold),
		HostAvailableBytes:    sample.AvailableBytes,
		HostPressureState:     state.pressureState,
		Actions:               make([]ManualReclaimAction, 0, len(actions)+len(candidates)),
	}
	resp.Actions = append(resp.Actions, actions...)
	for _, action := range actions {
		switch action.Status {
		case "error":
			summary.errorCount++
		case "unsupported":
			summary.unsupportedCount++
		default:
			summary.unchangedCount++
		}
	}

	applyCtx, applySpan := c.startChildSpan(ctx, "guestmemory.apply_balloon_targets")
	for _, candidate := range candidates {
		plannedTarget, ok := plannedTargets[candidate.vm.ID]
		if !ok {
			plannedTarget = candidate.vm.AssignedMemoryBytes
		}

		appliedTarget := plannedTarget
		if absInt64(appliedTarget-candidate.currentTargetGuestBytes) < c.config.MinAdjustmentBytes {
			appliedTarget = candidate.currentTargetGuestBytes
		}
		if !req.force {
			if lastAppliedAt, ok := state.lastApplied[candidate.vm.ID]; ok && now.Sub(lastAppliedAt) < c.config.PerVMCooldown {
				appliedTarget = candidate.currentTargetGuestBytes
			}
		}
		delta := appliedTarget - candidate.currentTargetGuestBytes
		if appliedTarget != candidate.currentTargetGuestBytes {
			if delta > 0 {
				appliedTarget = candidate.currentTargetGuestBytes + minInt64(delta, c.config.PerVMMaxStepBytes)
			} else {
				appliedTarget = candidate.currentTargetGuestBytes - minInt64(-delta, c.config.PerVMMaxStepBytes)
			}
		}

		appliedTarget = clampInt64(appliedTarget, candidate.protectedFloorBytes, candidate.vm.AssignedMemoryBytes)
		plannedTarget = clampInt64(plannedTarget, candidate.protectedFloorBytes, candidate.vm.AssignedMemoryBytes)

		action := ManualReclaimAction{
			InstanceID:                     candidate.vm.ID,
			InstanceName:                   candidate.vm.Name,
			Hypervisor:                     candidate.vm.HypervisorType,
			AssignedMemoryBytes:            candidate.vm.AssignedMemoryBytes,
			ProtectedFloorBytes:            candidate.protectedFloorBytes,
			PreviousTargetGuestMemoryBytes: candidate.currentTargetGuestBytes,
			PlannedTargetGuestMemoryBytes:  plannedTarget,
			TargetGuestMemoryBytes:         candidate.currentTargetGuestBytes,
			Status:                         "unchanged",
		}

		resp.PlannedReclaimBytes += maxInt64(0, floorAnchorBytes(candidate.vm)-plannedTarget)

		if !req.dryRun && appliedTarget != candidate.currentTargetGuestBytes {
			if err := candidate.hv.SetTargetGuestMemoryBytes(applyCtx, appliedTarget); err != nil {
				action.Status = "error"
				action.Error = err.Error()
				logFromContext(ctx, c.log).WarnContext(ctx,
					"guest memory reclaim action failed",
					"operation", "active_ballooning_apply",
					"trigger", trigger,
					"instance_id", candidate.vm.ID,
					"hypervisor", candidate.vm.HypervisorType,
					"previous_target_guest_memory_bytes", candidate.currentTargetGuestBytes,
					"target_guest_memory_bytes", appliedTarget,
					"error", err,
				)
				resp.Actions = append(resp.Actions, action)
				continue
			}
			state.lastApplied[candidate.vm.ID] = now
			action.Status = "applied"
			action.TargetGuestMemoryBytes = appliedTarget
		}
		if req.dryRun && appliedTarget != candidate.currentTargetGuestBytes {
			action.Status = "planned"
			action.TargetGuestMemoryBytes = appliedTarget
		}
		if !req.dryRun {
			action.AppliedReclaimBytes = maxInt64(0, floorAnchorBytes(candidate.vm)-action.TargetGuestMemoryBytes)
		}
		resp.AppliedReclaimBytes += action.AppliedReclaimBytes
		resp.Actions = append(resp.Actions, action)

		switch action.Status {
		case "applied":
			summary.appliedCount++
		case "planned":
			summary.plannedCount++
		case "error":
			summary.errorCount++
		case "unsupported":
			summary.unsupportedCount++
		default:
			summary.unchangedCount++
		}
	}
	// Prune lastApplied entries for VMs no longer in the candidate list.
	activeVMs := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		activeVMs[candidate.vm.ID] = struct{}{}
	}
	for vmID := range state.lastApplied {
		if _, ok := activeVMs[vmID]; !ok {
			delete(state.lastApplied, vmID)
		}
	}

	applySpan.SetAttributes(
		attribute.Int("eligible_vms", summary.eligibleVMs),
		attribute.Int("applied_vms", summary.appliedCount),
		attribute.Int("planned_vms", summary.plannedCount),
		attribute.Int("error_vms", summary.errorCount),
	)
	if summary.errorCount > 0 {
		applySpan.SetStatus(codes.Error, "one or more balloon target updates failed")
	} else {
		applySpan.SetStatus(codes.Ok, "")
	}
	applySpan.End()

	summary.plannedReclaim = resp.PlannedReclaimBytes
	summary.appliedReclaim = resp.AppliedReclaimBytes
	c.recordReconcileSuccess(ctx, trigger, req, span, start, summary, resp.Actions)
	c.logPressureTransition(ctx, summary)
	c.logReconcileSummary(ctx, req, summary, reconcileStatus(summary))

	return resp, nil
}

func holdUntil(hold *manualHold) *time.Time {
	if hold == nil || hold.until.IsZero() {
		return nil
	}
	until := hold.until
	return &until
}

func skippedAction(vm BalloonVM, status, err string) ManualReclaimAction {
	return ManualReclaimAction{
		InstanceID:          vm.ID,
		InstanceName:        vm.Name,
		Hypervisor:          vm.HypervisorType,
		AssignedMemoryBytes: vm.AssignedMemoryBytes,
		Status:              status,
		Error:               err,
	}
}
