package sandbox

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var now = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

func TestBuildRejectsBadValues(t *testing.T) {
	for _, c := range []Config{
		{App: "shop", ID: "ab12", CPU: "-1"},
		{App: "shop", ID: "ab12", Memory: "0"},
		{App: "shop", ID: "ab12", Pods: -5},
		{App: "shop", ID: "ab12", TTL: -time.Hour},
	} {
		if _, err := Build(c, now); err == nil {
			t.Errorf("통과하면 안 되는 설정: %+v", c)
		}
	}
}

func TestBuild(t *testing.T) {
	o, err := Build(Config{App: "shop", ID: "ab12"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if o.Namespace.Name != "sandbox-shop-ab12" {
		t.Fatalf("네임스페이스 이름: %s", o.Namespace.Name)
	}
	if o.Namespace.Labels[PodSecurityEnforce] != "baseline" || o.Role.Labels[PodSecurityEnforce] != "" {
		t.Fatalf("Pod Security 라벨은 네임스페이스에만: %v", o.Namespace.Labels)
	}
	if o.Namespace.Labels[LabelSandbox] != "true" || o.Namespace.Annotations[AnnoExpires] != "2026-11-04T11:00:00Z" {
		t.Fatalf("라벨·만료 시각: %v %v", o.Namespace.Labels, o.Namespace.Annotations)
	}

	// RoleBinding은 이 네임스페이스의 Role과 AI 계정만 잇는다
	rb := o.RoleBinding
	if rb.RoleRef.Kind != "Role" || rb.Subjects[0].Namespace != o.Namespace.Name || len(rb.Subjects) != 1 {
		t.Fatalf("RoleBinding: %+v", rb)
	}

	// AI 계정이 격리를 스스로 풀 수 있는 권한이 없어야 한다
	for _, r := range o.Role.Rules {
		for _, res := range r.Resources {
			for _, banned := range []string{"roles", "rolebindings", "networkpolicies", "resourcequotas", "limitranges", "serviceaccounts/token", "pods/exec", "namespaces", "*"} {
				if res == banned {
					t.Errorf("Role에 %s 권한이 있음", res)
				}
			}
		}
		if slices.Contains(r.Verbs, "*") || slices.Contains(r.Verbs, "escalate") || slices.Contains(r.Verbs, "bind") {
			t.Errorf("Role에 넓은 동사가 있음: %v", r.Verbs)
		}
		// 만들고 고칠 수 있는 것은 CNPG Cluster뿐이다. Pod·Service를 만들 수 있으면 다른 계정으로 Pod를 띄우거나
		// Service externalIPs로 클러스터 트래픽을 가로챌 수 있다.
		writes := slices.ContainsFunc(r.Verbs, func(v string) bool { return v != "get" && v != "list" && v != "watch" })
		if writes && (!slices.Equal(r.APIGroups, []string{"postgresql.cnpg.io"}) || !slices.Equal(r.Resources, []string{"clusters"})) {
			t.Errorf("CNPG Cluster 말고 쓰기 권한이 있음: %+v", r)
		}
		if slices.Contains(r.Resources, "secrets") && !slices.Equal(r.Verbs, []string{"get"}) {
			t.Errorf("secret은 이름으로 읽기만: %v", r.Verbs)
		}
	}

	// 기본 차단: 들어오고 나가는 쪽 모두 정책 대상이고, 밖으로는 DNS만 열린다
	np := o.NetworkPolicy.Spec
	if len(np.PolicyTypes) != 2 || len(np.PodSelector.MatchLabels) != 0 {
		t.Fatalf("NetworkPolicy가 모든 Pod의 들어오고 나감을 막아야 함: %+v", np)
	}
	if len(np.Egress) != 3 {
		t.Fatalf("나가는 규칙은 같은 네임스페이스, kube-dns, NodeLocal DNS 세 개여야 함: %d개", len(np.Egress))
	}
	for _, e := range np.Egress {
		for _, p := range e.To {
			// 기본 설정에서 IP 대역은 NodeLocal DNSCache 주소의 DNS 포트뿐이다
			if p.IPBlock != nil && (p.IPBlock.CIDR != NodeLocalDNSCIDR || len(e.Ports) != 2) {
				t.Fatalf("기본 설정에 허용된 IP 대역은 %s의 DNS 포트뿐이어야 함: %+v %+v", NodeLocalDNSCIDR, p.IPBlock, e.Ports)
			}
		}
	}

	q := o.ResourceQuota.Spec.Hard
	if q.Cpu().String() == "" || q[corev1.ResourceLimitsMemory] != q[corev1.ResourceRequestsMemory] {
		t.Fatalf("Quota: %v", q)
	}
	if lb := q[corev1.ResourceServicesLoadBalancers]; lb.Value() != 0 {
		t.Fatal("LoadBalancer는 0개여야 함")
	}
	if p := q[corev1.ResourcePods]; p.Value() != 20 {
		t.Fatalf("Pod 상한: %v", p.Value())
	}
	if *o.ServiceAccount.AutomountServiceAccountToken {
		t.Fatal("AI 계정 토큰이 Pod에 자동으로 붙으면 안 됨")
	}
}

func TestBuildAllowsConfiguredExits(t *testing.T) {
	o, err := Build(Config{App: "shop", ID: "ab12", AllowIngressNamespaces: []string{"cnpg-system"}, AllowEgressCIDRs: []string{"10.96.0.1/32"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.NetworkPolicy.Spec.Egress) != 4 || o.NetworkPolicy.Spec.Egress[3].To[0].IPBlock.CIDR != "10.96.0.1/32" {
		t.Fatalf("나가는 허용 대역이 안 들어감: %+v", o.NetworkPolicy.Spec.Egress)
	}
	if len(o.NetworkPolicy.Spec.Ingress[0].From) != 2 {
		t.Fatalf("들어오는 허용 네임스페이스가 안 들어감: %+v", o.NetworkPolicy.Spec.Ingress)
	}
}

func TestBuildRejects(t *testing.T) {
	for _, c := range []Config{
		{App: ""}, {App: "Shop"}, {App: "shop_app"}, {App: strings.Repeat("a", 33)},
		{App: "shop", ID: "AB12"}, {App: "shop", ID: "abc"},
		{App: "shop", CPU: "four"},
	} {
		if _, err := Build(c, now); err == nil {
			t.Errorf("거절해야 함: %+v", c)
		}
	}
}

func TestBuildRandomID(t *testing.T) {
	a, _ := Build(Config{App: "shop"}, now)
	b, _ := Build(Config{App: "shop"}, now)
	if !strings.HasPrefix(a.Namespace.Name, "sandbox-shop-") || len(a.Namespace.Name) != len("sandbox-shop-")+4 {
		t.Fatalf("이름: %s", a.Namespace.Name)
	}
	if a.Namespace.Name == b.Namespace.Name {
		t.Log("무작위 ID가 겹침 (드묾)")
	}
}

func TestCreateAndDelete(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	sb, err := Create(ctx, cs, Config{App: "shop", ID: "ab12"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Namespace != "sandbox-shop-ab12" || !sb.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("%+v", sb)
	}

	// 만드는 순서: 네임스페이스 다음 바로 NetworkPolicy
	var created []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			created = append(created, a.GetResource().Resource)
		}
	}
	want := []string{"namespaces", "networkpolicies", "resourcequotas", "limitranges", "serviceaccounts", "roles", "rolebindings"}
	if !slices.Equal(created, want) {
		t.Fatalf("생성 순서: %v", created)
	}

	if err := Delete(ctx, cs, sb.Namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, sb.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("지워지지 않음")
	}
	if err := Delete(ctx, cs, sb.Namespace); err != nil {
		t.Fatalf("이미 없으면 성공이어야 함: %v", err)
	}
}

func TestCreateCleansUpOnFailure(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "roles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("권한 없음")
	})
	_, err := Create(context.Background(), cs, Config{App: "shop", ID: "ab12"}, now)
	if err == nil || !strings.Contains(err.Error(), "Role 생성 실패") {
		t.Fatalf("실패해야 함: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "sandbox-shop-ab12", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("실패하면 네임스페이스를 지워야 함")
	}
}

func TestDeleteRefusesNonSandbox(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-fake-0000"}}, // 이름만 비슷하고 라벨 없음
	)
	for _, ns := range []string{"shop", "kube-system", "sandbox-fake-0000"} {
		if err := Delete(context.Background(), cs, ns); !errors.Is(err, ErrNotSandbox) {
			t.Errorf("%s: 거절해야 함, 받은 오류 %v", ns, err)
		}
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "sandbox-fake-0000", metav1.GetOptions{}); err != nil {
		t.Fatal("라벨 없는 네임스페이스가 지워짐")
	}
}

func TestToken(t *testing.T) {
	cs := fake.NewClientset()
	var gotSec int64
	cs.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenRequest)
		gotSec = *tr.Spec.ExpirationSeconds
		tr.Status = authnv1.TokenRequestStatus{Token: "t", ExpirationTimestamp: metav1.NewTime(now.Add(time.Duration(gotSec) * time.Second))}
		return true, tr, nil
	})
	sb := &Sandbox{Namespace: "sandbox-shop-ab12", ExpiresAt: now.Add(time.Hour)}

	if _, _, err := Token(context.Background(), cs, sb, 0, now); err != nil || gotSec != 3600 {
		t.Fatalf("기본 1시간이어야 함: %d초, %v", gotSec, err)
	}
	// 샌드박스 만료 시각을 넘지 않는다
	if _, _, err := Token(context.Background(), cs, sb, 0, now.Add(40*time.Minute)); err != nil || gotSec != 1200 {
		t.Fatalf("남은 20분으로 줄여야 함: %d초, %v", gotSec, err)
	}
	// 너무 조금 남았으면 발급하지 않는다
	if _, _, err := Token(context.Background(), cs, sb, 0, now.Add(55*time.Minute)); err == nil {
		t.Fatal("5분 남았으면 거절해야 함")
	}
}

func TestSweep(t *testing.T) {
	mk := func(name, exp string, sandbox bool) *corev1.Namespace {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AnnoExpires: exp}}}
		if sandbox {
			n.Labels = map[string]string{LabelSandbox: "true"}
		}
		return n
	}
	cs := fake.NewClientset(
		mk("sandbox-shop-old1", "2026-11-04T09:00:00Z", true),
		mk("sandbox-shop-new1", "2026-11-04T10:30:00Z", true),
		mk("sandbox-shop-bad1", "내일", true),
		mk("shop", "2026-01-01T00:00:00Z", false), // 운영: 라벨이 없으니 손대지 않는다
	)
	res, err := Sweep(context.Background(), cs, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Deleted, []string{"sandbox-shop-old1"}) || !slices.Equal(res.Skipped, []string{"sandbox-shop-bad1"}) {
		t.Fatalf("%+v", res)
	}
	for _, n := range []string{"sandbox-shop-new1", "sandbox-shop-bad1", "shop"} {
		if _, err := cs.CoreV1().Namespaces().Get(context.Background(), n, metav1.GetOptions{}); err != nil {
			t.Errorf("%s가 지워짐", n)
		}
	}
}

// aiFake는 권한 확인 요청에 '거절'로 답하는 가짜 AI 클라이언트다. fake clientset은 이 요청을 처리하지 못한다.
// Pod 생성은 Pod Security(baseline)가 hostNetwork Pod를 거절하는 것처럼 답한다.
func aiFake() *fake.Clientset {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, a.(k8stesting.CreateAction).GetObject(), nil
	})
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Spec.HostNetwork {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "host-escape-x",
				errors.New(`violates PodSecurity "baseline:latest": host namespaces (hostNetwork=true)`))
		}
		return false, nil, nil
	})
	return cs
}

// allowPodCreate는 권한 확인 요청 중 Pod 생성만 허용으로 답하게 한다.
func allowPodCreate(cs *fake.Clientset) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview)
		ra := r.Spec.ResourceAttributes
		r.Status.Allowed = ra.Resource == "pods" && ra.Subresource == "" && ra.Verb == "create"
		return true, r, nil
	})
}

func TestCheckPodSecurity(t *testing.T) {
	t.Run("관리자도 호스트 Pod를 만들 수 없으면 통과", func(t *testing.T) {
		r := checkPodSecurity(context.Background(), aiFake(), "sandbox-shop-ab12")
		if !r.Pass || r.Name != "host-pod-admission" || r.Detail != HostPodByPodSecurity {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("Pod Security가 꺼져 있으면 실패", func(t *testing.T) {
		r := checkPodSecurity(context.Background(), fake.NewClientset(), "sandbox-shop-ab12")
		if r.Pass || r.Got != ExpectOpen {
			t.Fatalf("%+v", r)
		}
	})
}

func TestCheckRBAC(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no"))
	t.Run("막혀 있으면 통과", func(t *testing.T) {
		ai := aiFake()
		ai.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden })
		rs := checkRBAC(context.Background(), ai, "sandbox-shop-ab12", "shop")
		if !AllPass(rs) {
			t.Fatalf("%+v", rs)
		}
		if rs[0].Command != "kubectl get secrets -n shop" {
			t.Fatalf("명령 표시: %s", rs[0].Command)
		}
	})
	t.Run("운영 secret이 읽히면 실패", func(t *testing.T) {
		rs := checkRBAC(context.Background(), aiFake(), "sandbox-shop-ab12", "shop")
		if AllPass(rs) || rs[0].Got != ExpectOpen {
			t.Fatalf("%+v", rs[0])
		}
	})
	t.Run("자기 정책을 지울 수 있으면 실패", func(t *testing.T) {
		ai := aiFake()
		ai.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden })
		ai.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
			r := a.(k8stesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview)
			r.Status.Allowed = r.Spec.ResourceAttributes.Resource == "networkpolicies"
			return true, r, nil
		})
		rs := checkRBAC(context.Background(), ai, "sandbox-shop-ab12", "shop")
		var failed []string
		for _, r := range rs {
			if !r.Pass {
				failed = append(failed, r.Name)
			}
		}
		if !slices.Equal(failed, []string{"own-netpol-delete"}) {
			t.Fatalf("실패 항목: %v", failed)
		}
	})
	t.Run("자기 네임스페이스에서 일을 못 하면 실패", func(t *testing.T) {
		ai := aiFake()
		ai.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden })
		rs := checkRBAC(context.Background(), ai, "sandbox-shop-ab12", "shop")
		if last := rs[len(rs)-1]; last.Name != "own-pods-list" || last.Pass {
			t.Fatalf("%+v", last)
		}
	})
	t.Run("호스트 Pod를 만들 수 있으면 실패", func(t *testing.T) {
		ai := aiFake()
		allowPodCreate(ai)
		ai.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden })
		ai.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, &corev1.Pod{}, nil })
		rs := checkRBAC(context.Background(), ai, "sandbox-shop-ab12", "shop")
		r := rs[len(rs)-2]
		if r.Name != "host-pod-create" || r.Pass || r.Got != ExpectOpen {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("Pod 생성 권한이 없으면 RBAC가 막은 것", func(t *testing.T) {
		// 권한 확인 요청으로 판정하므로, 실제 생성 요청은 보내지 않는다
		ai := aiFake()
		ai.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			t.Error("권한이 없으면 Pod 생성을 시도하지 않아야 함")
			return true, nil, nil
		})
		r := checkHostPod(context.Background(), ai, "sandbox-shop-ab12")
		if !r.Pass || r.Got != ExpectBlocked || r.Detail != HostPodByRBAC {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("Pod 생성 권한이 있으면 Pod Security가 막아야 함", func(t *testing.T) {
		ai := aiFake()
		allowPodCreate(ai)
		r := checkHostPod(context.Background(), ai, "sandbox-shop-ab12")
		if !r.Pass || r.Got != ExpectBlocked || r.Detail != HostPodByPodSecurity {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("Quota 거절은 막힘으로 치지 않음", func(t *testing.T) {
		ai := aiFake()
		allowPodCreate(ai)
		ai.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("exceeded quota: sandbox-quota"))
		})
		r := checkHostPod(context.Background(), ai, "sandbox-shop-ab12")
		if r.Pass || r.Got != "error" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("네트워크 오류는 막힘으로 치지 않음", func(t *testing.T) {
		ai := aiFake()
		ai.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})
		rs := checkRBAC(context.Background(), ai, "sandbox-shop-ab12", "shop")
		if rs[0].Pass || rs[0].Got != "error" {
			t.Fatalf("%+v", rs[0])
		}
	})
}

func TestProbePod(t *testing.T) {
	p, err := ProbePod("sandbox-shop-ab12", "busybox:1.37", DefaultSettle, append(DefaultTargets, Target{Name: "db", Host: "10.0.1.5", Port: 5432, Expect: ExpectBlocked}))
	if err != nil {
		t.Fatal(err)
	}
	args := p.Spec.Containers[0].Args
	if !slices.Equal(args[len(args)-4:], []string{"db", "10.0.1.5", "5432", "0"}) || !slices.Contains(args, "1") {
		t.Fatalf("대상은 인자로만 넘겨야 함: %v", args)
	}
	if args[3] != "15" {
		t.Fatalf("첫 인자는 연결 전 대기 초여야 함: %v", args[:4])
	}
	if strings.Contains(args[1], "10.0.1.5") {
		t.Fatal("대상이 셸 스크립트에 끼어 있음")
	}
	if *p.Spec.AutomountServiceAccountToken || !*p.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("검사 Pod는 토큰 없이 root가 아닌 사용자로 떠야 함")
	}

	for _, bad := range []Target{
		{"db", "10.0.1.5; rm -rf /", 5432, ExpectBlocked, false},
		{"db", "$(id)", 5432, ExpectBlocked, false},
		{"db x", "10.0.1.5", 5432, ExpectBlocked, false},
		{"db", "10.0.1.5", 0, ExpectBlocked, false},
		{"db", "10.0.1.5", 5432, "maybe", false},
	} {
		if _, err := ProbePod("ns", "busybox", 0, []Target{bad}); err == nil {
			t.Errorf("거절해야 함: %+v", bad)
		}
	}
}

func TestParseProbe(t *testing.T) {
	targets := append(append([]Target{}, DefaultTargets...), Target{Name: "db", Host: "10.0.1.5", Port: 5432, Expect: ExpectBlocked})
	t.Run("모두 기대대로", func(t *testing.T) {
		rs, err := ParseProbe("PROBE dns open\nPROBE metadata blocked\nPROBE apiserver blocked\nPROBE db blocked\nPROBE-END\n", targets)
		if err != nil || !AllPass(rs) || len(rs) != 4 {
			t.Fatalf("%v %+v", err, rs)
		}
		if rs[1].Command != "curl http://169.254.169.254/" || rs[3].Command != "nc 10.0.1.5 5432" {
			t.Fatalf("명령 표시: %+v", rs)
		}
	})
	t.Run("정책이 안 먹어서 API 서버가 열림", func(t *testing.T) {
		rs, _ := ParseProbe("PROBE dns open\nPROBE metadata blocked\nPROBE apiserver open\nPROBE db blocked\nPROBE-END\n", targets)
		if AllPass(rs) || rs[2].Pass {
			t.Fatalf("%+v", rs[2])
		}
	})
	t.Run("DNS가 막히면 실패", func(t *testing.T) {
		rs, _ := ParseProbe("PROBE dns blocked\nPROBE metadata blocked\nPROBE apiserver blocked\nPROBE db blocked\nPROBE-END\n", targets)
		if rs[0].Pass {
			t.Fatal("DNS가 막혀 있으면 검사 자체를 믿을 수 없음")
		}
	})
	t.Run("증거 대상을 찾지 못하면 실패", func(t *testing.T) {
		rs, _ := ParseProbe("PROBE dns open\nPROBE metadata blocked\nPROBE apiserver unresolved\nPROBE db blocked\nPROBE-END\n", targets)
		if AllPass(rs) || rs[2].Pass || rs[2].Detail == "" {
			t.Fatalf("%+v", rs[2])
		}
	})
	t.Run("결과 빠짐", func(t *testing.T) {
		rs, _ := ParseProbe("PROBE dns open\nPROBE metadata blocked\nPROBE-END\n", targets)
		if AllPass(rs) || rs[2].Got != "missing" {
			t.Fatalf("%+v", rs)
		}
	})
	t.Run("중간에 끊긴 로그", func(t *testing.T) {
		if _, err := ParseProbe("PROBE dns open\n", targets); err == nil {
			t.Fatal("끝 표시가 없으면 오류여야 함")
		}
	})
	t.Run("빈 결과는 통과가 아님", func(t *testing.T) {
		if AllPass(nil) {
			t.Fatal("검사 항목이 없으면 통과로 치면 안 됨")
		}
	})
}

func TestForCNPG(t *testing.T) {
	cs := fake.NewClientset(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "kubernetes"}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
	}, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.1", ClusterIPs: []string{"10.96.0.1"}},
	})
	c, err := ForCNPG(context.Background(), cs, Config{App: "moimroom", ID: "ab12"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := Build(c, now)
	if err != nil {
		t.Fatal(err)
	}
	np := o.NetworkPolicy.Spec
	var opened []string
	for _, r := range np.Egress {
		for _, to := range r.To {
			if to.IPBlock != nil {
				opened = append(opened, to.IPBlock.CIDR)
			}
		}
	}
	if !slices.Contains(opened, "172.18.0.2/32") {
		t.Fatalf("API 서버 엔드포인트가 열려야 함: %v", opened)
	}
	if !slices.Contains(opened, "10.96.0.1/32") {
		t.Fatalf("EKS처럼 서비스 주소로 판정하는 곳을 위해 kubernetes 서비스 주소도 열어야 함: %v", opened)
	}
	from := np.Ingress[0].From
	if from[len(from)-1].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != CNPGOperatorNamespace {
		t.Fatalf("CNPG 오퍼레이터가 들어올 수 있어야 함: %+v", from)
	}

	if _, err := ForCNPG(context.Background(), fake.NewClientset(), Config{App: "moimroom"}); err == nil {
		t.Fatal("API 서버 엔드포인트가 없으면 오류여야 함")
	}
}

// GKE의 NodeLocal DNSCache는 Pod가 kube-dns 대신 169.254.20.10으로 DNS를 묻게 한다. 클러스터를 읽지 않고
// 그 주소의 53번 포트만 항상 열어야 하고, 메타데이터 주소는 계속 막혀야 한다.
// (2026-11-07 GKE 리허설: 정책이 kube-dns Pod만 허용해 격리 검사의 dns 항목이 막혔다. DaemonSet을 읽어 있을 때만
// 여는 방식은 관리 계정에 읽기 권한이 없으면 조용히 건너뛰어 같은 실패가 났다.)
func TestBuildAlwaysOpensNodeLocalDNSPortOnly(t *testing.T) {
	// DaemonSet이 하나도 없는 클러스터(읽을 수 없는 경우와 같다)에서도 열려야 한다
	cnpg, err := ForCNPG(context.Background(), fake.NewClientset(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "kubernetes"}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
	}), Config{App: "moimroom", ID: "ab12"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Config{"기본": {App: "moimroom", ID: "ab12"}, "CNPG 모드": cnpg} {
		o, err := Build(c, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var rules []netv1.NetworkPolicyEgressRule
		for _, r := range o.NetworkPolicy.Spec.Egress {
			for _, to := range r.To {
				if to.IPBlock == nil {
					continue
				}
				if to.IPBlock.CIDR == NodeLocalDNSCIDR {
					rules = append(rules, r)
				}
				// 어떤 허용 대역도 메타데이터 주소를 덮으면 안 된다
				if coversMetadata(to.IPBlock) {
					t.Fatalf("%s: 메타데이터 주소를 덮는 허용 대역: %+v", name, to.IPBlock)
				}
			}
		}
		if len(rules) != 1 {
			t.Fatalf("%s: NodeLocal DNS 규칙이 하나 있어야 함: %+v", name, rules)
		}
		var ports []string
		for _, p := range rules[0].Ports {
			ports = append(ports, string(*p.Protocol)+"/"+p.Port.String())
		}
		if !slices.Equal(ports, []string{"UDP/53", "TCP/53"}) {
			t.Fatalf("%s: DNS 포트만 열어야 함: %v", name, ports)
		}
	}
}

// coversMetadata는 허용 대역이 except를 빼고도 메타데이터 주소를 덮는지 본다.
func coversMetadata(b *netv1.IPBlock) bool {
	_, n, err := net.ParseCIDR(b.CIDR)
	if err != nil {
		return true
	}
	if !n.Contains(net.ParseIP("169.254.169.254")) {
		return false
	}
	for _, e := range b.Except {
		if _, x, err := net.ParseCIDR(e); err == nil && x.Contains(net.ParseIP("169.254.169.254")) {
			return false
		}
	}
	return true
}

// 이 GKE에서 Pod의 DNS 서버는 kube-dns ClusterIP이고, NodeLocal DNSCache가 그 패킷을 가로채 응답한다(infra ADR 0052).
// kube-dns Service의 주소를 읽으면 그 주소의 DNS 포트만 열고, 읽지 못하면 DNS 포트를 모든 목적지로 연다(메타데이터 제외).
func TestForCNPGOpensKubeDNSClusterIP(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "kubernetes"}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
	}
	dnsPortsOf := func(r netv1.NetworkPolicyEgressRule) []string {
		var ports []string
		for _, p := range r.Ports {
			ports = append(ports, string(*p.Protocol)+"/"+p.Port.String())
		}
		return ports
	}
	// IP 대역 규칙을 CIDR로 모은다. except가 있으면 "CIDR except X"로 적는다.
	opened := func(c Config) map[string][]string {
		o, err := Build(c, now)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		for _, r := range o.NetworkPolicy.Spec.Egress {
			for _, to := range r.To {
				if to.IPBlock != nil {
					key := to.IPBlock.CIDR
					if len(to.IPBlock.Except) > 0 {
						key += " except " + strings.Join(to.IPBlock.Except, ",")
					}
					got[key] = dnsPortsOf(r)
				}
			}
		}
		return got
	}
	dnsOnly := []string{"UDP/53", "TCP/53"}

	// kube-dns Service를 읽은 경우: 그 주소와 NodeLocal 주소만, 모든 목적지 규칙은 없다
	withSvc, err := ForCNPG(context.Background(), fake.NewClientset(slice, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.82.0.10", ClusterIPs: []string{"10.82.0.10"}},
	}), Config{App: "moimroom", ID: "ab12"})
	if err != nil {
		t.Fatal(err)
	}
	if withSvc.AllowDNSAnywhere {
		t.Fatal("kube-dns Service를 읽었으면 모든 목적지로 열면 안 됨")
	}
	got := opened(withSvc)
	if !slices.Equal(got["10.82.0.10/32"], dnsOnly) || !slices.Equal(got[NodeLocalDNSCIDR], dnsOnly) {
		t.Fatalf("kube-dns ClusterIP와 NodeLocal 주소의 DNS 포트가 열려야 함: %v", got)
	}
	for k := range got {
		if strings.HasPrefix(k, "0.0.0.0/0") {
			t.Fatalf("모든 목적지 규칙이 있으면 안 됨: %v", got)
		}
	}

	// kube-dns Service를 읽지 못한 경우: DNS 포트를 모든 목적지로 열되 메타데이터 주소는 뺀다
	without, err := ForCNPG(context.Background(), fake.NewClientset(slice), Config{App: "moimroom", ID: "ab12"})
	if err != nil {
		t.Fatal(err)
	}
	if !without.AllowDNSAnywhere {
		t.Fatal("kube-dns Service를 읽지 못하면 DNS를 모든 목적지로 열어야 함")
	}
	got = opened(without)
	if !slices.Equal(got["0.0.0.0/0 except 169.254.169.254/32"], dnsOnly) {
		t.Fatalf("DNS 포트만, 메타데이터 주소는 제외하고 열려야 함: %v", got)
	}
}

func TestBuildAllowDNSCIDRs(t *testing.T) {
	for _, bad := range []string{"not-a-cidr", "169.254.0.0/16", "169.254.169.254/32", "0.0.0.0/0"} {
		if _, err := Build(Config{App: "shop", ID: "ab12", AllowDNSCIDRs: []string{bad}}, now); err == nil {
			t.Errorf("DNS 허용 대역 %q는 거절해야 함", bad)
		}
	}
	// 추가 대역은 DNS 포트만 열고, NodeLocal DNS 주소를 또 넣어도 규칙은 하나다
	o, err := Build(Config{App: "shop", ID: "ab12", AllowDNSCIDRs: []string{"10.0.0.2/32", NodeLocalDNSCIDR}}, now)
	if err != nil {
		t.Fatal(err)
	}
	eg := o.NetworkPolicy.Spec.Egress
	if len(eg) != 4 || eg[2].To[0].IPBlock.CIDR != NodeLocalDNSCIDR || eg[3].To[0].IPBlock.CIDR != "10.0.0.2/32" || len(eg[3].Ports) != 2 {
		t.Fatalf("DNS 허용 규칙이 안 들어감: %+v", eg)
	}
}

func TestIsolationTargetsForCNPG(t *testing.T) {
	names := func(ts []Target) []string {
		var n []string
		for _, x := range ts {
			n = append(n, x.Name)
		}
		return n
	}
	if got := names(targetsFor(IsolationConfig{})); !slices.Equal(got, []string{"metadata", "apiserver"}) {
		t.Fatalf("기본: %v", got)
	}
	// API 서버를 열었으면 API 서버 대신 다른 네임스페이스 서비스로 정책 동작을 증명한다
	got := names(targetsFor(IsolationConfig{APIServerAllowed: true, Targets: []Target{{Name: "db"}}}))
	if !slices.Equal(got, []string{"metadata", "other-namespace", "db"}) {
		t.Fatalf("CNPG: %v", got)
	}
}

func TestFailed(t *testing.T) {
	rs := []CheckResult{
		{Name: "dns", Expect: ExpectOpen, Got: "open", Pass: true},
		{Name: "db", Expect: ExpectBlocked, Got: "open", Pass: false},
		{Name: "metadata", Expect: ExpectBlocked, Got: "open", Pass: false},
	}
	got := Failed(rs)
	want := []string{"db: blocked 기대, open", "metadata: blocked 기대, open"}
	if !slices.Equal(got, want) {
		t.Fatalf("실패 항목 = %v, want %v", got, want)
	}
	if len(Failed([]CheckResult{{Name: "dns", Expect: ExpectOpen, Got: "open", Pass: true}})) != 0 {
		t.Fatal("모두 통과하면 비어 있어야 함")
	}
	if len(Failed(nil)) != 0 {
		t.Fatal("검사가 없으면 비어 있어야 함")
	}
}

func TestEffectiveSettle(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, DefaultSettle}, // 비우면 기본값
		{-1, 0},            // 음수는 기다리지 않음 (strict 모드 직접 확인)
		{-time.Hour, 0},    //
		{3 * time.Second, 3 * time.Second},
	} {
		if got := effectiveSettle(c.in); got != c.want {
			t.Errorf("effectiveSettle(%s) = %s, want %s", c.in, got, c.want)
		}
	}
	// 대기 0초도 검사 Pod 인자로 만들 수 있어야 한다
	p, err := ProbePod("ns", "busybox", effectiveSettle(-1), []Target{{Name: "x", Host: "10.0.0.1", Port: 80, Expect: ExpectBlocked}})
	if err != nil {
		t.Fatal(err)
	}
	if args := p.Spec.Containers[0].Args; args[3] != "0" {
		t.Fatalf("대기 없음은 인자 0이어야 함: %v", args[:4])
	}
}
