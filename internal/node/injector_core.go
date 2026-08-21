package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/anchor-dra/anchor/internal/model"
)

type NetworkNamespace struct {
	Path string
}

type PlanReconcileResult struct {
	Plan     PodNetworkPlan
	Drifted  bool
	DriftErr error
	Err      error
}

type LinkInjector interface {
	Inject(context.Context, PodNetworkPlan, NetworkNamespace) error
	Verify(context.Context, PodNetworkPlan, NetworkNamespace) error
	Reconcile(context.Context) ([]PlanReconcileResult, error)
}

// PathNetworkOps is the narrow seam around platform netlink operations. The
// orchestration and rollback semantics are tested without requiring root or a
// real network namespace.
type PathNetworkOps interface {
	InjectPath(context.Context, PodNetworkPlan, model.PlacementPath, NetworkNamespace) (bool, error)
	VerifyPath(context.Context, model.PlacementPath, NetworkNamespace) error
	DeletePath(context.Context, model.PlacementPath, NetworkNamespace) error
}

type PlanInjector struct {
	Store *PlanStore
	Ops   PathNetworkOps
}

func (i *PlanInjector) Inject(ctx context.Context, plan PodNetworkPlan, namespace NetworkNamespace) error {
	if i.Ops == nil {
		return errors.New("path network operations are required")
	}
	created := make([]model.PlacementPath, 0, len(plan.Paths))
	for _, path := range plan.Paths {
		wasCreated, err := i.Ops.InjectPath(ctx, plan, path, namespace)
		if err != nil {
			for index := len(created) - 1; index >= 0; index-- {
				_ = i.Ops.DeletePath(ctx, created[index], namespace)
			}
			return fmt.Errorf("inject path %q: %w", path.Name, err)
		}
		if wasCreated {
			created = append(created, path)
		}
	}
	if err := i.Verify(ctx, plan, namespace); err != nil {
		for index := len(created) - 1; index >= 0; index-- {
			_ = i.Ops.DeletePath(ctx, created[index], namespace)
		}
		return err
	}
	return nil
}

func (i *PlanInjector) Verify(ctx context.Context, plan PodNetworkPlan, namespace NetworkNamespace) error {
	if i.Ops == nil {
		return errors.New("path network operations are required")
	}
	for _, path := range plan.Paths {
		if err := i.Ops.VerifyPath(ctx, path, namespace); err != nil {
			return fmt.Errorf("verify path %q: %w", path.Name, err)
		}
	}
	return nil
}

// Reconcile is level-triggered: verify every live plan, repair drift with the
// same idempotent injection path, and report per-plan results without turning
// one workload failure into a plugin-level error.
func (i *PlanInjector) Reconcile(ctx context.Context) ([]PlanReconcileResult, error) {
	if i.Store == nil {
		return nil, errors.New("plan store is required")
	}
	plans, planErrors, err := i.Store.LoadAllLenient()
	if err != nil {
		return nil, err
	}
	results := make([]PlanReconcileResult, 0, len(planErrors))
	for _, planErr := range planErrors {
		results = append(results, PlanReconcileResult{Err: planErr})
	}
	for _, plan := range plans {
		if (plan.Phase != PlanInjected && plan.Phase != PlanRecovering) || plan.NetworkNamespace == "" {
			continue
		}
		namespace := NetworkNamespace{Path: plan.NetworkNamespace}
		verifyErr := i.Verify(ctx, plan, namespace)
		if verifyErr == nil {
			if plan.Phase == PlanRecovering {
				results = append(results, PlanReconcileResult{Plan: plan})
			}
			continue
		}
		repairErr := i.Inject(ctx, plan, namespace)
		results = append(results, PlanReconcileResult{Plan: plan, Drifted: true, DriftErr: verifyErr, Err: repairErr})
	}
	return results, nil
}
