package rehearsal

import (
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

// Coverage records actual synthetic-table checks, not production-data coverage.
type Coverage struct {
	Synthetic bool            `json:"synthetic"`
	Tables    []TableCoverage `json:"tables"`
}

type TableCoverage struct {
	Table       string `json:"table"`
	SourceRows  int64  `json:"sourceRows"`
	TargetRows  int64  `json:"targetRows"`
	CheckedRows int64  `json:"checkedRows"`
}

// fixtureCoverage checks the engine's in-memory results before accepting a run.
// The engine treats an empty table selection as success, so Outcome alone is insufficient.
func fixtureCoverage(st engine.State, schemas []string, rows int) (*Coverage, error) {
	fence, post := st.Steps[engine.StepSwitchover], st.Steps[engine.StepVerify]
	if st.Outcome != engine.OutcomeSucceeded || fence == nil || post == nil || fence.Status != engine.StatusSucceeded || post.Status != engine.StatusSucceeded {
		return nil, fmt.Errorf("engine verification steps did not succeed")
	}
	gate, _ := fence.Evidence["fenceGate"].(map[string]any)
	compared, _ := gate["tables"].([]verify.TableResult)
	report, _ := post.Evidence["report"].(*engine.Report)
	names := fixtureTables(schemas)
	if gate["ok"] != true || report == nil || !report.OK || len(report.Problems) != 0 || report.MissingRows != 0 || len(compared) != len(names) || len(report.Tables) != len(names) {
		return nil, fmt.Errorf("both verification stages must check every fixture table")
	}
	expected := map[string]int64{}
	for _, name := range names {
		count := int64(rows)
		if strings.HasSuffix(name, ".rehearsal_items") {
			count = int64(rows/4) * 3
		}
		expected[name] = count
	}
	counts := map[string]verify.TableResult{}
	for _, r := range compared {
		want, known := expected[r.Table]
		_, duplicate := counts[r.Table]
		if !known || duplicate || want <= 0 || r.Error != "" || !r.Match || r.SourceCount != want || r.TargetCount != want {
			return nil, fmt.Errorf("incomplete fence comparison for %s", r.Table)
		}
		counts[r.Table] = r
	}
	checked := map[string]int64{}
	for _, r := range report.Tables {
		want, known := expected[r.Table]
		_, duplicate := checked[r.Table]
		if !known || duplicate || r.Error != "" || r.Skipped != "" || r.Missing != 0 || r.Checked != want {
			return nil, fmt.Errorf("incomplete post-cutover check for %s", r.Table)
		}
		checked[r.Table] = r.Checked
	}
	coverage := &Coverage{Synthetic: true}
	for _, name := range names {
		r := counts[name]
		coverage.Tables = append(coverage.Tables, TableCoverage{Table: name, SourceRows: r.SourceCount, TargetRows: r.TargetCount, CheckedRows: checked[name]})
	}
	return coverage, nil
}
