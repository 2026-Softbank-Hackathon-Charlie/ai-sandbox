package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/rehearsal"
)

func TestReportRehearsalSeparatesWarningsFromExecution(t *testing.T) {
	for _, tc := range []struct {
		name, report string
		err          error
		code         int
		warning      bool
	}{
		{"pass", `{"passed":true}`, nil, 0, false},
		{"pass with delivery warning", `{"passed":true,"warnings":["log delivery incomplete password=do-not-print"]}`, nil, 0, true},
		{"execution failed with warning", `{"passed":false,"warnings":["cleanup failed password=do-not-print"]}`, nil, 1, true},
		{"fatal", `{"passed":true}`, errors.New("execution interrupted"), 1, false},
		{"no report", "null", nil, 1, false},
		{"candidate mismatch", `{"passed":true}`, matchCandidate(plan.Digest{PlanSHA256: "ppp"}, "other", &apiv1.PlanRecord{Ref: "pln_test"}), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rep *rehearsal.Report
			if err := json.Unmarshal([]byte(tc.report), &rep); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			printRehearsalWarnings(&out, rep)
			if code := reportRehearsal(&out, rep, tc.err, "pln_test", 4); code != tc.code {
				t.Fatalf("exit=%d, want %d", code, tc.code)
			}
			text := out.String()
			wantWarnings := 0
			if tc.warning {
				wantWarnings = 1
			}
			if strings.Count(text, "경고:") != wantWarnings || strings.Contains(text, "do-not-print") {
				t.Fatalf("warning missing or secret leaked: %s", text)
			}
			if strings.Contains(text, "승인하지 마세요") != (tc.code != 0) {
				t.Fatalf("approval advice contradicts exit status: %s", text)
			}
			if !strings.Contains(text, "pln_test") || tc.code == 0 && !strings.Contains(text, "revision 4") {
				t.Fatalf("review candidate missing: %s", text)
			}
			// 리허설은 통과했는데 후보만 다르면 리허설 실패로 안내하지 않는다.
			failed := tc.code != 0 && !errors.Is(tc.err, errCandidate)
			if strings.Contains(text, "통과하지 못했습니다") != failed {
				t.Fatalf("candidate mismatch must not read as a failed rehearsal: %s", text)
			}
		})
	}
}

func TestMatchCandidate(t *testing.T) {
	att := func(sum string) *apiv1.PlanRecord {
		list := []apiv1.PlanAttachment{{Name: "rehearsal-report.json", Sha256: "rrr"}, {Name: plan.ManifestName, Sha256: sum}}
		return &apiv1.PlanRecord{Ref: "pln_test", Attachments: &list}
	}
	c := plan.Digest{PlanSHA256: "ppp", ManifestSHA256: "mmm"}
	if err := matchCandidate(c, "ppp", att("mmm")); err != nil {
		t.Fatalf("같은 후보인데 걸림: %v", err)
	}
	if err := matchCandidate(c, "other", att("mmm")); err == nil || !strings.Contains(err.Error(), "제출한 Plan") {
		t.Fatalf("다른 Plan 본문을 놓침: %v", err)
	}
	if err := matchCandidate(c, "ppp", att("old")); err == nil || !strings.Contains(err.Error(), plan.ManifestName) {
		t.Fatalf("리허설하지 않은 매니페스트 첨부를 놓침: %v", err)
	}
	if err := matchCandidate(c, "ppp", &apiv1.PlanRecord{Ref: "pln_test"}); err == nil {
		t.Fatal("매니페스트 첨부가 없는데 통과함")
	}
	if err := matchCandidate(plan.Digest{PlanSHA256: "ppp"}, "ppp", &apiv1.PlanRecord{Ref: "pln_test"}); err != nil {
		t.Fatalf("매니페스트 없는 Plan인데 걸림: %v", err)
	}
}

func TestSourceStatsFromPlatform(t *testing.T) {
	notMeasured := apiv1.PlanDraftResultSourceStatsReasonNotMeasured
	if s := sourceStats(&apiv1.PlanDraftResult{SourceStatsReason: &notMeasured}); s != nil {
		t.Fatalf("재지 못했으면 nil이어야 함: %+v", s)
	}
	if got := reasonOf(&apiv1.PlanDraftResult{SourceStatsReason: &notMeasured}); got != "not_measured" {
		t.Fatalf("이유: %s", got)
	}
	size, wal := int64(2<<30), 1048576.0
	s := sourceStats(&apiv1.PlanDraftResult{SourceStats: &apiv1.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8, Bytes: &size, WalBytesPerSecond: &wal, WindowSeconds: 5}})
	if s == nil || s.Sessions != 80 || s.MaxConnections != 200 || s.MaxWorkerProcesses != 8 || s.Bytes != size || s.WALBytesPerSecond != wal || s.WindowSeconds != 5 {
		t.Fatalf("옮긴 값: %+v", s)
	}
	// 크기와 WAL 속도를 못 쟀으면 0으로 두어 초기 복사 검사가 막지 않는다. 접속 수 검사는 그대로 한다.
	s = sourceStats(&apiv1.PlanDraftResult{SourceStats: &apiv1.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8}})
	if s == nil || s.Bytes != 0 || s.WALBytesPerSecond != 0 || s.RequiredConnections() < 200 {
		t.Fatalf("일부만 잰 값: %+v", s)
	}
}

// fakePlatform은 초안 API만 흉내 낸다. supportsStats가 false면 #47 이전 플랫폼처럼 sourceStats를 무시하고 Plan 자체를 돌려준다.
func fakePlatform(t *testing.T, supportsStats bool, reason string) (*client.Client, *[]bool) {
	var asked []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plans/draft" {
			http.NotFound(w, r)
			return
		}
		var req apiv1.PlanDraftRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		withStats := req.SourceStats != nil && *req.SourceStats
		asked = append(asked, withStats)
		p := apiv1.EnginePlan{Id: "moimroom-gke-to-eks"}
		w.Header().Set("Content-Type", "application/json")
		if !withStats || !supportsStats {
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		res := apiv1.PlanDraftResult{Plan: p}
		if reason != "" {
			r := apiv1.PlanDraftResultSourceStatsReason(reason)
			res.SourceStatsReason = &r
		} else {
			res.SourceStats = &apiv1.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8}
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "agent-token"), &asked
}

func TestDraftWithStats(t *testing.T) {
	req := apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "eks"}
	for _, c := range []struct {
		name            string
		measure, stats  bool
		reason          string
		wantAsked       []bool
		wantStats, warn bool
	}{
		{"실측 받음", true, true, "", []bool{true}, true, false},
		{"플랫폼이 재지 못함", true, true, "not_measured", []bool{true}, false, true},
		{"실측을 모르는 플랫폼", true, false, "", []bool{true, false}, false, true},
		{"-no-measure", false, true, "", []bool{false}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pc, asked := fakePlatform(t, c.stats, c.reason)
			var w bytes.Buffer
			d, s, err := draftWithStats(context.Background(), pc, req, c.measure, &w)
			if err != nil || d == nil || d.Id != "moimroom-gke-to-eks" {
				t.Fatalf("초안: %+v %v", d, err)
			}
			if !slices.Equal(*asked, c.wantAsked) {
				t.Fatalf("sourceStats 요청: %v, want %v", *asked, c.wantAsked)
			}
			if (s != nil) != c.wantStats || strings.Contains(w.String(), "경고") != c.warn {
				t.Fatalf("실측값 %+v, 출력 %q", s, w.String())
			}
		})
	}
}

func TestSourceCluster(t *testing.T) {
	cluster := func(spec, status map[string]any) *unstructured.Unstructured {
		o := map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "moimroom-db-gke", "namespace": "moimroom"}}
		if spec != nil {
			o["spec"] = spec
		}
		if status != nil {
			o["status"] = status
		}
		return &unstructured.Unstructured{Object: o}
	}
	gvr := map[schema.GroupVersionResource]string{llm.CNPGClusterGVR: "ClusterList"}
	params := map[string]any{"parameters": map[string]any{"max_wal_senders": "20", "shared_buffers": "1GB"}}
	for _, c := range []struct {
		name       string
		obj        *unstructured.Unstructured
		wantImage  string
		wantParams map[string]string
	}{
		{"실제로 도는 이미지와 hot standby 설정만", cluster(map[string]any{"imageName": "ghcr.io/cloudnative-pg/postgresql:17.6", "postgresql": params}, map[string]any{"image": "ghcr.io/cloudnative-pg/postgresql:17.11"}), "ghcr.io/cloudnative-pg/postgresql:17.11", map[string]string{"max_wal_senders": "20"}},
		{"status가 없으면 spec", cluster(map[string]any{"imageName": "ghcr.io/cloudnative-pg/postgresql:17.11"}, nil), "ghcr.io/cloudnative-pg/postgresql:17.11", nil},
		{"이미지가 없음", cluster(map[string]any{}, nil), "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr, c.obj)
			sc, err := sourceCluster(context.Background(), dyn, "moimroom", "moimroom-db-gke")
			if c.wantImage == "" {
				if err == nil {
					t.Fatalf("이미지가 없으면 오류여야 함: %+v", sc)
				}
				return
			}
			if err != nil || sc.Image != c.wantImage || !reflect.DeepEqual(sc.Parameters, c.wantParams) {
				t.Fatalf("got %+v, err %v", sc, err)
			}
		})
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr)
	if _, err := sourceCluster(context.Background(), dyn, "moimroom", "moimroom-db-gke"); err == nil {
		t.Fatal("원본 클러스터가 없으면 오류여야 함")
	}
}

// 2026-11-05: 카탈로그가 낡은 독립 DB(moimroom-gcp-db)를 원본으로 가리켰고 같은 네임스페이스에 실제 primary
// (moimroom-db-gke)가 따로 있었다. 원본을 읽을 때 이 상황이 드러나야 한다.
func TestSourceClusterRole(t *testing.T) {
	cluster := func(name string, replica bool) *unstructured.Unstructured {
		spec := map[string]any{"imageName": "ghcr.io/cloudnative-pg/postgresql:17.11"}
		if replica {
			spec["replica"] = map[string]any{"enabled": true}
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": name, "namespace": "charlie-apps"}, "spec": spec,
		}}
	}
	gvr := map[schema.GroupVersionResource]string{llm.CNPGClusterGVR: "ClusterList"}
	read := func(name string, objs ...runtime.Object) *plan.SourceCluster {
		t.Helper()
		sc, err := sourceCluster(context.Background(), dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr, objs...), "charlie-apps", name)
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}

	const dsnGKE = "postgres://app:${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app?sslmode=require"

	// 카탈로그의 원본은 낡은 moimroom-gcp-db인데 Plan의 DSN은 실제 primary를 가리킨다. 승인 전에 막아야 한다.
	sc := read("moimroom-gcp-db", cluster("moimroom-gcp-db", false), cluster("moimroom-db-gke", false))
	if sc.Name != "moimroom-gcp-db" || sc.Namespace != "charlie-apps" || sc.Replica || !slices.Equal(sc.OtherPrimaries, []string{"moimroom-db-gke"}) {
		t.Fatalf("got %+v", sc)
	}
	if err := vetSource(sc, nil, dsnGKE, false, io.Discard); err == nil {
		t.Fatal("DSN이 다른 primary를 가리키면 막아야 함")
	}

	// 카탈로그를 고친 뒤: 독립 DB가 남아 있어도 올바른 원본은 통과한다.
	sc = read("moimroom-db-gke", cluster("moimroom-gcp-db", false), cluster("moimroom-db-gke", false))
	if err := vetSource(sc, nil, dsnGKE, false, io.Discard); err != nil {
		t.Fatalf("올바른 원본은 통과해야 함: %v", err)
	}

	// 혼자 있는 primary는 통과, replica인 이웃은 primary로 세지 않는다.
	sc = read("moimroom-db-gke", cluster("moimroom-db-gke", false), cluster("moimroom-db-follower", true))
	if sc.Replica || len(sc.OtherPrimaries) != 0 || sc.OthersUnknown {
		t.Fatalf("got %+v", sc)
	}

	// 원본 자체가 replica면 허용 플래그로도 막는다.
	if err := vetSource(read("moimroom-db-follower", cluster("moimroom-db-follower", true)), nil, dsnGKE, true, io.Discard); err == nil {
		t.Fatal("원본이 replica면 막아야 함")
	}
}

func TestIsReplica(t *testing.T) {
	with := func(name string, replica map[string]any) *unstructured.Unstructured {
		spec := map[string]any{}
		if replica != nil {
			spec["replica"] = replica
		}
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}, "spec": spec}}
	}
	for _, c := range []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{"replica 설정 없음", with("a", nil), false},
		{"enabled", with("a", map[string]any{"enabled": true}), true},
		{"enabled false", with("a", map[string]any{"enabled": false}), false},
		{"분산 토폴로지에서 primary가 다른 클러스터", with("a", map[string]any{"primary": "b", "source": "b"}), true},
		{"분산 토폴로지에서 primary가 자기 자신", with("a", map[string]any{"primary": "a", "source": "b"}), false},
	} {
		if got := isReplica(c.obj); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// 원본을 못 읽었을 때: 없으면(카탈로그가 틀림) 막고, 권한 문제 등은 경고만 하고 넘어간다.
func TestVetSourceReadFailures(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, "moimroom-gcp-db")
	var w strings.Builder
	if err := vetSource(nil, notFound, "", false, &w); err == nil || !strings.Contains(err.Error(), "찾을 수 없음") {
		t.Fatalf("원본이 없으면 막아야 함: %v", err)
	}
	if err := vetSource(nil, apierrors.NewForbidden(schema.GroupResource{Resource: "clusters"}, "x", errors.New("no")), "", false, &w); err != nil {
		t.Fatalf("권한 오류는 호출한 쪽이 경고하고 넘어간다: %v", err)
	}
	// 이미지가 비어 읽기 오류가 나도 이미 읽은 역할로 검사한다.
	if err := vetSource(&plan.SourceCluster{Name: "a", Namespace: "ns", Replica: true}, errors.New("image 없음"), "", false, &w); err == nil {
		t.Fatal("읽기 오류가 있어도 replica면 막아야 함")
	}
}

// 목록 조회 권한이 없으면(읽기 전용 kubeconfig) 다른 primary 검사만 건너뛰고 경고한다.
func TestSourceClusterListForbidden(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "moimroom-db-gke", "namespace": "charlie-apps"},
		"spec":     map[string]any{"imageName": "ghcr.io/cloudnative-pg/postgresql:17.11"},
	}}
	gvr := map[schema.GroupVersionResource]string{llm.CNPGClusterGVR: "ClusterList"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr, obj)
	dyn.PrependReactor("list", "clusters", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, "", errors.New("list 권한 없음"))
	})
	sc, err := sourceCluster(context.Background(), dyn, "charlie-apps", "moimroom-db-gke")
	if err != nil || !sc.OthersUnknown || len(sc.OtherPrimaries) != 0 {
		t.Fatalf("got %+v, err %v", sc, err)
	}
	var w strings.Builder
	if err := vetSource(sc, nil, "", false, &w); err != nil {
		t.Fatalf("목록을 못 읽은 것만으로 막으면 안 됨: %v", err)
	}
	if !strings.Contains(w.String(), "경고") {
		t.Fatalf("경고가 나와야 함: %q", w.String())
	}
}

func TestSaveProposal(t *testing.T) {
	raw, err := os.ReadFile("../../plan/testdata/proposal.json")
	if err != nil {
		t.Fatal(err)
	}
	var prop plan.Proposal
	if err := json.Unmarshal(raw, &prop); err != nil {
		t.Fatal(err)
	}
	prop.Plan.Source.DSN = "postgres://app:s3cr3t-plain@db:5432/app"
	out := filepath.Join(t.TempDir(), "report-gke-to-eks.json")
	path, err := saveProposal(out, &prop)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "report-gke-to-eks-proposal.json" {
		t.Fatalf("리포트 옆에 써야 함: %s", path)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "s3cr3t-plain") || !strings.Contains(string(b), "manifest") {
		t.Fatalf("비밀값은 가리고 매니페스트는 남겨야 함:\n%s", b)
	}
}
