// Package sandbox는 리허설용 샌드박스 네임스페이스를 만들고 지우고, 격리가 실제로 되는지 검사한다.
// 샌드박스는 대상 클러스터에 만든다 (설계 문서 6장).
package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	// LabelSandbox가 붙은 네임스페이스만 이 패키지가 지운다.
	LabelSandbox = "hopcharlie/sandbox"
	LabelApp     = "hopcharlie/app"
	// AnnoExpires는 만료 시각(RFC 3339)이다. 남은 샌드박스는 Sweep이 이 값을 보고 지운다.
	AnnoExpires = "hopcharlie/expires-at"

	// AIServiceAccount는 AI 도구(sandbox_apply, sandbox_smoke)가 쓰는 계정이다.
	AIServiceAccount = "ai-sandbox"

	namePrefix = "sandbox-"
)

// Config는 샌드박스 하나의 설정이다. 0값이면 기본값(데모 값)을 쓴다.
type Config struct {
	App string
	// ID는 네임스페이스 이름 끝의 4자리다. 비우면 무작위로 만든다.
	ID string
	// TTL이 지나면 Sweep이 지운다. 기본 1시간.
	TTL time.Duration

	CPU    string // 기본 "4"
	Memory string // 기본 "8Gi"
	Pods   int    // 기본 20

	// AllowIngressNamespaces는 샌드박스 Pod로 들어오는 연결을 허용할 네임스페이스다.
	// CNPG를 쓰면 operator 네임스페이스(예: cnpg-system)를 넣어야 한다.
	AllowIngressNamespaces []string
	// AllowEgressCIDRs는 샌드박스 밖으로 나가는 연결을 허용할 주소 대역이다.
	// CNPG 인스턴스는 API 서버에 붙어야 하므로 API 서버 주소를 넣어야 한다. 그 밖에는 비워 둔다.
	AllowEgressCIDRs []string
}

// CNPGOperatorNamespace는 CloudNativePG 오퍼레이터의 기본 네임스페이스다.
const CNPGOperatorNamespace = "cnpg-system"

// ForCNPG는 샌드박스에서 CNPG 클러스터가 뜰 수 있게 설정을 더한다.
// 오퍼레이터가 인스턴스 관리자(8000번)에 들어오고, 인스턴스는 API 서버로 나간다.
// 다른 곳(운영망, 메타데이터 주소, 다른 네임스페이스)은 그대로 막힌다.
func ForCNPG(ctx context.Context, cs kubernetes.Interface, c Config) (Config, error) {
	ips, err := APIServerIPs(ctx, cs)
	if err != nil {
		return c, err
	}
	c.AllowIngressNamespaces = append(append([]string{}, c.AllowIngressNamespaces...), CNPGOperatorNamespace)
	c.AllowEgressCIDRs = append([]string{}, c.AllowEgressCIDRs...)
	for _, ip := range ips {
		cidr := ip + "/32"
		if strings.Contains(ip, ":") {
			cidr = ip + "/128"
		}
		c.AllowEgressCIDRs = append(c.AllowEgressCIDRs, cidr)
	}
	return c, nil
}

// APIServerIPs는 default 네임스페이스의 kubernetes 서비스 엔드포인트 주소다.
// NetworkPolicy는 서비스 IP가 아니라 변환된 엔드포인트 주소로 판정하므로 이 주소를 열어야 한다.
func APIServerIPs(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	slices, err := cs.DiscoveryV1().EndpointSlices("default").List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=kubernetes"})
	if err != nil {
		return nil, fmt.Errorf("API 서버 주소 조회 실패: %w", err)
	}
	var ips []string
	for _, sl := range slices.Items {
		for _, ep := range sl.Endpoints {
			ips = append(ips, ep.Addresses...)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("API 서버 엔드포인트가 없음")
	}
	return ips, nil
}

var appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
var sandboxID = regexp.MustCompile(`^[a-z0-9]{4}$`)

func (c Config) withDefaults() (Config, error) {
	if !appName.MatchString(c.App) {
		return c, fmt.Errorf("앱 이름은 소문자, 숫자, 하이픈으로 32자 이하여야 함 (받은 값 %q)", c.App)
	}
	if c.ID == "" {
		c.ID = randomID()
	}
	if !sandboxID.MatchString(c.ID) {
		return c, fmt.Errorf("ID는 소문자·숫자 4자리여야 함 (받은 값 %q)", c.ID)
	}
	if c.TTL == 0 {
		c.TTL = time.Hour
	}
	if c.CPU == "" {
		c.CPU = "4"
	}
	if c.Memory == "" {
		c.Memory = "8Gi"
	}
	if c.Pods == 0 {
		c.Pods = 20
	}
	for _, q := range []string{c.CPU, c.Memory} {
		if _, err := resource.ParseQuantity(q); err != nil {
			return c, fmt.Errorf("자원 값 %q를 읽을 수 없음: %w", q, err)
		}
	}
	return c, nil
}

// Namespace는 이 설정으로 만들 네임스페이스 이름이다.
func (c Config) Namespace() string { return namePrefix + c.App + "-" + c.ID }

func randomID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// Objects는 샌드박스에 만들 리소스를 만드는 순서대로 담는다.
type Objects struct {
	Namespace      *corev1.Namespace
	ServiceAccount *corev1.ServiceAccount
	Role           *rbacv1.Role
	RoleBinding    *rbacv1.RoleBinding
	NetworkPolicy  *netv1.NetworkPolicy
	ResourceQuota  *corev1.ResourceQuota
	LimitRange     *corev1.LimitRange
}

// aiRules는 AI 계정이 자기 네임스페이스 안에서 할 수 있는 일이다.
// Helm 설치·삭제와 스모크 테스트에 필요한 만큼만 준다. Role, RoleBinding, NetworkPolicy,
// ResourceQuota, LimitRange, serviceaccounts/token, pods/exec는 주지 않는다. 격리를 스스로 풀 수 없게 하기 위해서다.
// secrets는 Helm이 릴리스 기록을 secret으로 남기기 때문에 필요하다. 샌드박스에는 테스트 데이터만 있다.
var aiRules = []rbacv1.PolicyRule{
	{
		APIGroups: []string{""},
		Resources: []string{"pods", "services", "configmaps", "secrets", "persistentvolumeclaims", "serviceaccounts"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	},
	{APIGroups: []string{""}, Resources: []string{"pods/log", "events"}, Verbs: []string{"get", "list", "watch"}},
	{
		APIGroups: []string{"apps"},
		Resources: []string{"deployments", "statefulsets", "replicasets"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	},
	{
		APIGroups: []string{"batch"},
		Resources: []string{"jobs", "cronjobs"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	},
	{
		APIGroups: []string{"policy"},
		Resources: []string{"poddisruptionbudgets"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	},
	{
		APIGroups: []string{"postgresql.cnpg.io"},
		Resources: []string{"clusters"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	},
}

// Build는 설정으로 리소스를 만든다. 클러스터에는 아무것도 보내지 않는다.
func Build(c Config, now time.Time) (Objects, error) {
	c, err := c.withDefaults()
	if err != nil {
		return Objects{}, err
	}
	ns := c.Namespace()
	labels := map[string]string{LabelSandbox: "true", LabelApp: c.App}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}
	}

	egress := []netv1.NetworkPolicyEgressRule{
		// 같은 네임스페이스 안
		{To: []netv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
		// kube-dns
		{
			To: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []netv1.NetworkPolicyPort{
				{Protocol: ptr(corev1.ProtocolUDP), Port: ptr(intstr.FromInt32(53))},
				{Protocol: ptr(corev1.ProtocolTCP), Port: ptr(intstr.FromInt32(53))},
			},
		},
	}
	for _, cidr := range c.AllowEgressCIDRs {
		egress = append(egress, netv1.NetworkPolicyEgressRule{To: []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: cidr}}}})
	}
	ingressFrom := []netv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	for _, n := range c.AllowIngressNamespaces {
		ingressFrom = append(ingressFrom, netv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": n}},
		})
	}

	cpu, mem := resource.MustParse(c.CPU), resource.MustParse(c.Memory)
	return Objects{
		Namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        ns,
			Labels:      labels,
			Annotations: map[string]string{AnnoExpires: now.Add(c.TTL).UTC().Format(time.RFC3339)},
		}},
		ServiceAccount: &corev1.ServiceAccount{
			ObjectMeta:                   meta(AIServiceAccount),
			AutomountServiceAccountToken: ptr(false),
		},
		Role: &rbacv1.Role{ObjectMeta: meta(AIServiceAccount), Rules: aiRules},
		RoleBinding: &rbacv1.RoleBinding{
			ObjectMeta: meta(AIServiceAccount),
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: AIServiceAccount, Namespace: ns}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: AIServiceAccount},
		},
		// 기본 전부 차단. 같은 네임스페이스와 kube-dns만 연다.
		// 운영망, Tailscale 대역(100.64.0.0/10), 메타데이터 주소(169.254.169.254)는 허용 목록에 없으므로 막힌다.
		NetworkPolicy: &netv1.NetworkPolicy{
			ObjectMeta: meta("sandbox-isolation"),
			Spec: netv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
				Ingress:     []netv1.NetworkPolicyIngressRule{{From: ingressFrom}},
				Egress:      egress,
			},
		},
		ResourceQuota: &corev1.ResourceQuota{
			ObjectMeta: meta("sandbox-quota"),
			Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    cpu,
				corev1.ResourceLimitsCPU:      cpu,
				corev1.ResourceRequestsMemory: mem,
				corev1.ResourceLimitsMemory:   mem,
				corev1.ResourcePods:           *resource.NewQuantity(int64(c.Pods), resource.DecimalSI),
				// 샌드박스를 밖에 노출하지 않는다
				corev1.ResourceServicesLoadBalancers: resource.MustParse("0"),
				corev1.ResourceServicesNodePorts:     resource.MustParse("0"),
			}},
		},
		// 자원을 적지 않은 Pod도 Quota 안에서 뜨도록 기본값을 준다
		LimitRange: &corev1.LimitRange{
			ObjectMeta: meta("sandbox-defaults"),
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			}}},
		},
	}, nil
}

func ptr[T any](v T) *T { return &v }
