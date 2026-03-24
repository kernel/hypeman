package hypervisor

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type traceAttrsKey struct{}

type traceWrapped interface {
	isTraceWrapped()
}

type tracingHypervisor struct {
	hvType Type
	next   Hypervisor
	tracer trace.Tracer
	attrs  []attribute.KeyValue
}

type tracingVMStarter struct {
	hvType Type
	next   VMStarter
	tracer trace.Tracer
}

func WithTraceAttributes(ctx context.Context, attrs ...attribute.KeyValue) context.Context {
	if len(attrs) == 0 {
		return ctx
	}

	existing, _ := ctx.Value(traceAttrsKey{}).([]attribute.KeyValue)
	merged := make([]attribute.KeyValue, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, traceAttrsKey{}, merged)
}

func TraceAttributesFromContext(ctx context.Context) []attribute.KeyValue {
	existing, _ := ctx.Value(traceAttrsKey{}).([]attribute.KeyValue)
	if len(existing) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, len(existing))
	copy(out, existing)
	return out
}

func WrapHypervisor(hvType Type, hv Hypervisor) Hypervisor {
	if hv == nil {
		return nil
	}
	if _, ok := hv.(traceWrapped); ok {
		return hv
	}

	return &tracingHypervisor{
		hvType: hvType,
		next:   hv,
		tracer: otel.Tracer(traceSubsystemForType(hvType)),
		attrs: []attribute.KeyValue{
			attribute.String("hypervisor", string(hvType)),
		},
	}
}

func WrapVMStarter(hvType Type, starter VMStarter) VMStarter {
	if starter == nil {
		return nil
	}
	if _, ok := starter.(traceWrapped); ok {
		return starter
	}
	return &tracingVMStarter{
		hvType: hvType,
		next:   starter,
		tracer: otel.Tracer(traceSubsystemForType(hvType)),
	}
}

func traceSubsystemForType(hvType Type) string {
	switch hvType {
	case TypeCloudHypervisor:
		return "hypeman/hypervisor/cloudhypervisor"
	case TypeFirecracker:
		return "hypeman/hypervisor/firecracker"
	case TypeQEMU:
		return "hypeman/hypervisor/qemu"
	case TypeVZ:
		return "hypeman/hypervisor/vz"
	default:
		return "hypeman/hypervisor"
	}
}

func StartImplementationSpan(ctx context.Context, hvType Type, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	baseAttrs := []attribute.KeyValue{
		attribute.String("hypervisor", string(hvType)),
	}
	baseAttrs = append(baseAttrs, attrs...)
	return startTraceSpan(ctx, otel.Tracer(traceSubsystemForType(hvType)), name, baseAttrs...)
}

func StartProcessSpan(ctx context.Context, hvType Type) (context.Context, trace.Span) {
	return StartImplementationSpan(ctx, hvType, "hypervisor.start_process", attribute.String("operation", "start_process"))
}

func FinishTraceSpan(span trace.Span, err error) {
	finishTraceSpan(span, err)
}

func startTraceSpan(ctx context.Context, tracer trace.Tracer, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	allAttrs := TraceAttributesFromContext(ctx)
	if len(attrs) > 0 {
		allAttrs = append(allAttrs, attrs...)
	}
	if len(allAttrs) == 0 {
		return tracer.Start(ctx, name)
	}
	return tracer.Start(ctx, name, trace.WithAttributes(allAttrs...))
}

func finishTraceSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func (h *tracingHypervisor) isTraceWrapped() {}

func (s *tracingVMStarter) isTraceWrapped() {}

func (h *tracingHypervisor) Capabilities() Capabilities {
	return h.next.Capabilities()
}

func (h *tracingHypervisor) spanAttrs(attrs ...attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(h.attrs)+len(attrs))
	out = append(out, h.attrs...)
	out = append(out, attrs...)
	return out
}

func (h *tracingHypervisor) DeleteVM(ctx context.Context) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.delete_vm", h.spanAttrs(attribute.String("operation", "delete_vm"))...)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.DeleteVM(ctx)
}

func (h *tracingHypervisor) Shutdown(ctx context.Context) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.shutdown", h.spanAttrs(attribute.String("operation", "shutdown"))...)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.Shutdown(ctx)
}

func (h *tracingHypervisor) GetVMInfo(ctx context.Context) (_ *VMInfo, err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.get_vm_info", h.spanAttrs(attribute.String("operation", "get_vm_info"))...)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.GetVMInfo(ctx)
}

func (h *tracingHypervisor) Pause(ctx context.Context) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.pause", h.spanAttrs(attribute.String("operation", "pause"))...)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.Pause(ctx)
}

func (h *tracingHypervisor) Resume(ctx context.Context) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.resume", h.spanAttrs(attribute.String("operation", "resume"))...)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.Resume(ctx)
}

func (h *tracingHypervisor) Snapshot(ctx context.Context, destPath string) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.snapshot",
		h.spanAttrs(
			attribute.String("operation", "snapshot"),
		)...,
	)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.Snapshot(ctx, destPath)
}

func (h *tracingHypervisor) ResizeMemory(ctx context.Context, bytes int64) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.resize_memory",
		h.spanAttrs(
			attribute.String("operation", "resize_memory"),
			attribute.Int64("memory_bytes", bytes),
		)...,
	)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.ResizeMemory(ctx, bytes)
}

func (h *tracingHypervisor) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.resize_memory_and_wait",
		h.spanAttrs(
			attribute.String("operation", "resize_memory_and_wait"),
			attribute.Int64("memory_bytes", bytes),
			attribute.Int64("timeout_seconds", int64(timeout.Seconds())),
		)...,
	)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.ResizeMemoryAndWait(ctx, bytes, timeout)
}

func (h *tracingHypervisor) SetTargetGuestMemoryBytes(ctx context.Context, bytes int64) (err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.set_target_guest_memory_bytes",
		h.spanAttrs(
			attribute.String("operation", "set_target_guest_memory_bytes"),
			attribute.Int64("guest_memory_bytes", bytes),
		)...,
	)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.SetTargetGuestMemoryBytes(ctx, bytes)
}

func (h *tracingHypervisor) GetTargetGuestMemoryBytes(ctx context.Context) (_ int64, err error) {
	ctx, span := startTraceSpan(ctx, h.tracer, "hypervisor.get_target_guest_memory_bytes",
		h.spanAttrs(attribute.String("operation", "get_target_guest_memory_bytes"))...,
	)
	defer func() { finishTraceSpan(span, err) }()
	return h.next.GetTargetGuestMemoryBytes(ctx)
}

func (s *tracingVMStarter) SocketName() string {
	return s.next.SocketName()
}

func (s *tracingVMStarter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	return s.next.GetBinaryPath(p, version)
}

func (s *tracingVMStarter) GetVersion(p *paths.Paths) (string, error) {
	return s.next.GetVersion(p)
}

func (s *tracingVMStarter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config VMConfig) (pid int, hv Hypervisor, err error) {
	ctx, span := startTraceSpan(ctx, s.tracer, "hypervisor.start_vm",
		attribute.String("hypervisor", string(s.hvType)),
		attribute.String("operation", "start_vm"),
	)
	defer func() {
		if err == nil && hv != nil {
			hv = WrapHypervisor(s.hvType, hv)
		}
		finishTraceSpan(span, err)
	}()
	pid, hv, err = s.next.StartVM(ctx, p, version, socketPath, config)
	return pid, hv, err
}

func (s *tracingVMStarter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (pid int, hv Hypervisor, err error) {
	ctx, span := startTraceSpan(ctx, s.tracer, "hypervisor.restore_vm",
		attribute.String("hypervisor", string(s.hvType)),
		attribute.String("operation", "restore_vm"),
	)
	defer func() {
		if err == nil && hv != nil {
			hv = WrapHypervisor(s.hvType, hv)
		}
		finishTraceSpan(span, err)
	}()
	pid, hv, err = s.next.RestoreVM(ctx, p, version, socketPath, snapshotPath)
	return pid, hv, err
}

func (s *tracingVMStarter) PrepareFork(ctx context.Context, req ForkPrepareRequest) (_ ForkPrepareResult, err error) {
	ctx, span := startTraceSpan(ctx, s.tracer, "hypervisor.prepare_fork",
		attribute.String("hypervisor", string(s.hvType)),
		attribute.String("operation", "prepare_fork"),
		attribute.Bool("has_snapshot_config_path", req.SnapshotConfigPath != ""),
	)
	defer func() { finishTraceSpan(span, err) }()
	return s.next.PrepareFork(ctx, req)
}
