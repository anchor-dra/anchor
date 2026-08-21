package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/anchor-dra/anchor/internal/model"
)

type fakePathOps struct {
	present    map[string]bool
	drifted    map[string]bool
	fail       map[string]error
	verifyFail map[string]error
	created    []string
	deleted    []string
	verified   []string
}

func newFakePathOps() *fakePathOps {
	return &fakePathOps{present: map[string]bool{}, drifted: map[string]bool{}, fail: map[string]error{}, verifyFail: map[string]error{}}
}

func (o *fakePathOps) InjectPath(_ context.Context, _ PodNetworkPlan, path model.PlacementPath, _ NetworkNamespace) (bool, error) {
	if err := o.fail[path.Name]; err != nil {
		return false, err
	}
	created := !o.present[path.Name]
	o.present[path.Name] = true
	o.drifted[path.Name] = false
	if created {
		o.created = append(o.created, path.Name)
	}
	return created, nil
}

func (o *fakePathOps) VerifyPath(_ context.Context, path model.PlacementPath, _ NetworkNamespace) error {
	o.verified = append(o.verified, path.Name)
	if err := o.verifyFail[path.Name]; err != nil {
		return err
	}
	if !o.present[path.Name] || o.drifted[path.Name] {
		return fmt.Errorf("path %s drifted", path.Name)
	}
	return nil
}

func TestPlanInjectorRollsBackNewPathsWhenFinalVerificationFails(t *testing.T) {
	ops := newFakePathOps()
	ops.verifyFail["a"] = errors.New("verification failed")
	injector := &PlanInjector{Ops: ops}
	err := injector.Inject(context.Background(), planWithPaths("a", "b"), NetworkNamespace{Path: "/proc/1/ns/net"})
	if err == nil {
		t.Fatal("expected verification failure")
	}
	if got := fmt.Sprint(ops.deleted); got != "[b a]" {
		t.Fatalf("rollback order=%s, want reverse creation order", got)
	}
}

func (o *fakePathOps) DeletePath(_ context.Context, path model.PlacementPath, _ NetworkNamespace) error {
	o.deleted = append(o.deleted, path.Name)
	delete(o.present, path.Name)
	return nil
}

func planWithPaths(names ...string) PodNetworkPlan {
	plan := testPlan()
	plan.Paths = nil
	for index, name := range names {
		plan.Paths = append(plan.Paths, model.PlacementPath{Name: name, InterfaceName: fmt.Sprintf("carrier-%d", index)})
	}
	return plan
}

func TestPlanInjectorIsIdempotent(t *testing.T) {
	ops := newFakePathOps()
	injector := &PlanInjector{Ops: ops}
	plan := planWithPaths("a", "b")
	namespace := NetworkNamespace{Path: "/proc/1/ns/net"}
	if err := injector.Inject(context.Background(), plan, namespace); err != nil {
		t.Fatal(err)
	}
	if err := injector.Inject(context.Background(), plan, namespace); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(ops.created); got != "[a b]" {
		t.Fatalf("created paths=%s, want each path exactly once", got)
	}
}

func TestPlanInjectorRollsBackOnlyPathsCreatedByFailedAttempt(t *testing.T) {
	ops := newFakePathOps()
	ops.present["a"] = true
	ops.fail["c"] = errors.New("injected failure")
	injector := &PlanInjector{Ops: ops}
	err := injector.Inject(context.Background(), planWithPaths("a", "b", "c"), NetworkNamespace{Path: "/proc/1/ns/net"})
	if err == nil {
		t.Fatal("expected injection failure")
	}
	if got := fmt.Sprint(ops.deleted); got != "[b]" {
		t.Fatalf("rolled back paths=%s, want only newly created path b", got)
	}
	if !ops.present["a"] {
		t.Fatal("pre-existing path a was removed during rollback")
	}
}

func TestPlanInjectorReconcileRepairsDrift(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := planWithPaths("a")
	plan.Phase = PlanInjected
	plan.NetworkNamespace = "/proc/1/ns/net"
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	ops := newFakePathOps()
	ops.present["a"] = true
	ops.drifted["a"] = true
	results, err := (&PlanInjector{Store: store, Ops: ops}).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Drifted || results[0].Err != nil {
		t.Fatalf("unexpected reconciliation result: %#v", results)
	}
	if results[0].DriftErr == nil {
		t.Fatal("drift observation was not preserved for telemetry")
	}
	if ops.drifted["a"] || len(ops.created) != 0 {
		t.Fatalf("drift was not repaired idempotently: %#v", ops)
	}
}
