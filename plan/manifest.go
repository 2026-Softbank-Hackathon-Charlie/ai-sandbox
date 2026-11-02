package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"sigs.k8s.io/yaml"
)

// AI가 쓸 수 있는 CNPG Cluster spec 키. 계정, 슈퍼유저, 백업 대상처럼 권한이나 데이터 위치를 바꾸는 키는 뺐다.
var manifestSpecKeys = []string{
	"instances", "imageName", "storage", "walStorage", "resources", "affinity",
	"smartShutdownTimeout", "bootstrap", "replica", "externalClusters", "monitoring", "postgresql",
}

// CheckManifest는 대상 매니페스트가 이 Plan의 대상 클러스터를 원본의 복제본으로 만드는지 본다.
// 엔진은 prepare 단계에서 대상 클러스터가 없을 때만 이 매니페스트를 적용한다.
func CheckManifest(manifest string, p *eplan.Plan) Issues {
	var is Issues
	doc, err := parseYAMLObject(manifest)
	if err != nil {
		is.add("", "YAML 객체 하나여야 함: %v", err)
		return is
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	if v := str(doc, "apiVersion"); v != "postgresql.cnpg.io/v1" {
		is.add("apiVersion", "postgresql.cnpg.io/v1이어야 함 (받은 값 %q)", v)
	}
	if v := str(doc, "kind"); v != "Cluster" {
		is.add("kind", "Cluster여야 함 (받은 값 %q)", v)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		is.add("metadata", "있어야 함")
		meta = map[string]any{}
	}
	if n := str(meta, "name"); n != p.Target.Cluster {
		is.add("metadata.name", "Plan의 target.cluster %q와 같아야 함 (받은 값 %q)", p.Target.Cluster, n)
	}
	if ns := str(meta, "namespace"); ns != "" && ns != p.Target.Namespace {
		is.add("metadata.namespace", "비우거나 Plan의 target.namespace %q여야 함 (받은 값 %q)", p.Target.Namespace, ns)
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		is.add("spec", "있어야 함")
		return is
	}
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(manifestSpecKeys, k) {
			is.add("spec."+k, "쓸 수 없는 키 (허용: %s)", strings.Join(manifestSpecKeys, ", "))
		}
	}
	if n, ok := spec["instances"].(float64); ok && (n < 1 || n > 5 || n != float64(int(n))) {
		is.add("spec.instances", "1~5인 정수여야 함 (받은 값 %v)", n)
	}
	// 대상은 원본의 복제본으로 시작해야 한다. 아니면 빈 DB가 생기고 동기화가 성립하지 않는다.
	rep, _ := spec["replica"].(map[string]any)
	if rep == nil {
		is.add("spec.replica", "원본 %q의 복제본 설정이 있어야 함", p.Source.Cluster)
	} else {
		if src := str(rep, "primary"); src != p.Source.Cluster {
			is.add("spec.replica.primary", "원본 클러스터 %q여야 함 (받은 값 %q)", p.Source.Cluster, src)
		}
		if self := str(rep, "self"); self != "" && self != p.Target.Cluster {
			is.add("spec.replica.self", "대상 클러스터 %q여야 함 (받은 값 %q)", p.Target.Cluster, self)
		}
	}
	if pg, ok := spec["postgresql"].(map[string]any); ok {
		for k := range pg {
			if k != "parameters" {
				is.add("spec.postgresql."+k, "postgresql 아래에는 parameters만 쓸 수 있음")
			}
		}
	}
	if b, ok := spec["bootstrap"].(map[string]any); ok {
		for k := range b {
			if k != "pg_basebackup" {
				is.add("spec.bootstrap."+k, "복제본은 pg_basebackup으로만 시작함")
			}
		}
	}
	return is
}

// 진단 수정안이 고칠 수 있는 매니페스트 경로. 대상 환경 차이(StorageClass, 크기, 자원, 이미지)만 고친다.
func manifestPathAllowed(seg []string) bool {
	if len(seg) < 2 || seg[0] != "spec" {
		return false
	}
	switch seg[1] {
	case "storage", "walStorage":
		return len(seg) == 3 && (seg[2] == "storageClass" || seg[2] == "size")
	case "instances", "imageName", "smartShutdownTimeout":
		return len(seg) == 2
	case "resources":
		return len(seg) >= 3
	}
	return false
}

func parseYAMLObject(s string) (map[string]any, error) {
	if strings.Contains(s, "\n---") {
		return nil, fmt.Errorf("문서가 여러 개임")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("비어 있음")
	}
	return doc, nil
}

// splitPointer는 JSON Pointer(RFC 6901)를 조각으로 나눈다.
func splitPointer(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("경로는 /로 시작해야 함 (받은 값 %q)", path)
	}
	seg := strings.Split(path[1:], "/")
	for i, s := range seg {
		if s == "" {
			return nil, fmt.Errorf("빈 경로 조각이 있음 (받은 값 %q)", path)
		}
		seg[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return seg, nil
}

// setPointer는 JSON Pointer 위치에 값을 넣는다. mustExist면 replace(이미 있어야 함), 아니면 add(부모만 있으면 됨).
func setPointer(doc any, seg []string, v any, mustExist bool) error {
	cur := doc
	for i, s := range seg {
		last := i == len(seg)-1
		switch node := cur.(type) {
		case map[string]any:
			child, ok := node[s]
			if last {
				if mustExist && !ok {
					return fmt.Errorf("replace할 키 %q가 없음", s)
				}
				node[s] = v
				return nil
			}
			if !ok {
				return fmt.Errorf("중간 키 %q가 없음", s)
			}
			cur = child
		case []any:
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 || n >= len(node) {
				return fmt.Errorf("배열 번호 %q가 범위 밖", s)
			}
			if last {
				node[n] = v
				return nil
			}
			cur = node[n]
		default:
			return fmt.Errorf("%q 아래로 내려갈 수 없음", s)
		}
	}
	return nil
}

// patchYAML은 YAML 문서 사본에 연산을 적용한다.
func patchYAML(src string, ops []Op) (string, error) {
	doc, err := parseYAMLObject(src)
	if err != nil {
		return "", err
	}
	var root any = doc
	for i, op := range ops {
		seg, err := splitPointer(op.Path)
		if err != nil {
			return "", fmt.Errorf("patch[%d]: %w", i, err)
		}
		var v any
		if err := json.Unmarshal(op.Value, &v); err != nil {
			return "", fmt.Errorf("patch[%d] 값을 읽을 수 없음: %w", i, err)
		}
		if err := setPointer(root, seg, v, op.Op == "replace"); err != nil {
			return "", fmt.Errorf("patch[%d] %s: %w", i, op.Path, err)
		}
	}
	out, err := yaml.Marshal(root)
	return string(out), err
}
