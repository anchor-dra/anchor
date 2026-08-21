package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/anchor-dra/anchor/internal/model"
)

type PlanPhase string

const (
	PlanPrepared         PlanPhase = "Prepared"
	PlanInjectionPending PlanPhase = "InjectionPending"
	PlanInjected         PlanPhase = "Injected"
	PlanRecovering       PlanPhase = "Recovering"
	PlanUnpreparing      PlanPhase = "Unpreparing"
	PlanRetained         PlanPhase = "Retained"
)

type PodNetworkPlan struct {
	Version             int                   `json:"version"`
	PodNamespace        string                `json:"podNamespace"`
	PodName             string                `json:"podName"`
	PodUID              string                `json:"podUID"`
	ClaimNamespace      string                `json:"claimNamespace"`
	ClaimName           string                `json:"claimName"`
	ClaimUID            string                `json:"claimUID"`
	RequestName         string                `json:"requestName"`
	PoolName            string                `json:"poolName"`
	DeviceName          string                `json:"deviceName"`
	Strategy            string                `json:"strategy,omitempty"`
	PlacementName       string                `json:"placementName"`
	PlacementGeneration int64                 `json:"placementGeneration,omitempty"`
	Paths               []model.PlacementPath `json:"paths"`
	Phase               PlanPhase             `json:"phase"`
	NetworkNamespace    string                `json:"networkNamespace,omitempty"`
	LastObservation     time.Time             `json:"lastObservation"`
	FirstFailureAt      *time.Time            `json:"firstFailureAt,omitempty"`
	LastError           string                `json:"lastError,omitempty"`
	RetryCount          int64                 `json:"retryCount,omitempty"`
}

func (p *PodNetworkPlan) validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported plan version %d", p.Version)
	}
	if p.PodUID == "" || p.ClaimUID == "" || p.PlacementName == "" {
		return errors.New("plan is missing pod, claim, or placement identity")
	}
	if len(p.Paths) == 0 {
		return errors.New("plan contains no network paths")
	}
	return nil
}

type PlanStore struct {
	dir string
	mu  sync.Mutex
}

func NewPlanStore(dir string) (*PlanStore, error) {
	if dir == "" {
		return nil, errors.New("plan state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create plan state directory: %w", err)
	}
	return &PlanStore{dir: dir}, nil
}

func (s *PlanStore) Save(plan PodNetworkPlan) error {
	if err := plan.validate(); err != nil {
		return err
	}
	plan.LastObservation = time.Now().UTC()
	encoded, err := json.MarshalIndent(&plan, "", "  ")
	if err != nil {
		return fmt.Errorf("encode network plan: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	temporary, err := os.CreateTemp(s.dir, ".plan-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary network plan: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary network plan: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary network plan: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary network plan: %w", err)
	}
	if err := os.Rename(temporaryName, s.path(plan.ClaimUID)); err != nil {
		return fmt.Errorf("commit network plan: %w", err)
	}
	return nil
}

func (s *PlanStore) Get(claimUID string) (*PodNetworkPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(s.path(claimUID))
}

func (s *PlanStore) ForPod(podUID string) ([]PodNetworkPlan, error) {
	plans, _, err := s.LoadAllLenient()
	if err != nil {
		return nil, err
	}
	result := make([]PodNetworkPlan, 0, len(plans))
	for _, plan := range plans {
		if plan.PodUID == podUID && plan.Phase != PlanRetained {
			result = append(result, plan)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ClaimUID < result[j].ClaimUID })
	return result, nil
}

// LoadAllLenient isolates corrupt plan files so one workload cannot make the
// node plugin unavailable. Callers still receive per-file errors for logging
// and fail the affected workload closed through the claim/plan count check.
func (s *PlanStore) LoadAllLenient() ([]PodNetworkPlan, []error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("list network plans: %w", err)
	}
	plans := make([]PodNetworkPlan, 0, len(entries))
	var planErrors []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		plan, err := s.read(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			planErrors = append(planErrors, err)
			continue
		}
		plans = append(plans, *plan)
	}
	return plans, planErrors, nil
}

func (s *PlanStore) LoadAll() ([]PodNetworkPlan, error) {
	plans, planErrors, err := s.LoadAllLenient()
	return plans, errors.Join(append(planErrors, err)...)
}

func (s *PlanStore) Delete(claimUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(claimUID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete network plan for claim %s: %w", claimUID, err)
	}
	return nil
}

func (s *PlanStore) path(claimUID string) string {
	return filepath.Join(s.dir, model.SafeName(claimUID)+".json")
}

func (s *PlanStore) read(path string) (*PodNetworkPlan, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plan PodNetworkPlan
	if err := json.Unmarshal(encoded, &plan); err != nil {
		return nil, fmt.Errorf("decode network plan %s: %w", path, err)
	}
	if err := plan.validate(); err != nil {
		return nil, fmt.Errorf("validate network plan %s: %w", path, err)
	}
	return &plan, nil
}
