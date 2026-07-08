package providers

import (
	"context"
	"log/slog"
	"strings"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/uffdgraduate"
	"go.opentelemetry.io/otel"
)

// uffdGraduationManager is the subset of the instance manager the graduation
// controller needs. ListInstances is on the public interface; the other two are
// concrete manager methods, matched here by type assertion.
type uffdGraduationManager interface {
	ListInstances(ctx context.Context, filter *instances.ListInstancesFilter) ([]instances.Instance, error)
	GraduateSnapshotMemoryPager(ctx context.Context, id string) error
	UFFDGraduationTargetVersion() string
}

type uffdGraduationStore struct {
	manager uffdGraduationManager
}

func (s uffdGraduationStore) ListPagerInstances(ctx context.Context) ([]uffdgraduate.Instance, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]uffdgraduate.Instance, 0, len(insts))
	for _, inst := range insts {
		if inst.State != instances.StateRunning {
			continue
		}
		if strings.TrimSpace(inst.FirecrackerUFFDSessionID) == "" {
			continue
		}
		out = append(out, uffdgraduate.Instance{
			ID:           inst.Id,
			Name:         inst.Name,
			PagerVersion: inst.FirecrackerUFFDPagerVersion,
		})
	}
	return out, nil
}

func (s uffdGraduationStore) GraduateInstance(ctx context.Context, id string) error {
	return s.manager.GraduateSnapshotMemoryPager(ctx, id)
}

func (s uffdGraduationStore) TargetVersion() string {
	return s.manager.UFFDGraduationTargetVersion()
}

// ProvideUFFDGraduationController builds the controller, or returns nil when the
// feature is disabled or no UFFD pager is configured (file backend / non-linux).
func ProvideUFFDGraduationController(instanceManager instances.Manager, cfg uffdgraduate.Config, log *slog.Logger) *uffdgraduate.Controller {
	if instanceManager == nil || log == nil || !cfg.Enabled {
		return nil
	}
	mgr, ok := instanceManager.(uffdGraduationManager)
	if !ok {
		log.Warn("uffd graduation is enabled but the instance manager does not support it; controller disabled")
		return nil
	}
	if strings.TrimSpace(mgr.UFFDGraduationTargetVersion()) == "" {
		log.Warn("uffd graduation is enabled but no uffd pager is configured (snapshot memory backend is not uffd); controller disabled")
		return nil
	}
	return uffdgraduate.NewController(
		uffdGraduationStore{manager: mgr},
		cfg,
		uffdgraduate.ControllerOptions{
			Log:   log.With("controller", "uffd_graduation"),
			Meter: otel.GetMeterProvider().Meter("hypeman/uffdgraduate"),
		},
	)
}
