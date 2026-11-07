package rehearsal

import (
	"encoding/json"
	"testing"
	"time"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

func TestSandboxPlanPreservesSourceRewirePermission(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p := proposal(t)
		p.Plan.AllowSourceRewire = enabled
		sp := SandboxPlan(&p.Plan, "sandbox-three-env", "fixture-kubeconfig", "fixture-target.yaml", "fixture-src-dsn", "fixture-dst-dsn")
		if sp.AllowSourceRewire != enabled || p.Plan.AllowSourceRewire != enabled {
			t.Fatal("sandbox adaptation changed the approved source rewire permission")
		}
		raw, err := json.Marshal(sp)
		if err != nil {
			t.Fatal(err)
		}
		var replayed eplan.Plan
		if err := json.Unmarshal(raw, &replayed); err != nil || replayed.AllowSourceRewire != enabled {
			t.Fatalf("sandbox JSON lost opt-in=%v: %v", enabled, err)
		}
		// The temporary source already points at its target: this contract
		// test exercises no infrastructure and makes no real topology patch.
		source := SourceManifest(sp, "sandbox-three-env", DBInfo{Name: "app", User: "app"})
		rep := source.Object["spec"].(map[string]any)["replica"].(map[string]any)
		if rep["primary"] != sp.Source.Cluster || rep["source"] != sp.Target.Cluster {
			t.Fatal("sandbox source no longer forms the strict migration pair")
		}
	}
}

func TestSandboxFenceGateBudgetMatchesEngineDefaults(t *testing.T) {
	for _, configured := range []time.Duration{0, 7 * time.Second} {
		p := proposal(t)
		p.Plan.Thresholds.MaxFenceGate.Duration = configured
		sp := SandboxPlan(&p.Plan, "sandbox-three-env", "fixture-kubeconfig", "fixture-target.yaml", "fixture-src-dsn", "fixture-dst-dsn")
		// Compare direct struct execution with the engine's actual file-plan
		// defaults, using fixture DSNs without any process-secret references.
		loaded := *sp
		loaded.Thresholds.MaxFenceGate.Duration = configured
		raw, _ := json.Marshal(loaded)
		defaults, err := eplan.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		want := defaults.Thresholds.MaxFenceGate.Duration
		if sp.Thresholds.MaxFenceGate.Duration != want || p.Plan.Thresholds.MaxFenceGate.Duration != configured {
			t.Fatalf("gate budget default changed caller or was omitted: got=%v want=%v", sp.Thresholds.MaxFenceGate, want)
		}
	}
}
