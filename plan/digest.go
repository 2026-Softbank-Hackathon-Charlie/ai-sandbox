package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Digest는 리허설한 후보(Plan과 대상 매니페스트)를 가리킨다 (platform #24).
// 플랫폼은 POST /v1/plans 본문을 그대로 저장하고 첨부마다 SHA-256을 기록하므로,
// 승인할 때 저장된 Plan 본문과 target.yaml 첨부의 해시를 이 값과 비교하면 같은 후보인지 알 수 있다.
// revision은 넣지 않는다. 리포트를 첨부하는 순간 revision이 올라가므로 자기 참조가 된다.
type Digest struct {
	// PlanSHA256은 플랫폼에 제출하는 본문(SubmitBody)의 hex SHA-256이다.
	PlanSHA256 string `json:"planSha256"`
	// ManifestSHA256은 target.yaml 첨부 내용의 hex SHA-256이다. 매니페스트를 쓰지 않는 Plan이면 비어 있다.
	ManifestSHA256 string `json:"manifestSha256,omitempty"`
}

// SubmitBody는 Plan을 플랫폼에 제출할 때 보내는 본문이다. 제출과 Digest가 같은 바이트를 쓰도록 여기서만 만든다.
func (p *Proposal) SubmitBody() ([]byte, error) {
	return json.Marshal(p.Plan)
}

// ManifestAttached는 target.yaml을 첨부로 올리는 Plan인지 알려준다.
func (p *Proposal) ManifestAttached() bool {
	return p.Plan.Target.Manifest == ManifestRef
}

// Digest는 제출 본문과 첨부할 매니페스트의 해시를 계산한다.
func (p *Proposal) Digest() (Digest, error) {
	body, err := p.SubmitBody()
	if err != nil {
		return Digest{}, err
	}
	d := Digest{PlanSHA256: SHA256Hex(body)}
	if p.ManifestAttached() {
		d.ManifestSHA256 = SHA256Hex([]byte(p.Manifest))
	}
	return d, nil
}

// SHA256Hex는 플랫폼 첨부의 sha256과 같은 형식(소문자 hex)으로 해시한다.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
