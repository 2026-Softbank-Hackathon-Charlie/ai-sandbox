package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// SandboxSource는 샌드박스에 만들 임시 원본의 이름이다. 보통 Plan의 원본 클러스터 이름과 같다.
// 두 환경에서 같은 이름을 쓰는 DB 짝(infra ADR 0063)은 원본과 대상을 한 샌드박스에 둘 수 없으므로
// 원본 이름에 -source를 붙인다.
func SandboxSource(p *eplan.Plan) string {
	if p.Source.Cluster == p.Target.Cluster {
		return p.Source.Cluster + "-source"
	}
	return p.Source.Cluster
}

// SourceManifest는 샌드박스에 만들 임시 원본이다. 이름은 Plan의 원본 클러스터와 같게 해서
// 대상 매니페스트가 가리키는 시크릿(<원본>-app, -replication, -ca)이 그대로 맞게 한다.
// 엔진의 분산 토폴로지대로 replica.source는 대상을 가리킨다 (migration-engine examples/cnpg와 같은 모양).
func SourceManifest(p *eplan.Plan, ns string, db DBInfo) *unstructured.Unstructured {
	src, dst := SandboxSource(p), p.Target.Cluster
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
	src := SandboxSource(p)
	if src != p.Source.Cluster {
		renameSource(spec, p.Source.Cluster, src)
	}
	ext, _ := spec["externalClusters"].([]any)
	seen := map[string]bool{}
	for _, e := range ext {
		m, _ := e.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["name"].(string)
		if name != src && name != p.Target.Cluster {
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
	for _, name := range []string{src, p.Target.Cluster} {
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

// renameSource는 원본과 대상의 이름이 같은 대상 매니페스트에서 원본을 가리키는 자리를 샌드박스 원본 이름(to)으로 바꾼다.
// 이름만으로는 어느 쪽인지 알 수 없으므로 자리로 정한다: replica.primary와 replica.source,
// pg_basebackup의 source와 원본 앱 계정 시크릿은 원본이고, replica.self는 대상 자신이다.
// externalClusters는 그 이름의 첫 항목을 원본으로 남기고 나머지는 지운다. 대상 항목은 호출한 쪽이 붙인다.
func renameSource(spec map[string]any, from, to string) {
	if r, _ := spec["replica"].(map[string]any); r != nil {
		for _, k := range []string{"primary", "source"} {
			if r[k] == from {
				r[k] = to
			}
		}
	}
	if b, _ := spec["bootstrap"].(map[string]any); b != nil {
		if pb, _ := b["pg_basebackup"].(map[string]any); pb != nil {
			if pb["source"] == from {
				pb["source"] = to
			}
			if s, _ := pb["secret"].(map[string]any); s != nil && s["name"] == from+"-app" {
				s["name"] = to + "-app"
			}
		}
	}
	ext, _ := spec["externalClusters"].([]any)
	var out []any
	renamed := false
	for _, e := range ext {
		if m, _ := e.(map[string]any); m != nil && m["name"] == from {
			if renamed {
				continue
			}
			m["name"], renamed = to, true
		}
		out = append(out, e)
	}
	if ext != nil {
		spec["externalClusters"] = out
	}
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
	// TLSServerName은 터널(127.0.0.1)로 붙을 때 인증서의 원래 이름이다. 빠지면 엔진이 인증서 확인에 실패한다.
	cfg.Clusters["sandbox"] = &clientcmdapi.Cluster{Server: admin.Host, CertificateAuthorityData: ca, InsecureSkipTLSVerify: admin.Insecure, TLSServerName: admin.ServerName}
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
	sp.Source = eplan.Endpoint{Env: p.Source.Env, Kubeconfig: kubeconfig, Context: "sandbox", Namespace: ns, Cluster: SandboxSource(p), DSN: srcDSN}
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

// retryableConnectErr는 임시 원본이 막 healthy가 된 때 잠깐 나타나는 접속 오류만 true다.
// 비밀번호 인증 실패(28P01), 서버가 아직 연결을 받지 못함(57P03), 접속 수 초과(53300),
// 그리고 프록시 쪽 연결 오류(연결 거부·끊김)가 대상이다. 잘못된 DSN이나 DB 없음 같은 영구 오류는
// 60초를 기다리지 않고 바로 돌려준다.
func retryableConnectErr(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "57P03", "53300":
			return true
		}
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// connectRetry는 wait 동안 3초마다 다시 접속해 본다. 일시적인 오류(retryableConnectErr)만 다시 시도하고,
// 마지막 오류를 돌려준다.
func connectRetry(ctx context.Context, dsn string, wait time.Duration) (*pgx.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil || !retryableConnectErr(err) || time.Now().After(deadline) || ctx.Err() != nil {
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
//   - 기본 StorageClass가 있고 지우면 디스크도 지우는 것(Delete)이면 비운다. 기본값이 Retain이면 아래 규칙으로 Delete인 것을 찾고, 없을 때만 기본값을 쓴다.
//   - 대상 매니페스트의 StorageClass가 이 클러스터에 있고 지우면 디스크도 지우는 것(Delete)이면 그것.
//   - 아니면 Delete인 것 중 CSI 드라이버를 먼저, 이름순으로 첫 번째. Retain은 샌드박스를 지워도 디스크가 남으므로 고르지 않는다.
func TempStorageClass(scs []storagev1.StorageClass, targetManifest string) (string, error) {
	deletes := func(s storagev1.StorageClass) bool {
		return s.ReclaimPolicy == nil || *s.ReclaimPolicy == corev1.PersistentVolumeReclaimDelete
	}
	// 기본값이 지우면 디스크도 지우는 것이면 그대로 쓴다. 기본값이 Retain이면 샌드박스를 지워도 디스크가 남으므로
	// 아래에서 Delete인 것을 찾고, 없을 때만 기본값을 쓴다.
	hasDefault := false
	for _, s := range scs {
		if s.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			if deletes(s) {
				return "", nil
			}
			hasDefault = true
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
		if hasDefault {
			return "", nil // Delete인 것이 없으면 Retain 기본값이라도 써서 뜨게 한다
		}
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

// SandboxStorageClasses는 샌드박스에 띄울 대상 매니페스트의 storage·walStorage StorageClass 가운데
// reclaimPolicy가 Retain인 것을 같은 프로비저너의 Delete 클래스로 바꾼다. Retain이면 샌드박스를 지워도
// EBS 같은 디스크가 Released로 남아 리허설마다 쌓인다(2026-11-05 실제 EKS, cnpg-gp3-retain 10Gi).
// 운영 매니페스트는 바꾸지 않는다. 바꾼 것은 notes로 돌려주어 리허설 기록에 남긴다.
// 이 클러스터에 없는 클래스는 그대로 둔다. 그 값이 대상에서 되는지는 리허설이 잡아야 하는 것이다.
func SandboxStorageClasses(manifest string, scs []storagev1.StorageClass) (string, []string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return "", nil, fmt.Errorf("대상 매니페스트를 읽을 수 없음: %w", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		return manifest, nil, nil
	}
	byName := map[string]storagev1.StorageClass{}
	for _, s := range scs {
		byName[s.Name] = s
	}
	deletes := func(s storagev1.StorageClass) bool {
		return s.ReclaimPolicy == nil || *s.ReclaimPolicy == corev1.PersistentVolumeReclaimDelete
	}
	var notes []string
	for _, key := range []string{"storage", "walStorage"} {
		st, _ := spec[key].(map[string]any)
		name, _ := st["storageClass"].(string)
		cur, ok := byName[name]
		if name == "" || !ok || deletes(cur) {
			continue
		}
		// 같은 프로비저너 가운데 운영과 가장 비슷한 것: 같은 volumeBindingMode(다르면 다중 AZ에서 운영에 없는
		// 스케줄 실패가 날 수 있음), 그다음 같은 parameters가 많은 것(type, encrypted, iops 등), 그다음 이름순.
		var cands []storagev1.StorageClass
		for _, s := range scs {
			if deletes(s) && s.Provisioner == cur.Provisioner {
				cands = append(cands, s)
			}
		}
		if len(cands) == 0 {
			return "", nil, fmt.Errorf("%s.storageClass %s는 Retain인데 같은 프로비저너(%s)의 Delete StorageClass가 없어 샌드박스 디스크가 남게 됨", key, name, cur.Provisioner)
		}
		sameParams := func(s storagev1.StorageClass) int {
			n := 0
			for k, v := range cur.Parameters {
				if s.Parameters[k] == v {
					n++
				}
			}
			return n
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if mi, mj := bindingMode(cands[i]) == bindingMode(cur), bindingMode(cands[j]) == bindingMode(cur); mi != mj {
				return mi
			}
			if pi, pj := sameParams(cands[i]), sameParams(cands[j]); pi != pj {
				return pi > pj
			}
			return cands[i].Name < cands[j].Name
		})
		st["storageClass"] = cands[0].Name
		notes = append(notes, fmt.Sprintf("%s.storageClass %s(Retain) → %s(Delete, 같은 프로비저너): 샌드박스를 지울 때 디스크도 지움", key, name, cands[0].Name))
	}
	if len(notes) == 0 {
		return manifest, nil, nil
	}
	out, err := yaml.Marshal(doc)
	return string(out), notes, err
}

// bindingMode는 비어 있으면 쿠버네티스 기본값(Immediate)이다.
func bindingMode(s storagev1.StorageClass) storagev1.VolumeBindingMode {
	if s.VolumeBindingMode == nil {
		return storagev1.VolumeBindingImmediate
	}
	return *s.VolumeBindingMode
}
