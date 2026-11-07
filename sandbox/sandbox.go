// Package sandbox는 리허설용 샌드박스 네임스페이스를 만들고 지우고, 격리가 실제로 되는지 검사한다.
// 샌드박스는 대상 클러스터에 만든다 (설계 문서 6장).
package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
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

	// AIServiceAccount는 샌드박스 토큰의 계정이다. 리허설, 엔진, AI 진단 도구가 이 토큰으로만 샌드박스를 다룬다.
	AIServiceAccount = "ai-sandbox"

	namePrefix = "sandbox-"

	// DefaultCPU, DefaultMemory는 샌드박스 Quota 기본값(데모 값)이다. 리허설은 대상 매니페스트만큼 더 잡는다.
	DefaultCPU    = "4"
	DefaultMemory = "8Gi"
	// DefaultContainerCPU, DefaultContainerMemory는 자원을 적지 않은 컨테이너에 LimitRange가 주는 limits다.
	DefaultContainerCPU    = "500m"
	DefaultContainerMemory = "512Mi"

	// PodSecurityEnforce, PodSecurityLevel은 샌드박스 네임스페이스에 거는 Pod Security Admission 기준이다.
	// CNPG 인스턴스 Pod도 뜰 수 있게 restricted가 아니라 baseline을 쓴다.
	PodSecurityEnforce = "pod-security.kubernetes.io/enforce"
	PodSecurityLevel   = "baseline"
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
	// AllowDNSCIDRs는 DNS(53번 포트, UDP와 TCP)만 허용할 주소 대역이다. GKE의 NodeLocal DNSCache처럼
	// Pod가 kube-dns 대신 노드의 링크 로컬 주소로 DNS를 묻는 클러스터에서 쓴다. 다른 포트는 열지 않고,
	// 메타데이터 주소(169.254.169.254)를 덮는 대역은 거절한다.
	AllowDNSCIDRs []string
	// AllowDNSAnywhere는 DNS(53번 포트)를 모든 목적지로 허용한다(메타데이터 주소만 제외). kube-dns Service의 주소를
	// 읽지 못해 정확한 대역을 모를 때의 대비다. infra ADR 0052가 같은 GKE에서 이렇게 해서 DNS 장애를 풀었다.
	AllowDNSAnywhere bool
}

// CNPGOperatorNamespace는 CloudNativePG 오퍼레이터의 기본 네임스페이스다.
const CNPGOperatorNamespace = "cnpg-system"

const (
	// NodeLocalDNSCIDR은 GKE NodeLocal DNSCache의 수신 주소다. 메타데이터 주소와는 다른 주소다.
	// NodeLocal DNSCache가 있는 클러스터(GKE)에서 Pod는 kube-dns가 아니라 이 주소로 DNS를 묻는다.
	// 정책이 kube-dns Pod만 허용하면 이름이 풀리지 않아 격리 검사의 dns 항목이 막힌다(2026-11-07 GKE 리허설).
	// 그 DaemonSet을 읽어 있을 때만 여는 방식은 관리 계정에 읽기 권한이 없으면 조용히 건너뛰어 같은 실패가 났으므로,
	// 클러스터를 읽지 않고 항상 이 주소의 DNS 포트만 연다. 다른 클러스터에는 이 주소에 듣는 것이 없어 영향이 없다.
	NodeLocalDNSCIDR = "169.254.20.10/32"
	// metadataIP는 어떤 허용 대역도 덮으면 안 되는 클라우드 메타데이터 주소다.
	metadataIP = "169.254.169.254"
)

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
	// kubernetes 서비스 주소(ClusterIP)도 연다. EKS VPC CNI의 정책 에이전트는 서비스 주소로 판정해서,
	// 엔드포인트만 열면 Pod 안의 instance manager가 kubernetes.default로 API에 붙지 못한다.
	// 2026-11-05 실제 EKS에서 엔드포인트 주소는 열리고 172.21.0.1:443은 막혔고, 그 결과 정책이 붙기 전에
	// API에 붙지 못한 임시 원본은 app 계정 비밀번호를 받지 못했다("User app has no password assigned").
	if svc, err := cs.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{}); err == nil {
		for _, ip := range svc.Spec.ClusterIPs {
			if ip == "" || ip == corev1.ClusterIPNone {
				continue
			}
			cidr := ip + "/32"
			if strings.Contains(ip, ":") {
				cidr = ip + "/128"
			}
			if !slices.Contains(c.AllowEgressCIDRs, cidr) {
				c.AllowEgressCIDRs = append(c.AllowEgressCIDRs, cidr)
			}
		}
	}
	// GKE의 NodeLocal DNSCache(hostNetwork)는 kube-dns ClusterIP로 가는 DNS를 가로채서 응답한다. Pod의 DNS 서버는
	// 그 ClusterIP라서, 정책이 kube-dns Pod selector만 보면 막힌다(infra ADR 0052, 같은 GKE에서 확인).
	// 그래서 kube-dns Service의 주소도 DNS 포트만 연다. 읽지 못하면 정확한 대역을 모르므로 DNS 포트를
	// 모든 목적지로 연다(메타데이터 주소 제외). 조용히 건너뛰어 같은 실패가 나는 것보다 낫다.
	c.AllowDNSCIDRs = append([]string{}, c.AllowDNSCIDRs...)
	found := false
	if svc, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{}); err == nil {
		for _, ip := range svc.Spec.ClusterIPs {
			if ip == "" || ip == corev1.ClusterIPNone {
				continue
			}
			cidr := ip + "/32"
			if strings.Contains(ip, ":") {
				cidr = ip + "/128"
			}
			found = true
			if !slices.Contains(c.AllowDNSCIDRs, cidr) {
				c.AllowDNSCIDRs = append(c.AllowDNSCIDRs, cidr)
			}
		}
	}
	if !found {
		c.AllowDNSAnywhere = true
	}
	return c, nil
}

// APIServerIPs는 default 네임스페이스의 kubernetes 서비스 엔드포인트 주소다.
// kindnet 같은 구현은 서비스 IP가 아니라 변환된 엔드포인트 주소로 판정하므로 이 주소를 열어야 한다
// (EKS는 서비스 주소로 판정해서 ForCNPG가 서비스 주소도 연다).
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
	// 음수 TTL은 만들자마자 만료돼 Sweep이 리허설 도중에 지울 수 있다.
	if c.TTL < 0 || c.Pods < 0 {
		return c, fmt.Errorf("TTL과 Pod 수는 0(기본값) 이상이어야 함 (받은 값 %s, %d)", c.TTL, c.Pods)
	}
	if c.TTL == 0 {
		c.TTL = time.Hour
	}
	if c.CPU == "" {
		c.CPU = DefaultCPU
	}
	if c.Memory == "" {
		c.Memory = DefaultMemory
	}
	if c.Pods == 0 {
		c.Pods = 20
	}
	for _, cidr := range c.AllowDNSCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return c, fmt.Errorf("DNS 허용 대역 %q를 읽을 수 없음: %w", cidr, err)
		}
		if n.Contains(net.ParseIP(metadataIP)) {
			return c, fmt.Errorf("DNS 허용 대역 %q가 메타데이터 주소 %s를 덮음", cidr, metadataIP)
		}
	}
	for _, q := range []string{c.CPU, c.Memory} {
		v, err := resource.ParseQuantity(q)
		if err != nil {
			return c, fmt.Errorf("자원 값 %q를 읽을 수 없음: %w", q, err)
		}
		if v.Sign() <= 0 {
			return c, fmt.Errorf("자원 값 %q는 0보다 커야 함", q)
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

// aiRules는 샌드박스 토큰으로 하는 일만큼만 준다. 리허설과 엔진이 CNPG Cluster를 만들고 고치고,
// 진단 도구와 멈춤 감지가 Pod·Pod 로그·PVC·이벤트를 읽고, 리허설이 임시 원본의 앱 계정 secret을 이름으로 읽는다.
// Pod, Service, Job처럼 워크로드를 만드는 권한은 주지 않는다. Service externalIPs로 클러스터의 다른 트래픽을
// 가로채거나(CVE-2020-8554), 다른 계정으로 Pod를 띄우는 길을 막기 위해서다. Role, RoleBinding, NetworkPolicy,
// ResourceQuota, LimitRange, serviceaccounts/token, pods/exec도 주지 않는다. 격리를 스스로 풀 수 없게 하기 위해서다.
var aiRules = []rbacv1.PolicyRule{
	{
		APIGroups: []string{"postgresql.cnpg.io"},
		Resources: []string{"clusters"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	},
	{APIGroups: []string{""}, Resources: []string{"pods", "pods/log", "persistentvolumeclaims", "events"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
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

	dnsPorts := func() []netv1.NetworkPolicyPort {
		return []netv1.NetworkPolicyPort{
			{Protocol: ptr(corev1.ProtocolUDP), Port: ptr(intstr.FromInt32(53))},
			{Protocol: ptr(corev1.ProtocolTCP), Port: ptr(intstr.FromInt32(53))},
		}
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
			Ports: dnsPorts(),
		},
	}
	// 노드의 DNS 캐시 같은 주소는 DNS 포트만 연다. NodeLocal DNSCache 주소는 항상 포함한다.
	dnsCIDRs := []string{NodeLocalDNSCIDR}
	for _, cidr := range c.AllowDNSCIDRs {
		if !slices.Contains(dnsCIDRs, cidr) {
			dnsCIDRs = append(dnsCIDRs, cidr)
		}
	}
	for _, cidr := range dnsCIDRs {
		egress = append(egress, netv1.NetworkPolicyEgressRule{
			To:    []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: cidr}}},
			Ports: dnsPorts(),
		})
	}
	if c.AllowDNSAnywhere {
		egress = append(egress, netv1.NetworkPolicyEgressRule{
			To:    []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{metadataIP + "/32"}}}},
			Ports: dnsPorts(),
		})
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

	// Pod 보안 기준(baseline)을 강제한다. hostNetwork Pod는 NetworkPolicy를 받지 않고, hostPath·privileged Pod는
	// 노드를 연다. AI 계정이 Pod를 만들 수 있으므로 이 라벨이 없으면 격리를 스스로 벗어날 수 있다.
	nsLabels := map[string]string{
		PodSecurityEnforce:                           PodSecurityLevel,
		"pod-security.kubernetes.io/enforce-version": "latest",
	}
	maps.Copy(nsLabels, labels)

	cpu, mem := resource.MustParse(c.CPU), resource.MustParse(c.Memory)
	return Objects{
		Namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        ns,
			Labels:      nsLabels,
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
				Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(DefaultContainerCPU), corev1.ResourceMemory: resource.MustParse(DefaultContainerMemory)},
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			}}},
		},
	}, nil
}

func ptr[T any](v T) *T { return &v }
