package rehearsal

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

// DBInfo는 원본 DSN에서 읽은 DB 이름과 사용자다. 비밀번호는 읽지 않는다.
type DBInfo struct {
	Name string
	User string
}

// ParseDB는 DSN(postgres://user:${VAR}@host:port/db?...)에서 사용자와 DB 이름을 읽는다.
func ParseDB(dsn string) (DBInfo, error) {
	// 플랫폼 Plan은 비밀번호 자리에 ${VAR}를 쓴다. URL 파서가 {}를 받지 않으므로 자리표시로 바꾼 뒤 읽는다.
	u, err := url.Parse(varRef.ReplaceAllString(dsn, "x"))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return DBInfo{}, fmt.Errorf("DSN 형식이 아님")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" || u.User == nil || u.User.Username() == "" {
		return DBInfo{}, fmt.Errorf("DSN에 사용자나 DB 이름이 없음")
	}
	return DBInfo{Name: db, User: u.User.Username()}, nil
}

var varRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

func svcHost(cluster, ns string) string { return cluster + "-rw." + ns + ".svc" }

func externalCluster(name, ns string) map[string]any {
	return map[string]any{
		"name":                 name,
		"connectionParameters": map[string]any{"host": svcHost(name, ns), "user": "streaming_replica", "dbname": "postgres", "sslmode": "verify-full"},
		"sslKey":               map[string]any{"name": name + "-replication", "key": "tls.key"},
		"sslCert":              map[string]any{"name": name + "-replication", "key": "tls.crt"},
		"sslRootCert":          map[string]any{"name": name + "-ca", "key": "ca.crt"},
	}
}

// SourceManifest는 샌드박스에 만들 임시 원본이다. 이름은 Plan의 원본 클러스터와 같게 해서
// 대상 매니페스트가 가리키는 시크릿(<원본>-app, -replication, -ca)이 그대로 맞게 한다.
// 엔진의 분산 토폴로지대로 replica.source는 대상을 가리킨다 (migration-engine examples/cnpg와 같은 모양).
func SourceManifest(p *eplan.Plan, ns string, db DBInfo) *unstructured.Unstructured {
	src, dst := p.Source.Cluster, p.Target.Cluster
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": src, "namespace": ns, "labels": map[string]any{"hopcharlie/role": "rehearsal-source"}},
		"spec": map[string]any{
			"instances":            int64(1),
			"smartShutdownTimeout": int64(5),
			"storage":              map[string]any{"size": "1Gi"},
			"bootstrap":            map[string]any{"initdb": map[string]any{"database": db.Name, "owner": db.User}},
			"replica":              map[string]any{"self": src, "primary": src, "source": dst},
			"externalClusters":     []any{externalCluster(src, ns), externalCluster(dst, ns)},
		},
	}}
}

// TargetManifest는 AI가 쓴 대상 매니페스트를 샌드박스로 옮긴다.
// 네임스페이스와 externalClusters의 접속 주소만 샌드박스 것으로 바꾸고, 나머지(StorageClass, 크기, 자원,
// 인스턴스 수, 이미지)는 그대로 둔다. 그 값이 대상 클러스터에서 실제로 되는지가 리허설이 보려는 것이다.
func TargetManifest(manifest string, p *eplan.Plan, ns string) (string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return "", fmt.Errorf("대상 매니페스트를 읽을 수 없음: %w", err)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	meta["namespace"] = ns
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		return "", fmt.Errorf("대상 매니페스트에 spec이 없음")
	}
	ext, _ := spec["externalClusters"].([]any)
	seen := map[string]bool{}
	for _, e := range ext {
		m, _ := e.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["name"].(string)
		if name != p.Source.Cluster && name != p.Target.Cluster {
			continue
		}
		seen[name] = true
		cp, _ := m["connectionParameters"].(map[string]any)
		if cp == nil {
			cp = map[string]any{}
			m["connectionParameters"] = cp
		}
		// 샌드박스 안의 접속 정보로 바꾼다: 주소와, CNPG가 샌드박스에 만든 복제용 인증서 시크릿 이름
		def := externalCluster(name, ns)
		cp["host"] = svcHost(name, ns)
		for _, k := range []string{"sslKey", "sslCert", "sslRootCert"} {
			m[k] = def[k]
		}
	}
	// 엔진은 전환 뒤 옛 원본이 새 primary를 따라가게 하므로 양쪽 항목이 모두 있어야 한다.
	for _, name := range []string{p.Source.Cluster, p.Target.Cluster} {
		if !seen[name] {
			ext = append(ext, externalCluster(name, ns))
		}
	}
	spec["externalClusters"] = ext
	// requests만 적은 자원은 limits도 같게 둔다. 샌드박스 LimitRange의 기본 limits(500m, 512Mi)가 requests보다 작으면
	// Pod가 거절되기 때문이다. 운영 매니페스트는 바꾸지 않는다.
	if res, _ := spec["resources"].(map[string]any); res != nil {
		req, _ := res["requests"].(map[string]any)
		lim, _ := res["limits"].(map[string]any)
		for k, v := range req {
			if _, ok := lim[k]; !ok {
				if lim == nil {
					lim = map[string]any{}
					res["limits"] = lim
				}
				lim[k] = v
			}
		}
	}
	out, err := yaml.Marshal(doc)
	return string(out), err
}

// SandboxConfig는 대상 매니페스트를 리허설할 수 있게 샌드박스 Quota를 잡는다. 기본값(임시 원본, 검사 Pod, 작업 Pod 몫)에
// 대상 클러스터 몫을 더한다. 대상 몫은 인스턴스마다 limits(없으면 requests, 그것도 없으면 LimitRange 기본값)이고,
// CNPG가 인스턴스를 붙일 때 띄우는 작업 Pod 하나를 더 센다. 운영에 맞게 정한 자원을 샌드박스 때문에 줄이지 않게 하려는 것이다.
func SandboxConfig(app, manifest string) (sandbox.Config, error) {
	c := sandbox.Config{App: app}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return c, fmt.Errorf("대상 매니페스트를 읽을 수 없음: %w", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	pods := int64(1)
	if n, ok := spec["instances"].(float64); ok && n > 0 {
		pods = int64(n)
	}
	pods++ // 붙일 때 뜨는 작업 Pod
	res, _ := spec["resources"].(map[string]any)
	need := func(name, def string) (resource.Quantity, error) {
		q := resource.MustParse(def)
		for _, k := range []string{"requests", "limits"} { // limits가 있으면 limits를 쓴다
			m, _ := res[k].(map[string]any)
			if v, ok := m[name]; ok {
				p, err := resource.ParseQuantity(fmt.Sprint(v))
				if err != nil {
					return q, fmt.Errorf("resources.%s.%s %v를 읽을 수 없음: %w", k, name, v, err)
				}
				q = p
			}
		}
		q.Mul(pods)
		return q, nil
	}
	cpu, err := need("cpu", sandbox.DefaultContainerCPU)
	if err != nil {
		return c, err
	}
	mem, err := need("memory", sandbox.DefaultContainerMemory)
	if err != nil {
		return c, err
	}
	cpu.Add(resource.MustParse(sandbox.DefaultCPU))
	mem.Add(resource.MustParse(sandbox.DefaultMemory))
	c.CPU, c.Memory = cpu.String(), mem.String()
	return c, nil
}

// TokenKubeconfig는 관리자 설정의 주소와 CA만 가져오고 인증은 샌드박스 토큰 하나로 하는 kubeconfig 파일을 쓴다.
// 엔진은 이 파일로 대상 네임스페이스의 CNPG Cluster만 다룬다 (최소 권한).
func TokenKubeconfig(dir string, admin *rest.Config, token string) (string, error) {
	ca := admin.CAData
	if len(ca) == 0 && admin.CAFile != "" {
		b, err := os.ReadFile(admin.CAFile)
		if err != nil {
			return "", err
		}
		ca = b
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["sandbox"] = &clientcmdapi.Cluster{Server: admin.Host, CertificateAuthorityData: ca, InsecureSkipTLSVerify: admin.Insecure}
	cfg.AuthInfos["ai-sandbox"] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts["sandbox"] = &clientcmdapi.Context{Cluster: "sandbox", AuthInfo: "ai-sandbox"}
	cfg.CurrentContext = "sandbox"
	path := filepath.Join(dir, "sandbox.kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	return path, nil
}

// SandboxPlan points the engine at sandbox resources and synthetic fixture tables.
// Gateway, files and production write records do not exist in the sandbox.
func SandboxPlan(p *eplan.Plan, ns, kubeconfig, manifestPath, srcDSN, dstDSN string) *eplan.Plan {
	sp := *p
	sp.ID = p.ID + "-rehearsal"
	sp.Source = eplan.Endpoint{Env: p.Source.Env, Kubeconfig: kubeconfig, Context: "sandbox", Namespace: ns, Cluster: p.Source.Cluster, DSN: srcDSN}
	sp.Target = eplan.Endpoint{Env: p.Target.Env, Kubeconfig: kubeconfig, Context: "sandbox", Namespace: ns, Cluster: p.Target.Cluster, DSN: dstDSN, Manifest: manifestPath}
	sp.Files = nil
	sp.Gateway = eplan.GatewaySpec{Mode: "none", SourceBackend: p.Source.Env, TargetBackend: p.Target.Env}
	sp.Verify = p.Verify
	sp.Verify.Schemas = fixtureSchemas(p.Verify.Schemas)
	sp.Verify.Tables = fixtureTables(sp.Verify.Schemas)
	sp.Verify.WrittenRecords = ""
	if sp.Verify.FenceGate == nil {
		gate := true
		sp.Verify.FenceGate = &gate
	}
	return &sp
}

func fixtureSchemas(schemas []string) []string {
	if len(schemas) == 0 {
		return []string{"public"}
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range schemas {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}

func fixtureTables(schemas []string) []string {
	var tables []string
	for _, s := range fixtureSchemas(schemas) {
		tables = append(tables, s+".rehearsal_orders", s+".rehearsal_items")
	}
	return tables
}

// DSN은 로컬 프록시로 가는 DSN이다. 비밀번호는 URL 인코딩한다.
func DSN(db DBInfo, password string, port int) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(db.User, password), Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/" + db.Name, RawQuery: "sslmode=require&connect_timeout=10"}
	return u.String()
}

// SeedConnectWait는 임시 원본이 healthy가 된 뒤 첫 접속을 다시 시도하는 시간이다.
// 2026-11-05 실제 EKS(CNPG 1.30.1)에서 healthy 직후의 첫 접속이 app 계정 비밀번호 인증에 실패했고(SQLSTATE 28P01),
// 몇 초 뒤 같은 비밀번호·같은 프록시로는 바로 접속됐다. kind에서는 드러나지 않았다.
var SeedConnectWait = 60 * time.Second

// connectRetry는 wait 동안 3초마다 다시 접속해 본다. 마지막 오류를 돌려준다.
func connectRetry(ctx context.Context, dsn string, wait time.Duration) (*pgx.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(3 * time.Second):
		}
	}
}

// Seed는 임시 원본에 테스트 데이터를 넣는다. 검증할 스키마마다 표 두 개를 만든다.
// 운영 데이터는 넣지 않는다 (설계 문서 6장).
func Seed(ctx context.Context, dsn string, schemas []string, rows int) error {
	if rows < 4 {
		return fmt.Errorf("fixture rows must be at least 4")
	}
	conn, err := connectRetry(ctx, dsn, SeedConnectWait)
	if err != nil {
		return fmt.Errorf("임시 원본 접속 실패: %w", err)
	}
	defer conn.Close(ctx)
	for _, s := range fixtureSchemas(schemas) {
		id := pgx.Identifier{s}.Sanitize()
		stmts := []string{
			"CREATE SCHEMA IF NOT EXISTS " + id,
			"CREATE TABLE IF NOT EXISTS " + id + ".rehearsal_orders (id bigserial PRIMARY KEY, total numeric(10,2), note text, placed_at timestamptz DEFAULT now())",
			"CREATE TABLE IF NOT EXISTS " + id + ".rehearsal_items (order_id bigint, line int, sku text, PRIMARY KEY (order_id, line))",
			fmt.Sprintf("INSERT INTO %s.rehearsal_orders (total, note) SELECT g * 1.5, 'n' || g FROM generate_series(1, %d) g", id, rows),
			fmt.Sprintf("INSERT INTO %s.rehearsal_items SELECT g, l, 'sku-' || l FROM generate_series(1, %d) g, generate_series(1, 3) l", id, rows/4),
		}
		for _, q := range stmts {
			if _, err := conn.Exec(ctx, q); err != nil {
				return fmt.Errorf("테스트 데이터 넣기 실패 (%s): %w", s, err)
			}
		}
	}
	return nil
}

// TempStorageClass는 임시 원본 디스크의 StorageClass를 고른다. 비어 있으면 클러스터 기본값을 쓴다.
// 임시 원본은 리허설 대상이 아니라 실제 원본을 대신하는 것이므로, 샌드박스 클러스터에서 반드시 뜨는 곳을 골라야 한다.
// 기본 StorageClass가 없는 클러스터(예: 워크로드 EKS)에서 비워 두면 PVC가 영원히 Pending이 되고
// 경고 이벤트도 되풀이되지 않아 멈춤 감지에 걸리지 않는다 (2026-11-05 실제 EKS에서 확인).
//   - 기본 StorageClass가 있으면 비운다.
//   - 대상 매니페스트의 StorageClass가 이 클러스터에 있고 지우면 디스크도 지우는 것(Delete)이면 그것.
//   - 아니면 Delete인 것 중 CSI 드라이버를 먼저, 이름순으로 첫 번째. Retain은 샌드박스를 지워도 디스크가 남으므로 고르지 않는다.
func TempStorageClass(scs []storagev1.StorageClass, targetManifest string) (string, error) {
	deletes := func(s storagev1.StorageClass) bool {
		return s.ReclaimPolicy == nil || *s.ReclaimPolicy == corev1.PersistentVolumeReclaimDelete
	}
	for _, s := range scs {
		if s.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return "", nil
		}
	}
	var doc map[string]any
	_ = yaml.Unmarshal([]byte(targetManifest), &doc)
	spec, _ := doc["spec"].(map[string]any)
	storage, _ := spec["storage"].(map[string]any)
	want, _ := storage["storageClass"].(string)
	var cands []storagev1.StorageClass
	for _, s := range scs {
		if !deletes(s) {
			continue
		}
		if s.Name == want {
			return s.Name, nil
		}
		cands = append(cands, s)
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("샌드박스 클러스터에 기본 StorageClass도, 지우면 디스크도 지우는(Delete) StorageClass도 없음")
	}
	csi := func(s storagev1.StorageClass) bool { return strings.Contains(s.Provisioner, ".csi.") }
	sort.SliceStable(cands, func(i, j int) bool {
		if csi(cands[i]) != csi(cands[j]) {
			return csi(cands[i])
		}
		return cands[i].Name < cands[j].Name
	})
	return cands[0].Name, nil
}

// PrepareTempSource는 임시 원본을 실제 원본처럼 띄운다. 리허설이 보려는 것은 대상이 "실제 원본"의 복제본으로 뜨는지이므로,
// 임시 원본이 대상 클러스터의 기본값으로 뜨면 리허설 결과가 운영과 달라진다(CNPG 기본 설정 100·32, 운영자 기본 이미지 18.6 등).
//   - 원본을 읽었으면(sc): 이미지와 hot standby 설정(plan.StandbyParams)을 원본 값으로 둔다. 원본에 없는 설정은 원본도 기본값이므로
//     임시 원본도 기본값이다. 원본 이미지가 CNPG 공식 이미지가 아니면(사설 레지스트리·미러) 샌드박스가 받지 못할 수 있으므로
//     대상 이미지를 쓴다. 대상 이미지는 계획 검사가 원본과 같은 메이저 버전임을 확인한 것이다.
//   - 원본을 모르면: 대상 매니페스트의 이미지와 hot standby 설정을 쓴다. 대상이 자기 설정 이상인 원본의 복제본으로 뜨는지만 본다.
func PrepareTempSource(src *unstructured.Unstructured, sc *plan.SourceCluster, targetManifest string) error {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(targetManifest), &doc); err != nil {
		return fmt.Errorf("대상 매니페스트를 읽을 수 없음: %w", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	targetImage, _ := spec["imageName"].(string)
	pg, _ := spec["postgresql"].(map[string]any)
	targetParams, _ := pg["parameters"].(map[string]any)

	img := targetImage
	set := map[string]any{}
	if sc != nil {
		if plan.IsCNPGImage(sc.Image) {
			img = sc.Image
		}
		for _, k := range plan.StandbyParams {
			if v, ok := sc.Parameters[k]; ok {
				set[k] = v
			}
		}
	} else {
		for _, k := range plan.StandbyParams {
			if v, ok := targetParams[k].(string); ok {
				set[k] = v
			}
		}
	}
	if len(set) > 0 {
		if err := unstructured.SetNestedMap(src.Object, set, "spec", "postgresql", "parameters"); err != nil {
			return err
		}
	}
	if img == "" {
		return nil // 둘 다 없으면 대상 클러스터의 기본 이미지
	}
	return unstructured.SetNestedField(src.Object, img, "spec", "imageName")
}
