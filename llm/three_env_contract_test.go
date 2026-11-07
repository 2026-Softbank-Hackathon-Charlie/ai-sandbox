package llm

import (
	"context"
	"encoding/json"
	"testing"
)

func TestProposalSchemaIncludesSourceRewireBoolean(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(proposalSchema, &schema); err != nil {
		t.Fatal(err)
	}
	pl := schema["properties"].(map[string]any)["plan"].(map[string]any)
	property, exists := pl["properties"].(map[string]any)["allowSourceRewire"].(map[string]any)
	if !exists || property["type"] != "boolean" {
		t.Fatal("strict proposal schema discards server source rewire opt-in")
	}
	for _, required := range pl["required"].([]any) {
		if required == "allowSourceRewire" {
			t.Fatal("legacy drafts must not require the optional flag")
		}
	}
}

func TestModelCannotAddRemoveOrChangeSourceRewire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		model   any
	}{
		{"remove enabled", true, nil},
		{"disable enabled", true, false},
		{"enable legacy", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := draft(t)
			d.AllowSourceRewire = tc.enabled
			answer := func(flag any) string {
				var out map[string]any
				if err := json.Unmarshal([]byte(readFile(t, "../plan/testdata/proposal.json")), &out); err != nil {
					t.Fatal(err)
				}
				if flag != nil {
					out["plan"].(map[string]any)["allowSourceRewire"] = flag
				}
				raw, _ := json.Marshal(out)
				return string(raw)
			}
			f := &fakeAPI{replies: []reply{{text: answer(tc.model)}, {text: answer(tc.enabled)}}}
			a, _ := newAgent(t, f)
			p, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: d})
			if err != nil || p == nil {
				t.Fatalf("corrected model response failed: %v", err)
			}
			if len(f.requests) != 2 {
				t.Fatal("model-controlled opt-in passed validation instead of retrying")
			}
			if p.Plan.AllowSourceRewire != tc.enabled || d.AllowSourceRewire != tc.enabled {
				t.Fatal("final proposal did not retain the caller's original permission")
			}
		})
	}
}
