package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// 검사 결과의 기대값
const (
	ExpectBlocked = "blocked"
	ExpectOpen    = "open"
)

// CheckResult는 차단 테스트 한 줄이다. 콘솔의 '막힌 명령' 표에 그대로 나온다.
type CheckResult struct {
	Name    string `json:"name"`
	Command string `json:"command"` // 사람이 읽는 같은 뜻의 명령
	Expect  string `json:"expect"`
	Got     string `json:"got"`
	Pass    bool   `json:"pass"`
	Detail  string `json:"detail,omitempty"`
}

// Target은 샌드박스 안에서 TCP 연결을 시도할 곳이다.
type Target struct {
	Name   string
	Host   string
	Port   int
	Expect string
	// MustResolve면 이름을 찾지 못한 것을 '막힘'으로 치지 않는다. 정책 동작의 증거로 쓰는 대상은
	// 정책이 없으면 열리는 곳이어야 하므로, 주소가 없으면 아무것도 증명하지 못한다.
	MustResolve bool
}

// IsolationConfig는 격리 검증 설정이다.
type IsolationConfig struct {
	// ProdNamespace는 운영 앱 네임스페이스다. 이 안의 secret을 AI 토큰으로 읽을 수 없어야 한다.
	ProdNamespace string
	// Targets는 기본 대상(메타데이터 주소, API 서버) 뒤에 더할 대상이다. 운영 DB 주소를 넣는다.
	Targets []Target
	// ProbeImage는 검사 Pod 이미지다. 기본 busybox:1.37.
	ProbeImage string
	// Timeout은 검사 Pod가 끝날 때까지 기다리는 시간이다. 기본 90초.
	Timeout time.Duration
	// APIServerAllowed면 샌드박스가 CNPG 때문에 API 서버로 나갈 수 있다(ForCNPG).
	// 그때는 API 서버 대신 다른 네임스페이스의 서비스(CNPG 웹훅)를 정책 동작의 증거로 쓴다.
	APIServerAllowed bool
}

// CNPGWitness는 정책이 없으면 열리고, 샌드박스 정책으로는 막혀야 하는 다른 네임스페이스의 서비스다.
var CNPGWitness = Target{Name: "other-namespace", Host: "cnpg-webhook-service.cnpg-system.svc.cluster.local", Port: 443, Expect: ExpectBlocked, MustResolve: true}

// targetsFor는 설정에 맞는 기본 검사 대상을 고른다.
func targetsFor(c IsolationConfig) []Target {
	var t []Target
	for _, d := range DefaultTargets {
		if d.Name == "apiserver" && c.APIServerAllowed {
			continue
		}
		t = append(t, d)
	}
	if c.APIServerAllowed {
		t = append(t, CNPGWitness)
	}
	return append(t, c.Targets...)
}

// DefaultTargets는 모든 샌드박스에서 막혀야 하는 곳이다.
// API 서버는 NetworkPolicy가 없으면 어느 Pod에서나 열리므로, 정책이 실제로 동작하는지 보여주는 증거가 된다.
// (ForCNPG로 API 서버를 열었으면 APIServerAllowed를 켠다. 그러면 이 항목 대신 CNPGWitness를 쓴다.)
var DefaultTargets = []Target{
	{Name: "metadata", Host: "169.254.169.254", Port: 80, Expect: ExpectBlocked},
	{Name: "apiserver", Host: "kubernetes.default.svc.cluster.local", Port: 443, Expect: ExpectBlocked, MustResolve: true},
}

var hostRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?$`)
var nameRe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// CheckIsolation은 AI 토큰(aiCS)과 관리자 권한(adminCS)으로 차단 테스트를 돌린다.
// 관리자 권한은 검사 Pod를 띄우고 로그를 읽는 데만 쓴다.
func CheckIsolation(ctx context.Context, adminCS, aiCS kubernetes.Interface, sb *Sandbox, c IsolationConfig) ([]CheckResult, error) {
	if c.ProdNamespace == "" {
		return nil, fmt.Errorf("운영 네임스페이스를 정해야 함")
	}
	results := checkRBAC(ctx, aiCS, sb.Namespace, c.ProdNamespace)
	net, err := checkNetwork(ctx, adminCS, sb.Namespace, c)
	if err != nil {
		return results, err
	}
	return append(results, net...), nil
}

// AllPass는 모든 항목이 기대대로인지 알려준다.
func AllPass(rs []CheckResult) bool {
	for _, r := range rs {
		if !r.Pass {
			return false
		}
	}
	return len(rs) > 0
}

func checkRBAC(ctx context.Context, ai kubernetes.Interface, sbNS, prodNS string) []CheckResult {
	var out []CheckResult

	// 실제로 시도한다. 읽기 요청이라 성공해도 바뀌는 것은 없다.
	r := CheckResult{Name: "prod-secrets", Command: "kubectl get secrets -n " + prodNS, Expect: ExpectBlocked}
	_, err := ai.CoreV1().Secrets(prodNS).List(ctx, metav1.ListOptions{Limit: 1})
	r.Got, r.Detail = rbacOutcome(err)
	r.Pass = r.Got == ExpectBlocked
	out = append(out, r)

	// 쓰기 권한은 실제로 시도하지 않고 API 서버에 물어본다. 혹시 열려 있으면 정책이 지워지기 때문이다.
	for _, q := range []struct{ name, cmd, ns, group, res, verb string }{
		{"own-netpol-delete", "kubectl delete networkpolicy -n " + sbNS + " sandbox-isolation", sbNS, "networking.k8s.io", "networkpolicies", "delete"},
		{"own-role-escalate", "kubectl edit role -n " + sbNS + " " + AIServiceAccount, sbNS, "rbac.authorization.k8s.io", "roles", "update"},
		{"own-token-create", "kubectl create token -n " + sbNS + " " + AIServiceAccount, sbNS, "", "serviceaccounts/token", "create"},
		{"prod-deploy-delete", "kubectl delete deployment -n " + prodNS + " --all", prodNS, "apps", "deployments", "delete"},
	} {
		res, sub, _ := strings.Cut(q.res, "/")
		ssar, err := ai.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{
			Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: q.ns, Group: q.group, Resource: res, Subresource: sub, Verb: q.verb,
			}},
		}, metav1.CreateOptions{})
		r := CheckResult{Name: q.name, Command: q.cmd, Expect: ExpectBlocked}
		switch {
		case err != nil:
			r.Got, r.Detail = "error", err.Error()
		case ssar.Status.Allowed:
			r.Got, r.Detail = ExpectOpen, ssar.Status.Reason
		default:
			r.Got = ExpectBlocked
		}
		r.Pass = r.Got == ExpectBlocked
		out = append(out, r)
	}

	// 노드 네트워크·파일시스템을 쓰는 Pod는 RBAC(Pod 생성 권한 없음)나 Pod Security가 막아야 한다.
	// dry-run으로 보내 실제로는 아무것도 만들지 않는다.
	out = append(out, checkHostPod(ctx, ai, sbNS))

	// 반대로, 자기 네임스페이스에서는 일할 수 있어야 한다. 아니면 리허설 자체가 안 된다.
	r = CheckResult{Name: "own-pods-list", Command: "kubectl get pods -n " + sbNS, Expect: ExpectOpen}
	_, err = ai.CoreV1().Pods(sbNS).List(ctx, metav1.ListOptions{Limit: 1})
	r.Got, r.Detail = rbacOutcome(err)
	r.Pass = r.Got == ExpectOpen
	return append(out, r)
}

// HostPod는 격리를 벗어나는 Pod다. hostNetwork면 NetworkPolicy를 받지 않고, hostPath면 노드 파일을 읽는다.
func HostPod(namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "host-escape-", Namespace: namespace},
		Spec: corev1.PodSpec{
			HostNetwork: true,
			Volumes:     []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}},
			Containers: []corev1.Container{{
				Name: "x", Image: "busybox:1.37",
				VolumeMounts: []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
		},
	}
}

// host-pod-create 검사에서 무엇이 막았는지 (CheckResult.Detail)
const (
	HostPodByRBAC        = "RBAC: Pod 생성 권한 없음"
	HostPodByPodSecurity = "Pod Security: 노드 네트워크·파일시스템을 쓰는 Pod 금지"
)

func checkHostPod(ctx context.Context, ai kubernetes.Interface, sbNS string) CheckResult {
	r := CheckResult{Name: "host-pod-create", Command: "kubectl run -n " + sbNS + " --dry-run=server (hostNetwork, hostPath /)", Expect: ExpectBlocked}
	_, err := ai.CoreV1().Pods(sbNS).Create(ctx, HostPod(sbNS), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	switch {
	case err == nil:
		r.Got = ExpectOpen
	// Quota 초과도 Forbidden이므로, Pod Security나 RBAC가 막은 것만 막힘으로 친다.
	// RBAC는 admission보다 먼저 보므로, Pod 생성 권한이 없으면 RBAC 거절이 온다.
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "PodSecurity"):
		r.Got, r.Detail = ExpectBlocked, HostPodByPodSecurity
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), `cannot create resource "pods"`):
		r.Got, r.Detail = ExpectBlocked, HostPodByRBAC
	default:
		r.Got, r.Detail = "error", err.Error()
	}
	r.Pass = r.Got == ExpectBlocked
	return r
}

func rbacOutcome(err error) (string, string) {
	switch {
	case err == nil:
		return ExpectOpen, ""
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return ExpectBlocked, ""
	default:
		// 네트워크 오류 같은 것은 막힌 것으로 치지 않는다. 검사가 안 된 것이다.
		return "error", err.Error()
	}
}

// probeScript는 인자로 받은 "이름 호스트 포트 이름확인" 묶음마다 TCP 연결을 시도하고 PROBE 줄을 찍는다.
// 이름확인이 1이면 먼저 이름을 찾고, 못 찾으면 unresolved를 찍는다 (연결 실패와 구분).
// 대상 값은 셸 문자열에 끼워 넣지 않고 인자로만 넘긴다.
const probeScript = `
if nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then echo "PROBE dns open"; else echo "PROBE dns blocked"; fi
while [ $# -ge 4 ]; do
  if [ "$4" = 1 ] && ! nslookup "$2" >/dev/null 2>&1; then echo "PROBE $1 unresolved"
  elif nc -w 3 "$2" "$3" </dev/null >/dev/null 2>&1; then echo "PROBE $1 open"; else echo "PROBE $1 blocked"; fi
  shift 4
done
echo "PROBE-END"
`

// ProbePod는 검사 Pod다. 같은 네임스페이스에 뜨므로 샌드박스 NetworkPolicy를 그대로 받는다.
func ProbePod(namespace, image string, targets []Target) (*corev1.Pod, error) {
	args := []string{"-c", probeScript, "probe"}
	for _, t := range targets {
		if !nameRe.MatchString(t.Name) || !hostRe.MatchString(t.Host) || t.Port < 1 || t.Port > 65535 {
			return nil, fmt.Errorf("검사 대상 형식이 잘못됨: %+v", t)
		}
		if t.Expect != ExpectBlocked && t.Expect != ExpectOpen {
			return nil, fmt.Errorf("%s: 기대값은 blocked 또는 open이어야 함", t.Name)
		}
		resolve := "0"
		if t.MustResolve {
			resolve = "1"
		}
		args = append(args, t.Name, t.Host, strconv.Itoa(t.Port), resolve)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "isolation-probe-",
			Namespace:    namespace,
			Labels:       map[string]string{LabelSandbox: "true", "hopcharlie/role": "isolation-probe"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				RunAsUser:      ptr(int64(65534)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   image,
				Command: []string{"sh"},
				Args:    args,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					ReadOnlyRootFilesystem:   ptr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}, nil
}

func checkNetwork(ctx context.Context, admin kubernetes.Interface, ns string, c IsolationConfig) ([]CheckResult, error) {
	image := c.ProbeImage
	if image == "" {
		image = "busybox:1.37"
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	targets := targetsFor(c)
	pod, err := ProbePod(ns, image, targets)
	if err != nil {
		return nil, err
	}
	created, err := admin.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("검사 Pod 생성 실패: %w", err)
	}
	defer func() {
		_ = admin.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), created.Name, metav1.DeleteOptions{})
	}()

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		p, err := admin.CoreV1().Pods(ns).Get(wctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("검사 Pod 상태 조회 실패: %w", err)
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			break
		}
		select {
		case <-wctx.Done():
			return nil, fmt.Errorf("검사 Pod가 %s 안에 끝나지 않음 (상태 %s)", timeout, p.Status.Phase)
		case <-time.After(time.Second):
		}
	}

	logs, err := admin.CoreV1().Pods(ns).GetLogs(created.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("검사 Pod 로그 읽기 실패: %w", err)
	}
	return ParseProbe(string(logs), targets)
}

// ParseProbe는 검사 Pod 로그를 결과로 바꾼다. 로그가 끝까지 찍히지 않았으면 오류다.
func ParseProbe(logs string, targets []Target) ([]CheckResult, error) {
	got := map[string]string{}
	ended := false
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) == 1 && f[0] == "PROBE-END":
			ended = true
		case len(f) == 3 && f[0] == "PROBE":
			got[f[1]] = f[2]
		}
	}
	if !ended {
		return nil, fmt.Errorf("검사 Pod 로그가 끝까지 찍히지 않음:\n%s", logs)
	}

	// DNS는 열려 있어야 한다. 막혀 있으면 정책이 아니라 검사 Pod 네트워크가 고장 난 것일 수 있다.
	out := []CheckResult{result("dns", "nslookup kubernetes.default", ExpectOpen, got["dns"])}
	for _, t := range targets {
		out = append(out, result(t.Name, commandFor(t), t.Expect, got[t.Name]))
	}
	return out, nil
}

func result(name, cmd, expect, got string) CheckResult {
	r := CheckResult{Name: name, Command: cmd, Expect: expect, Got: got}
	switch got {
	case "":
		r.Got, r.Detail = "missing", "로그에 결과가 없음"
	case "unresolved":
		r.Detail = "주소를 찾을 수 없어 정책 동작을 증명하지 못함"
	}
	r.Pass = r.Got == expect
	return r
}

func commandFor(t Target) string {
	if t.Port == 80 {
		return "curl http://" + t.Host + "/"
	}
	return fmt.Sprintf("nc %s %d", t.Host, t.Port)
}
