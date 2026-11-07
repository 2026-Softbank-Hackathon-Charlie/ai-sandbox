package plan

import (
	"encoding/json"
	"testing"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestSourceRewireMustMatchServerDraft(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, model := range []any{nil, false, true, "true"} {
			d := draft(t)
			d.AllowSourceRewire = enabled
			var answer map[string]any
			if err := json.Unmarshal([]byte(read(t, "proposal.json")), &answer); err != nil {
				t.Fatal(err)
			}
			if model != nil {
				answer["plan"].(map[string]any)["allowSourceRewire"] = model
			}
			raw, _ := json.Marshal(answer)
			p, issues := Parse(raw, d)
			got, boolean := model.(bool)
			wantValid := (model == nil && !enabled) || (boolean && got == enabled)
			if wantValid {
				if len(issues) != 0 || p.Plan.AllowSourceRewire != enabled {
					t.Fatalf("draft=%v model=%v rejected: %v", enabled, model, issues)
				}
			} else if len(issues) == 0 {
				t.Fatalf("draft=%v accepted model-controlled opt-in %v", enabled, model)
			}
		}
	}
}

func TestSourceRewireTypedDraftSubmitAndDigestRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		d := draft(t)
		d.AllowSourceRewire = enabled
		raw, _ := json.Marshal(d)
		// The CLI receives a typed platform EnginePlan before decoding the
		// engine Plan. Both dependencies must preserve this server-owned flag.
		var apiDraft apiv1.EnginePlan
		if err := json.Unmarshal(raw, &apiDraft); err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(apiDraft)
		var engineDraft eplan.Plan
		if err := json.Unmarshal(raw, &engineDraft); err != nil || engineDraft.AllowSourceRewire != enabled {
			t.Fatalf("typed platform draft lost opt-in=%v: %v", enabled, err)
		}
		p := Proposal{Plan: engineDraft}
		body, err := p.SubmitBody()
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		if _, exists := fields["allowSourceRewire"]; exists != enabled {
			t.Fatalf("legacy omission/true contract changed: %s", body)
		}
		var submitted apiv1.EnginePlan
		if err := json.Unmarshal(body, &submitted); err != nil || (submitted.AllowSourceRewire != nil && *submitted.AllowSourceRewire) != enabled {
			t.Fatalf("submitted opt-in was lost: %v", err)
		}
		before, _ := p.Digest()
		p.Plan.AllowSourceRewire = !enabled
		after, _ := p.Digest()
		if before.PlanSHA256 == after.PlanSHA256 {
			t.Fatal("rehearsal digest did not bind the source rewire permission")
		}
	}
}

func TestDiagnosisCannotPatchSourceRewirePermission(t *testing.T) {
	d := draft(t)
	d.AllowSourceRewire = true
	p := proposal(t)
	p.Plan.AllowSourceRewire = true
	for _, fix := range []Fix{
		{Kind: KindPlanParam, Patch: []Op{{Op: "replace", Path: "/allowSourceRewire", Value: json.RawMessage(`false`)}}},
		{Kind: KindPlanParam, Patch: []Op{{Op: "replace", Path: "/thresholds/maxDowntime", Value: json.RawMessage(`"35s"`)}}},
	} {
		next, issues := fix.Apply(p, d)
		if fix.Patch[0].Path == "/allowSourceRewire" {
			if len(issues) == 0 {
				t.Fatal("diagnosis changed a server-owned source rewire permission")
			}
		} else if len(issues) != 0 || !next.Plan.AllowSourceRewire {
			t.Fatalf("ordinary parameter repair lost source rewire permission: %v", issues)
		}
	}
}
