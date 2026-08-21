package node

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/nri/pkg/api"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/anchor-dra/anchor/internal/constants"
	"github.com/anchor-dra/anchor/internal/model"
)

func testPlan() PodNetworkPlan {
	return PodNetworkPlan{
		Version: 1, PodNamespace: "telco", PodName: "smsc", PodUID: "pod-uid",
		ClaimNamespace: "telco", ClaimName: "sigtran", ClaimUID: "claim-uid",
		RequestName: "endpoint", PoolName: "node-a", DeviceName: "carrier-slot-0",
		PlacementName: "claim-claim-uid", Phase: PlanInjectionPending,
		Paths: []model.PlacementPath{{Name: "a", InterfaceName: "sigtran-a", RoutingTable: 101, IP: "10.0.1.10/24", Interface: "ens6", ParentMAC: "02:00:00:00:00:01"}},
	}
}

func TestAnchorClaimCountResolvesDirectAndTemplateClaims(t *testing.T) {
	directName := "direct-claim"
	templateName := "claim-template"
	generatedName := "pod-endpoint-abcde"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "smsc", Namespace: "telco", UID: types.UID("pod-uid")},
		Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{
			{Name: "direct", ResourceClaimName: &directName},
			{Name: "generated", ResourceClaimTemplateName: &templateName},
		}},
		Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
			{Name: "generated", ResourceClaimName: &generatedName},
		}},
	}
	claim := func(name string) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "telco"},
			Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{{Driver: constants.DriverName}}},
			}},
		}
	}
	plugin := NewNRIPlugin(fake.NewSimpleClientset(pod, claim(directName), claim(generatedName)), nil, nil, nil, slog.Default())
	count, err := plugin.anchorClaimCount(context.Background(), &api.PodSandbox{Name: pod.Name, Namespace: pod.Namespace, Uid: string(pod.UID)})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("Anchor claim count=%d, want 2", count)
	}
}

func TestPodResourceClaimNamesRequiresTemplateStatusMapping(t *testing.T) {
	templateName := "claim-template"
	pod := &corev1.Pod{Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{
		Name: "endpoint", ResourceClaimTemplateName: &templateName,
	}}}}
	if _, err := podResourceClaimNames(pod); err == nil {
		t.Fatal("expected missing generated claim status to fail closed")
	}
}

func TestPlanStorePersistsAndIndexesByPodUID(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(plan.ClaimUID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PodUID != plan.PodUID || stored.Phase != PlanInjectionPending || stored.LastObservation.IsZero() {
		t.Fatalf("unexpected persisted plan: %#v", stored)
	}
	plans, err := store.ForPod(plan.PodUID)
	if err != nil || len(plans) != 1 || plans[0].ClaimUID != plan.ClaimUID {
		t.Fatalf("unexpected pod index: plans=%#v err=%v", plans, err)
	}
	stored.Phase = PlanRetained
	if err := store.Save(*stored); err != nil {
		t.Fatal(err)
	}
	plans, err = store.ForPod(plan.PodUID)
	if err != nil || len(plans) != 0 {
		t.Fatalf("retained plan remained active: plans=%#v err=%v", plans, err)
	}
}

func TestRetainedPlanGarbageCollectionFollowsClaimDeletion(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	plan.Phase = PlanRetained
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	plugin := &Plugin{core: fake.NewSimpleClientset(), store: store}
	if err := plugin.gcRetainedPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(plan.ClaimUID); !os.IsNotExist(err) {
		t.Fatalf("retained plan still exists after claim deletion: %v", err)
	}
}

type recordingInjector struct {
	plans            []PodNetworkPlan
	err              error
	verifyCalls      int
	verifyErr        error
	reconcileResults []PlanReconcileResult
	reconcileErr     error
}

func (i *recordingInjector) Inject(_ context.Context, plan PodNetworkPlan, _ NetworkNamespace) error {
	i.plans = append(i.plans, plan)
	return i.err
}
func (i *recordingInjector) Verify(context.Context, PodNetworkPlan, NetworkNamespace) error {
	i.verifyCalls++
	return i.verifyErr
}
func (i *recordingInjector) Reconcile(context.Context) ([]PlanReconcileResult, error) {
	return i.reconcileResults, i.reconcileErr
}

func eventMask(events ...api.Event) api.EventMask {
	var mask api.EventMask
	mask.Set(events...)
	return mask
}

func TestNRIConfigureSelectsLifecycleByContainerdVersion(t *testing.T) {
	tests := []struct {
		name           string
		runtimeName    string
		runtimeVersion string
		want           api.EventMask
		wantErr        bool
	}{
		{name: "upstream 2.2.5", runtimeName: "containerd", runtimeVersion: "2.2.5", want: eventMask(api.Event_CREATE_CONTAINER)},
		{name: "prefixed 2.2.5", runtimeName: "containerd", runtimeVersion: "v2.2.5", want: eventMask(api.Event_CREATE_CONTAINER)},
		{name: "packaged 2.2.5", runtimeName: "containerd", runtimeVersion: "2.2.5-1.amzn2023.0.2", want: eventMask(api.Event_CREATE_CONTAINER)},
		{name: "boundary 2.2.6", runtimeName: "containerd", runtimeVersion: "2.2.6", want: eventMask(api.Event_RUN_POD_SANDBOX, api.Event_CREATE_CONTAINER)},
		{name: "newer containerd", runtimeName: "containerd", runtimeVersion: "2.3.4", want: eventMask(api.Event_RUN_POD_SANDBOX, api.Event_CREATE_CONTAINER)},
		{name: "unknown version", runtimeName: "containerd", runtimeVersion: "development", want: eventMask(api.Event_CREATE_CONTAINER)},
		{name: "unsupported runtime", runtimeName: "cri-o", runtimeVersion: "1.35.0", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := NewNRIPlugin(nil, nil, nil, nil, slog.Default())
			got, err := plugin.Configure(context.Background(), "", test.runtimeName, test.runtimeVersion)
			if (err != nil) != test.wantErr {
				t.Fatalf("Configure() error=%v, wantErr=%v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("Configure() events=%s (0x%x), want %s (0x%x)", got.PrettyString(), got, test.want.PrettyString(), test.want)
			}
		})
	}
}

func TestCreateContainerInjectsRetriesAndThenVerifies(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	claimName := plan.ClaimName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: plan.PodName, Namespace: plan.PodNamespace, UID: types.UID(plan.PodUID)},
		Spec:       corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: plan.RequestName, ResourceClaimName: &claimName}}},
	}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: plan.ClaimNamespace},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{{Driver: constants.DriverName}}},
		}},
	}
	injector := &recordingInjector{err: errors.New("injected failure")}
	plugin := NewNRIPlugin(fake.NewSimpleClientset(pod, claim), nil, store, injector, slog.Default())
	sandbox := &api.PodSandbox{
		Name: plan.PodName, Namespace: plan.PodNamespace, Uid: plan.PodUID,
		Linux: &api.LinuxPodSandbox{Namespaces: []*api.LinuxNamespace{{Type: "network", Path: "/proc/1/ns/net"}}},
	}

	if _, _, err := plugin.CreateContainer(context.Background(), sandbox, &api.Container{Name: "init"}); err == nil {
		t.Fatal("CreateContainer allowed a container after failed injection")
	}
	stored, err := store.Get(plan.ClaimUID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PlanRecovering {
		t.Fatalf("failed plan phase=%s, want %s", stored.Phase, PlanRecovering)
	}

	injector.err = nil
	if _, _, err := plugin.CreateContainer(context.Background(), sandbox, &api.Container{Name: "init"}); err != nil {
		t.Fatalf("CreateContainer retry failed: %v", err)
	}
	if len(injector.plans) != 2 {
		t.Fatalf("injection attempts=%d, want 2", len(injector.plans))
	}
	if _, _, err := plugin.CreateContainer(context.Background(), sandbox, &api.Container{Name: "app"}); err != nil {
		t.Fatalf("CreateContainer verification failed: %v", err)
	}
	if len(injector.plans) != 2 {
		t.Fatalf("verified container repeated injection: attempts=%d", len(injector.plans))
	}
	if injector.verifyCalls != 1 {
		t.Fatalf("verification calls=%d, want 1", injector.verifyCalls)
	}
}

func TestCreateContainerPassesPodWithoutAnchorClaims(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ordinary", Namespace: "default", UID: types.UID("ordinary-uid"),
	}}
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	injector := &recordingInjector{}
	plugin := NewNRIPlugin(fake.NewSimpleClientset(pod), nil, store, injector, slog.Default())
	sandbox := &api.PodSandbox{Name: pod.Name, Namespace: pod.Namespace, Uid: string(pod.UID)}
	if _, _, err := plugin.CreateContainer(context.Background(), sandbox, &api.Container{Name: "app"}); err != nil {
		t.Fatalf("ordinary pod was blocked: %v", err)
	}
	if len(injector.plans) != 0 || injector.verifyCalls != 0 {
		t.Fatalf("ordinary pod reached injector: inject=%d verify=%d", len(injector.plans), injector.verifyCalls)
	}
}

func TestInjectedPlanRepairsDriftWithoutDoubleCountingFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		injectErr error
		wantPhase PlanPhase
	}{
		{name: "repair succeeds", wantPhase: PlanInjected},
		{name: "repair fails", injectErr: errors.New("repair failed"), wantPhase: PlanRecovering},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewPlanStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			plan := testPlan()
			plan.Phase = PlanInjected
			plan.NetworkNamespace = "/proc/1/ns/net"
			if err := store.Save(plan); err != nil {
				t.Fatal(err)
			}
			injector := &recordingInjector{verifyErr: errors.New("link drift"), err: test.injectErr}
			plugin := NewNRIPlugin(nil, nil, store, injector, slog.Default())
			sandbox := &api.PodSandbox{Name: plan.PodName, Namespace: plan.PodNamespace, Uid: plan.PodUID}
			err = plugin.injectPlans(context.Background(), sandbox, []PodNetworkPlan{plan}, NetworkNamespace{Path: plan.NetworkNamespace})
			if (err != nil) != (test.injectErr != nil) {
				t.Fatalf("repair error=%v, want failure=%v", err, test.injectErr != nil)
			}
			stored, err := store.Get(plan.ClaimUID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Phase != test.wantPhase || stored.RetryCount != 1 {
				t.Fatalf("stored phase=%s retries=%d, want phase=%s retries=1", stored.Phase, stored.RetryCount, test.wantPhase)
			}
			if injector.verifyCalls != 1 || len(injector.plans) != 1 {
				t.Fatalf("repair calls: verify=%d inject=%d, want 1 each", injector.verifyCalls, len(injector.plans))
			}
		})
	}
}

func TestNRIInjectionPersistsSuccessAndFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		injectErr error
		wantPhase PlanPhase
	}{
		{name: "success", wantPhase: PlanInjected},
		{name: "failure", injectErr: errors.New("injected failure"), wantPhase: PlanRecovering},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewPlanStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			plan := testPlan()
			if err := store.Save(plan); err != nil {
				t.Fatal(err)
			}
			injector := &recordingInjector{err: test.injectErr}
			plugin := NewNRIPlugin(nil, nil, store, injector, slog.Default())
			sandbox := &api.PodSandbox{Name: plan.PodName, Namespace: plan.PodNamespace, Uid: plan.PodUID}
			err = plugin.injectPlans(context.Background(), sandbox, []PodNetworkPlan{plan}, NetworkNamespace{Path: "/proc/1/ns/net"})
			if (err != nil) != (test.injectErr != nil) {
				t.Fatalf("unexpected injection error: %v", err)
			}
			stored, getErr := store.Get(plan.ClaimUID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if stored.Phase != test.wantPhase {
				t.Fatalf("phase=%s, want %s", stored.Phase, test.wantPhase)
			}
			if test.injectErr != nil && (stored.RetryCount != 1 || stored.FirstFailureAt == nil) {
				t.Fatalf("failure observation was not persisted: %#v", stored)
			}
		})
	}
}

func TestSynchronizeIsolatesWorkloadInjectionFailure(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	injector := &recordingInjector{err: errors.New("workload injection failed")}
	plugin := NewNRIPlugin(nil, nil, store, injector, slog.Default())
	_, err = plugin.Synchronize(context.Background(), []*api.PodSandbox{{
		Name: plan.PodName, Namespace: plan.PodNamespace, Uid: plan.PodUID, Pid: 1,
	}}, nil)
	if err != nil {
		t.Fatalf("workload failure escaped Synchronize: %v", err)
	}
	if !plugin.Healthy() {
		t.Fatal("workload failure incorrectly marked the NRI plugin unhealthy")
	}
	stored, err := store.Get(plan.ClaimUID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PlanRecovering {
		t.Fatalf("failed workload phase=%s, want Recovering", stored.Phase)
	}
}

func TestSynchronizeIsolatesUnrelatedCorruptPlan(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "corrupt.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := NewNRIPlugin(nil, nil, store, &recordingInjector{}, slog.Default())
	_, err = plugin.Synchronize(context.Background(), []*api.PodSandbox{{
		Name: plan.PodName, Namespace: plan.PodNamespace, Uid: plan.PodUID, Pid: 1,
	}}, nil)
	if err != nil {
		t.Fatalf("unrelated corrupt plan escaped Synchronize: %v", err)
	}
	if !plugin.Healthy() {
		t.Fatal("unrelated corrupt plan incorrectly marked the plugin unhealthy")
	}
}

func TestSynchronizeMarksGlobalPlanStoreFailureUnhealthy(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.dir); err != nil {
		t.Fatal(err)
	}
	plugin := NewNRIPlugin(nil, nil, store, &recordingInjector{}, slog.Default())
	if _, err := plugin.Synchronize(context.Background(), []*api.PodSandbox{{Uid: "pod-uid"}}, nil); err == nil {
		t.Fatal("expected inaccessible plan store to fail synchronization")
	}
	if plugin.Healthy() {
		t.Fatal("global plan-store failure left the plugin healthy")
	}
}

func TestNRIReconcilePersistsRepair(t *testing.T) {
	store, err := NewPlanStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	plan.Phase = PlanRecovering
	plan.NetworkNamespace = "/proc/1/ns/net"
	if err := store.Save(plan); err != nil {
		t.Fatal(err)
	}
	injector := &recordingInjector{reconcileResults: []PlanReconcileResult{{Plan: plan, Drifted: true, DriftErr: errors.New("address drift")}}}
	plugin := NewNRIPlugin(nil, nil, store, injector, slog.Default())
	if err := plugin.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(plan.ClaimUID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PlanInjected || stored.FirstFailureAt != nil || stored.LastError != "" || stored.RetryCount != 1 {
		t.Fatalf("repaired plan was not cleared: %#v", stored)
	}
}
