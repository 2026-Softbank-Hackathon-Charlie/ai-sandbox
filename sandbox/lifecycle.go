package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Sandbox는 만들어진 샌드박스다.
type Sandbox struct {
	Namespace string
	App       string
	ExpiresAt time.Time
}

// Create는 샌드박스 리소스를 만든다. 중간에 실패하면 만든 네임스페이스를 지우고 오류를 돌려준다.
func Create(ctx context.Context, cs kubernetes.Interface, c Config, now time.Time) (*Sandbox, error) {
	o, err := Build(c, now)
	if err != nil {
		return nil, err
	}
	ns := o.Namespace.Name
	if _, err := cs.CoreV1().Namespaces().Create(ctx, o.Namespace, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("네임스페이스 %s 생성 실패: %w", ns, err)
	}

	steps := []struct {
		what string
		do   func() error
	}{
		// 격리 정책을 가장 먼저 만든다. 다른 리소스가 먼저 생겨도 열린 상태로 뜨지 않게 하기 위해서다.
		{"NetworkPolicy", func() error {
			_, err := cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, o.NetworkPolicy, metav1.CreateOptions{})
			return err
		}},
		{"ResourceQuota", func() error {
			_, err := cs.CoreV1().ResourceQuotas(ns).Create(ctx, o.ResourceQuota, metav1.CreateOptions{})
			return err
		}},
		{"LimitRange", func() error {
			_, err := cs.CoreV1().LimitRanges(ns).Create(ctx, o.LimitRange, metav1.CreateOptions{})
			return err
		}},
		{"ServiceAccount", func() error {
			_, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, o.ServiceAccount, metav1.CreateOptions{})
			return err
		}},
		{"Role", func() error {
			_, err := cs.RbacV1().Roles(ns).Create(ctx, o.Role, metav1.CreateOptions{})
			return err
		}},
		{"RoleBinding", func() error {
			_, err := cs.RbacV1().RoleBindings(ns).Create(ctx, o.RoleBinding, metav1.CreateOptions{})
			return err
		}},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			// 정리는 원래 ctx가 끝났어도 시도한다
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if derr := Delete(cleanupCtx, cs, ns); derr != nil {
				return nil, fmt.Errorf("%s 생성 실패: %w (정리도 실패: %v)", s.what, err, derr)
			}
			return nil, fmt.Errorf("%s 생성 실패, 네임스페이스를 지움: %w", s.what, err)
		}
	}

	exp, _ := time.Parse(time.RFC3339, o.Namespace.Annotations[AnnoExpires])
	return &Sandbox{Namespace: ns, App: o.Namespace.Labels[LabelApp], ExpiresAt: exp}, nil
}

// ErrNotSandbox는 샌드박스가 아닌 네임스페이스를 지우려 할 때 돌려준다.
var ErrNotSandbox = errors.New("샌드박스 네임스페이스가 아님")

// Delete는 샌드박스 네임스페이스를 지운다. 이름과 라벨이 둘 다 맞을 때만 지운다.
// 운영 네임스페이스를 실수로 지우지 않기 위한 확인이다. 이미 없으면 성공으로 본다.
func Delete(ctx context.Context, cs kubernetes.Interface, namespace string) error {
	if !strings.HasPrefix(namespace, namePrefix) {
		return fmt.Errorf("%w: %s", ErrNotSandbox, namespace)
	}
	ns, err := cs.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Labels[LabelSandbox] != "true" {
		return fmt.Errorf("%w: %s (라벨 %s 없음)", ErrNotSandbox, namespace, LabelSandbox)
	}
	err = cs.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &ns.UID},
		PropagationPolicy: ptr(metav1.DeletePropagationForeground),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// MinTokenTTL은 API 서버가 받는 TokenRequest 최소 유효 시간이다.
const MinTokenTTL = 10 * time.Minute

// Token은 AI 계정의 짧은 토큰을 발급한다. 기본 1시간이고, 샌드박스 만료 시각을 넘지 않는다.
// 토큰 값은 로그에 남기지 않는다.
func Token(ctx context.Context, cs kubernetes.Interface, sb *Sandbox, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if ttl == 0 {
		ttl = time.Hour
	}
	if left := sb.ExpiresAt.Sub(now); left < ttl {
		ttl = left
	}
	if ttl < MinTokenTTL {
		return "", time.Time{}, fmt.Errorf("샌드박스 만료까지 %s 남아 토큰을 발급하지 않음 (최소 %s)", ttl.Round(time.Second), MinTokenTTL)
	}
	sec := int64(ttl / time.Second)
	tr, err := cs.CoreV1().ServiceAccounts(sb.Namespace).CreateToken(ctx, AIServiceAccount, &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &sec},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("토큰 발급 실패: %w", err)
	}
	return tr.Status.Token, tr.Status.ExpirationTimestamp.Time, nil
}

// TokenConfig는 base의 주소와 CA만 가져오고, 인증은 token 하나만 쓰는 설정을 만든다.
// base의 관리자 인증서나 exec 플러그인이 섞이지 않게 하기 위해서다.
func TokenConfig(base *rest.Config, token string) *rest.Config {
	c := rest.AnonymousClientConfig(base)
	c.BearerToken = token
	return c
}

// SweepResult는 만료 정리 결과다.
type SweepResult struct {
	Deleted []string
	// Skipped는 만료 시각을 읽을 수 없어 남겨 둔 네임스페이스다. 사람이 확인한다.
	Skipped []string
}

// Sweep은 만료 시각이 지난 샌드박스를 지운다. CronJob에서 주기적으로 부른다.
func Sweep(ctx context.Context, cs kubernetes.Interface, now time.Time) (SweepResult, error) {
	var res SweepResult
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelSandbox + "=true"})
	if err != nil {
		return res, err
	}
	var errs []error
	for _, ns := range list.Items {
		if ns.DeletionTimestamp != nil {
			continue
		}
		exp, err := time.Parse(time.RFC3339, ns.Annotations[AnnoExpires])
		if err != nil {
			res.Skipped = append(res.Skipped, ns.Name)
			continue
		}
		if now.Before(exp) {
			continue
		}
		if err := Delete(ctx, cs, ns.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ns.Name, err))
			continue
		}
		res.Deleted = append(res.Deleted, ns.Name)
	}
	return res, errors.Join(errs...)
}
