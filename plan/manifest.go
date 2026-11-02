package plan

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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
	if v := strOf(doc, "apiVersion"); v != "postgresql.cnpg.io/v1" {
		is.add("apiVersion", "postgresql.cnpg.io/v1이어야 함 (받은 값 %q)", v)
	}
	if v := strOf(doc, "kind"); v != "Cluster" {
		is.add("kind", "Cluster여야 함 (받은 값 %q)", v)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		is.add("metadata", "있어야 함")
		meta = map[string]any{}
	}
	if n := strOf(meta, "name"); n != p.Target.Cluster {
		is.add("metadata.name", "Plan의 target.cluster %q와 같아야 함 (받은 값 %q)", p.Target.Cluster, n)
	}
	if ns := strOf(meta, "namespace"); ns != "" && ns != p.Target.Namespace {
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
		// 엔진의 분산 토폴로지 검사(checkTopology)와 같은 규칙이다
		if src := strOf(rep, "primary"); src != p.Source.Cluster {
			is.add("spec.replica.primary", "원본 클러스터 %q여야 함 (받은 값 %q)", p.Source.Cluster, src)
		}
		if src := strOf(rep, "source"); src != p.Source.Cluster {
			is.add("spec.replica.source", "원본 클러스터의 externalCluster 이름 %q여야 함 (받은 값 %q)", p.Source.Cluster, src)
		}
		if self := strOf(rep, "self"); self != "" && self != p.Target.Cluster {
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
		if bb, ok := b["pg_basebackup"].(map[string]any); ok {
			is = append(is, checkBasebackup(bb, p)...)
		}
	}
	if v, ok := spec["imageName"]; ok {
		if s, _ := v.(string); !cnpgImage.MatchString(s) {
			is.add("spec.imageName", "CloudNativePG PostgreSQL 이미지(ghcr.io/cloudnative-pg/postgresql:<버전>)여야 함 (받은 값 %v)", v)
		}
	}
	is = append(is, checkExternalClusters(spec, p)...)
	return is
}

// cnpgImage는 대상 DB Pod에 쓸 수 있는 이미지다. 복제된 운영 데이터와 복제 인증서를 가진 채 뜨므로
// CloudNativePG가 배포하는 PostgreSQL 이미지만 받는다.
var cnpgImage = regexp.MustCompile(`^ghcr\.io/cloudnative-pg/postgresql(:[0-9]+(\.[0-9]+)?(-[0-9A-Za-z.-]+)?)?(@sha256:[0-9a-f]{64})?$`)

// checkBasebackup은 대상이 원본에서, 원본의 앱 계정 그대로 시작하는지 본다 (migration-engine examples/cnpg).
func checkBasebackup(bb map[string]any, p *eplan.Plan) Issues {
	var is Issues
	for _, k := range sortedKeys(bb) {
		if !slices.Contains([]string{"source", "database", "owner", "secret"}, k) {
			is.add("spec.bootstrap.pg_basebackup."+k, "쓸 수 없는 키 (허용: source, database, owner, secret)")
		}
	}
	if src := strOf(bb, "source"); src != p.Source.Cluster {
		is.add("spec.bootstrap.pg_basebackup.source", "원본 클러스터 %q여야 함 (받은 값 %q)", p.Source.Cluster, src)
	}
	u, err := dsnURL(p.Source.DSN)
	if err != nil {
		is.add("spec.bootstrap.pg_basebackup", "원본 DSN을 읽을 수 없음: %v", err)
		return is
	}
	if db, ok := bb["database"]; ok && db != strings.TrimPrefix(u.Path, "/") {
		is.add("spec.bootstrap.pg_basebackup.database", "원본 DSN의 DB %q여야 함 (받은 값 %v)", strings.TrimPrefix(u.Path, "/"), db)
	}
	if owner, ok := bb["owner"]; ok && owner != u.User.Username() {
		is.add("spec.bootstrap.pg_basebackup.owner", "원본 DSN의 사용자 %q여야 함 (받은 값 %v)", u.User.Username(), owner)
	}
	if sec, ok := bb["secret"]; ok {
		m, _ := sec.(map[string]any)
		if want := p.Source.Cluster + "-app"; m == nil || len(m) != 1 || strOf(m, "name") != want {
			is.add("spec.bootstrap.pg_basebackup.secret", "{name: %s}여야 함 (원본의 앱 계정)", want)
		}
	}
	return is
}

// checkExternalClusters는 복제 접속 정보가 Plan의 원본·대상 DSN과 같은 곳을 가리키는지 본다.
// CheckAgainstDraft는 Plan의 DSN만 비교하므로, 여기서 막지 않으면 매니페스트로 다른 DB에서 복제할 수 있다.
func checkExternalClusters(spec map[string]any, p *eplan.Plan) Issues {
	var is Issues
	raw, present := spec["externalClusters"]
	list, ok := raw.([]any)
	if present && !ok {
		is.add("spec.externalClusters", "목록이어야 함")
		return is
	}
	endpoints := map[string]eplan.Endpoint{p.Source.Cluster: p.Source, p.Target.Cluster: p.Target}
	seen := map[string]bool{}
	for i, e := range list {
		path := fmt.Sprintf("spec.externalClusters[%d]", i)
		m, _ := e.(map[string]any)
		if m == nil {
			is.add(path, "객체여야 함")
			continue
		}
		name := strOf(m, "name")
		ep, ok := endpoints[name]
		if !ok {
			is.add(path+".name", "원본 %q 또는 대상 %q만 쓸 수 있음 (받은 값 %q)", p.Source.Cluster, p.Target.Cluster, name)
			continue
		}
		if seen[name] {
			is.add(path+".name", "%q가 두 번 나옴", name)
		}
		seen[name] = true
		for _, k := range sortedKeys(m) {
			if !slices.Contains([]string{"name", "connectionParameters", "sslKey", "sslCert", "sslRootCert"}, k) {
				is.add(path+"."+k, "쓸 수 없는 키 (허용: name, connectionParameters, sslKey, sslCert, sslRootCert)")
			}
		}
		is = append(is, checkConnection(path+".connectionParameters", m["connectionParameters"], ep)...)
		for k, want := range map[string]string{"sslKey": name + "-replication", "sslCert": name + "-replication", "sslRootCert": name + "-ca"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			ref, _ := v.(map[string]any)
			if ref == nil || strOf(ref, "name") != want {
				is.add(path+"."+k, "시크릿 이름은 %q여야 함", want)
			}
		}
	}
	if !seen[p.Source.Cluster] {
		is.add("spec.externalClusters", "원본 %q 항목이 있어야 함 (replica.source가 가리킴)", p.Source.Cluster)
	}
	sort.Slice(is, func(i, j int) bool { return is[i].Path < is[j].Path })
	return is
}

func checkConnection(path string, v any, ep eplan.Endpoint) Issues {
	var is Issues
	cp, _ := v.(map[string]any)
	if cp == nil {
		is.add(path, "있어야 함")
		return is
	}
	u, err := dsnURL(ep.DSN)
	if err != nil {
		is.add(path, "%s DSN을 읽을 수 없음: %v", ep.Env, err)
		return is
	}
	for _, k := range sortedKeys(cp) {
		if !slices.Contains([]string{"host", "port", "user", "dbname", "sslmode"}, k) {
			is.add(path+"."+k, "쓸 수 없는 키 (허용: host, port, user, dbname, sslmode)")
		}
	}
	if h := strOf(cp, "host"); h != u.Hostname() {
		is.add(path+".host", "%s DSN의 호스트 %q여야 함 (받은 값 %q)", ep.Env, u.Hostname(), h)
	}
	if port, ok := cp["port"]; ok && fmt.Sprint(port) != orDefault(u.Port(), "5432") {
		is.add(path+".port", "%s DSN의 포트 %s여야 함 (받은 값 %v)", ep.Env, orDefault(u.Port(), "5432"), port)
	}
	if user := strOf(cp, "user"); user != "streaming_replica" {
		is.add(path+".user", "streaming_replica여야 함 (받은 값 %q)", user)
	}
	return is
}

// envRef는 플랫폼 DSN의 ${VAR}다. URL 파서가 {}를 받지 않으므로 자리표시로 바꾼 뒤 읽는다.
var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

func dsnURL(dsn string) (*url.URL, error) {
	u, err := url.Parse(envRef.ReplaceAllString(dsn, "x"))
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.User == nil {
		return nil, fmt.Errorf("postgres://사용자@호스트/DB 형식이 아님")
	}
	return u, nil
}

func strOf(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
