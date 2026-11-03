package plan

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// 엔진은 이 매니페스트를 대상 클러스터에 Force로 적용한다. 그래서 최상위와 metadata도 허용 목록으로 묶는다.
// annotations(cnpg.io/hibernation, cnpg.io/reconciliationLoop), finalizers, ownerReferences로
// 대상 DB를 멈추거나 만들자마자 지우게 할 수 있기 때문이다.
var (
	manifestTopKeys  = []string{"apiVersion", "kind", "metadata", "spec"}
	manifestMetaKeys = []string{"name", "namespace"}
)

// AI가 쓸 수 있는 CNPG Cluster spec 키. 계정, 슈퍼유저, 백업 대상처럼 권한이나 데이터 위치를 바꾸는 키는 뺐다.
// affinity(노드 선택)와 monitoring(사용자 쿼리를 메트릭으로 내보냄)도 이전에 필요 없어 뺐다.
var manifestSpecKeys = []string{
	"instances", "imageName", "storage", "walStorage", "resources",
	"smartShutdownTimeout", "bootstrap", "replica", "externalClusters", "postgresql",
}

// AI가 정할 수 있는 PostgreSQL 설정. 성능과 WAL 보존에 관한 것만 받는다.
// log_*처럼 데이터를 로그로 내보내거나, CNPG가 관리하는 복제·보안 설정은 뺐다.
var pgParameterKeys = []string{
	"max_connections", "shared_buffers", "work_mem", "maintenance_work_mem", "effective_cache_size",
	"max_wal_size", "min_wal_size", "wal_keep_size", "max_slot_wal_keep_size", "checkpoint_timeout",
	"checkpoint_completion_target", "random_page_cost", "effective_io_concurrency",
	"max_worker_processes", "max_parallel_workers", "max_parallel_workers_per_gather",
}

// 매니페스트 크기와 자원 상한 (제안값, 11/6에 확정한다).
const (
	maxManifestBytes = 64 << 10
	maxStorageSize   = "2Ti"
	maxCPU           = "16"
	maxMemory        = "64Gi"
)

// CheckManifest는 대상 매니페스트가 이 Plan의 대상 클러스터를 원본의 복제본으로 만드는지 본다.
// 엔진은 prepare 단계에서 대상 클러스터가 없을 때만 이 매니페스트를 적용한다.
func CheckManifest(manifest string, p *eplan.Plan) Issues {
	var is Issues
	if len(manifest) > maxManifestBytes {
		is.add("", "%d KiB 이하여야 함 (받은 크기 %d바이트)", maxManifestBytes>>10, len(manifest))
		return is
	}
	doc, err := parseYAMLObject(manifest)
	if err != nil {
		is.add("", "YAML 객체 하나여야 함: %v", err)
		return is
	}
	for _, k := range sortedKeys(doc) {
		if !slices.Contains(manifestTopKeys, k) {
			is.add(k, "쓸 수 없는 키 (허용: %s)", strings.Join(manifestTopKeys, ", "))
		}
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
	for _, k := range sortedKeys(meta) {
		if !slices.Contains(manifestMetaKeys, k) {
			is.add("metadata."+k, "쓸 수 없는 키 (허용: name, namespace). annotations, finalizers, ownerReferences로 대상 DB가 멈추거나 지워질 수 있음")
		}
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
	if v, ok := spec["instances"]; ok && !intIn(v, 1, 5) {
		is.add("spec.instances", "1~5인 정수여야 함 (받은 값 %v)", v)
	}
	if v, ok := spec["smartShutdownTimeout"]; ok && !intIn(v, 0, 600) {
		is.add("spec.smartShutdownTimeout", "0~600인 정수(초)여야 함 (받은 값 %v)", v)
	}
	is = append(is, checkStorage("spec.storage", spec["storage"], true)...)
	if v, ok := spec["walStorage"]; ok {
		is = append(is, checkStorage("spec.walStorage", v, false)...)
	}
	if v, ok := spec["resources"]; ok {
		is = append(is, checkResources(v)...)
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
		// minApplyDelay, enabled, promotionToken은 복제를 늦추거나 끄거나 승격 시점을 바꾼다. 승격은 엔진이 정한다.
		for _, k := range sortedKeys(rep) {
			if !slices.Contains([]string{"self", "primary", "source"}, k) {
				is.add("spec.replica."+k, "쓸 수 없는 키 (허용: self, primary, source). 복제 지연과 승격은 엔진이 정함")
			}
		}
	}
	if v, ok := spec["postgresql"]; ok {
		is = append(is, checkPostgresql(v)...)
	}
	b, _ := spec["bootstrap"].(map[string]any)
	if b == nil {
		is.add("spec.bootstrap", "pg_basebackup으로 원본 %q에서 시작해야 함 (받은 값 %v)", p.Source.Cluster, spec["bootstrap"])
	} else {
		for _, k := range sortedKeys(b) {
			if k != "pg_basebackup" {
				is.add("spec.bootstrap."+k, "복제본은 pg_basebackup으로만 시작함")
			}
		}
		if bb, ok := b["pg_basebackup"].(map[string]any); ok {
			is = append(is, checkBasebackup(bb, p)...)
		} else {
			is.add("spec.bootstrap.pg_basebackup", "객체여야 함 (source, database, owner, secret)")
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
	// 운영 데이터를 복제하는 연결이므로 암호화를 끄지 못하게 한다.
	if m := strOf(cp, "sslmode"); m != "require" && m != "verify-ca" && m != "verify-full" {
		is.add(path+".sslmode", "require, verify-ca, verify-full 중 하나여야 함 (받은 값 %v)", cp["sslmode"])
	}
	if db, ok := cp["dbname"]; ok && db != "postgres" {
		is.add(path+".dbname", "postgres여야 함 (받은 값 %v)", db)
	}
	return is
}

// intIn은 YAML 숫자(JSON으로 바꾸면 float64)가 lo~hi 사이의 정수인지 본다. 문자열 "3"이나 true는 받지 않는다.
func intIn(v any, lo, hi float64) bool {
	n, ok := v.(float64)
	return ok && n >= lo && n <= hi && n == math.Trunc(n)
}

// k8sName은 StorageClass 같은 리소스 이름(DNS-1123 subdomain)이다.
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// checkStorage는 storage·walStorage에 크기와 StorageClass만 쓰는지 본다.
// pvcTemplate은 dataSource로 다른 볼륨을 복제해 올 수 있어 받지 않는다.
func checkStorage(path string, v any, required bool) Issues {
	var is Issues
	if v == nil {
		if required {
			is.add(path, "있어야 함 (size 필수)")
		}
		return is
	}
	m, _ := v.(map[string]any)
	if m == nil {
		is.add(path, "객체여야 함 (size, storageClass)")
		return is
	}
	for _, k := range sortedKeys(m) {
		if k != "size" && k != "storageClass" {
			is.add(path+"."+k, "쓸 수 없는 키 (허용: size, storageClass)")
		}
	}
	if s, ok := m["size"]; !ok {
		is.add(path+".size", "있어야 함")
	} else {
		is = append(is, checkQuantity(path+".size", s, maxStorageSize)...)
	}
	if sc, ok := m["storageClass"]; ok {
		if s, _ := sc.(string); !k8sName.MatchString(s) {
			is.add(path+".storageClass", "StorageClass 이름이어야 함 (받은 값 %v)", sc)
		}
	}
	return is
}

// checkResources는 requests·limits에 cpu·memory만, 0보다 크고 상한 이하로 쓰는지 본다.
func checkResources(v any) Issues {
	var is Issues
	m, _ := v.(map[string]any)
	if m == nil {
		is.add("spec.resources", "객체여야 함 (requests, limits)")
		return is
	}
	for _, k := range sortedKeys(m) {
		path := "spec.resources." + k
		if k != "requests" && k != "limits" {
			is.add(path, "쓸 수 없는 키 (허용: requests, limits)")
			continue
		}
		r, _ := m[k].(map[string]any)
		if r == nil {
			is.add(path, "객체여야 함 (cpu, memory)")
			continue
		}
		for _, name := range sortedKeys(r) {
			switch name {
			case "cpu":
				is = append(is, checkQuantity(path+".cpu", r[name], maxCPU)...)
			case "memory":
				is = append(is, checkQuantity(path+".memory", r[name], maxMemory)...)
			default:
				is.add(path+"."+name, "쓸 수 없는 키 (허용: cpu, memory)")
			}
		}
	}
	return is
}

// checkQuantity는 쿠버네티스 수량(500m, 1Gi, 2)이 0보다 크고 max 이하인지 본다.
func checkQuantity(path string, v any, max string) Issues {
	var is Issues
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	}
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 || q.Cmp(resource.MustParse(max)) > 0 {
		is.add(path, "0보다 크고 %s 이하인 수량이어야 함 (받은 값 %v)", max, v)
	}
	return is
}

// checkPostgresql은 parameters만, 허용한 설정 이름과 문자열 값으로 쓰는지 본다.
func checkPostgresql(v any) Issues {
	var is Issues
	pg, _ := v.(map[string]any)
	if pg == nil {
		is.add("spec.postgresql", "객체여야 함 (parameters)")
		return is
	}
	for _, k := range sortedKeys(pg) {
		if k != "parameters" {
			is.add("spec.postgresql."+k, "postgresql 아래에는 parameters만 쓸 수 있음")
		}
	}
	raw, ok := pg["parameters"]
	if !ok {
		return is
	}
	params, _ := raw.(map[string]any)
	if params == nil {
		is.add("spec.postgresql.parameters", "객체여야 함")
		return is
	}
	for _, k := range sortedKeys(params) {
		path := "spec.postgresql.parameters." + k
		if !slices.Contains(pgParameterKeys, k) {
			is.add(path, "쓸 수 없는 설정 (허용: %s)", strings.Join(pgParameterKeys, ", "))
			continue
		}
		if s, _ := params[k].(string); s == "" {
			is.add(path, "값은 문자열이어야 함 (예: \"200\") (받은 값 %v)", params[k])
		}
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

// docEnd는 YAML 문서 끝 표시(...)다. 이 뒤에는 --- 없이도 새 문서가 올 수 있다.
var docEnd = regexp.MustCompile(`(?m)^\.\.\.([ \t#\r]|$)`)

func parseYAMLObject(s string) (map[string]any, error) {
	// 이 검사와 엔진은 첫 문서만 읽지만, 첨부로 남은 파일을 다른 도구가 읽으면 뒤 문서도 적용될 수 있다.
	if strings.Contains(s, "\n---") || docEnd.MatchString(s) {
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
