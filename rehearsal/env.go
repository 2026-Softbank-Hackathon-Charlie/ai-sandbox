package rehearsal

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
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
	out, err := yaml.Marshal(doc)
	return string(out), err
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

// SandboxPlan은 엔진이 샌드박스에서 돌릴 Plan이다. 원본·대상을 샌드박스 클러스터로 바꾸고,
// 게이트웨이, 파일, k6 쓰기 기록은 뺀다 (샌드박스에는 없다). 상한값과 검증 범위는 AI의 Plan 그대로다.
func SandboxPlan(p *eplan.Plan, ns, kubeconfig, manifestPath, srcDSN, dstDSN string) *eplan.Plan {
	sp := *p
	sp.ID = p.ID + "-rehearsal"
	sp.Source = eplan.Endpoint{Env: p.Source.Env, Kubeconfig: kubeconfig, Context: "sandbox", Namespace: ns, Cluster: p.Source.Cluster, DSN: srcDSN}
	sp.Target = eplan.Endpoint{Env: p.Target.Env, Kubeconfig: kubeconfig, Context: "sandbox", Namespace: ns, Cluster: p.Target.Cluster, DSN: dstDSN, Manifest: manifestPath}
	sp.Files = nil
	sp.Gateway = eplan.GatewaySpec{Mode: "none", SourceBackend: p.Source.Env, TargetBackend: p.Target.Env}
	sp.Verify = p.Verify
	sp.Verify.WrittenRecords = ""
	return &sp
}

// DSN은 로컬 프록시로 가는 DSN이다. 비밀번호는 URL 인코딩한다.
func DSN(db DBInfo, password string, port int) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(db.User, password), Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/" + db.Name, RawQuery: "sslmode=require&connect_timeout=10"}
	return u.String()
}

// Seed는 임시 원본에 테스트 데이터를 넣는다. 검증할 스키마마다 표 두 개를 만든다.
// 운영 데이터는 넣지 않는다 (설계 문서 6장).
func Seed(ctx context.Context, dsn string, schemas []string, rows int) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("임시 원본 접속 실패: %w", err)
	}
	defer conn.Close(ctx)
	if len(schemas) == 0 {
		schemas = []string{"public"}
	}
	for _, s := range schemas {
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
