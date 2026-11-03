package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestDigestMatchesSubmittedBytes(t *testing.T) {
	var p Proposal
	if err := json.Unmarshal([]byte(read(t, "proposal.json")), &p); err != nil {
		t.Fatal(err)
	}
	body, err := p.SubmitBody()
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// 플랫폼이 저장한 본문과 첨부를 sha256으로 다시 계산했을 때와 같아야 한다.
	plan := sha256.Sum256(body)
	manifest := sha256.Sum256([]byte(p.Manifest))
	if d.PlanSHA256 != hex.EncodeToString(plan[:]) || d.ManifestSHA256 != hex.EncodeToString(manifest[:]) {
		t.Fatalf("digest가 제출 바이트와 다름: %+v", d)
	}

	// 값 하나만 바뀌어도 다른 후보다.
	q := p
	q.Manifest += "\n"
	if e, _ := q.Digest(); e.ManifestSHA256 == d.ManifestSHA256 || e.PlanSHA256 != d.PlanSHA256 {
		t.Fatalf("매니페스트만 바꿨는데: %+v", e)
	}
	q = p
	q.Plan.Thresholds.MaxLagBytes++
	if e, _ := q.Digest(); e.PlanSHA256 == d.PlanSHA256 {
		t.Fatal("Plan 값이 바뀌었는데 planSha256이 같음")
	}
}

func TestDigestWithoutManifest(t *testing.T) {
	var p Proposal
	if err := json.Unmarshal([]byte(read(t, "proposal.json")), &p); err != nil {
		t.Fatal(err)
	}
	p.Plan.Target.Manifest = ""
	d, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d.ManifestSHA256 != "" {
		t.Fatalf("첨부하지 않는 매니페스트의 해시가 들어감: %+v", d)
	}
}
