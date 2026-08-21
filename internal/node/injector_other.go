//go:build !linux

package node

import (
	"context"
	"errors"
)

type IPvlanInjector struct {
	Store *PlanStore
}

func (*IPvlanInjector) Inject(context.Context, PodNetworkPlan, NetworkNamespace) error {
	return errors.New("ipvlan injection is supported only on Linux")
}

func (*IPvlanInjector) Verify(context.Context, PodNetworkPlan, NetworkNamespace) error {
	return errors.New("ipvlan injection is supported only on Linux")
}

func (*IPvlanInjector) Reconcile(context.Context) ([]PlanReconcileResult, error) { return nil, nil }
