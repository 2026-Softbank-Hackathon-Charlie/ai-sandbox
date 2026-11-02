// sandboxctl은 샌드박스를 손으로 만들고 검사하고 지우는 도구다. 리허설 엔진이 붙기 전 확인용이다.
//
//	sandboxctl -kubeconfig <파일> create -app shop
//	sandboxctl -kubeconfig <파일> check  -ns sandbox-shop-ab12 -prod-ns shop [-target db=10.0.1.5:5432]
//	sandboxctl -kubeconfig <파일> delete -ns sandbox-shop-ab12
//	sandboxctl -kubeconfig <파일> sweep
//	sandboxctl -kubeconfig <파일> rehearse-check -app shop -prod-ns shop   (만들기 → 검사 → 지우기)
//
// 결과는 JSON으로 찍는다. 토큰 값은 찍지 않는다.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig 파일 (필수. 현재 컨텍스트를 실수로 쓰지 않도록 기본값이 없음)")
	kctx := flag.String("context", "", "kubeconfig 안의 컨텍스트 이름 (비우면 파일의 현재 컨텍스트)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "사용법: sandboxctl -kubeconfig <파일> <create|check|delete|sweep|rehearse-check> [옵션]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *kubeconfig == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: *kctx},
	).ClientConfig()
	must(err)
	cfg.Timeout = 30 * time.Second
	admin, err := kubernetes.NewForConfig(cfg)
	must(err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd, args := flag.Arg(0), flag.Args()[1:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	app := fs.String("app", "", "앱 이름")
	ns := fs.String("ns", "", "샌드박스 네임스페이스")
	prodNS := fs.String("prod-ns", "", "운영 앱 네임스페이스 (이 안의 secret이 막혀야 함)")
	image := fs.String("probe-image", "", "검사 Pod 이미지 (기본 busybox:1.37)")
	var targets targetFlags
	fs.Var(&targets, "target", "막혀야 하는 곳 이름=호스트:포트 (여러 번 가능). 예: db=10.0.1.5:5432")
	must(fs.Parse(args))

	check := func(sb *sandbox.Sandbox) bool {
		tok, exp, err := sandbox.Token(ctx, admin, sb, 0, time.Now())
		must(err)
		ai, err := kubernetes.NewForConfig(sandbox.TokenConfig(cfg, tok))
		must(err)
		rs, err := sandbox.CheckIsolation(ctx, admin, ai, sb, sandbox.IsolationConfig{
			ProdNamespace: *prodNS, Targets: targets, ProbeImage: *image,
		})
		ok := err == nil && sandbox.AllPass(rs)
		out := map[string]any{"namespace": sb.Namespace, "tokenExpiresAt": exp, "results": rs, "pass": ok}
		if err != nil {
			out["error"] = err.Error()
		}
		printJSON(out)
		return ok
	}

	switch cmd {
	case "create":
		sb, err := sandbox.Create(ctx, admin, sandbox.Config{App: *app}, time.Now())
		must(err)
		printJSON(sb)
	case "check":
		n, err := admin.CoreV1().Namespaces().Get(ctx, *ns, metav1.GetOptions{})
		must(err)
		if n.Labels[sandbox.LabelSandbox] != "true" {
			must(fmt.Errorf("%s는 샌드박스가 아님", *ns))
		}
		exp, _ := time.Parse(time.RFC3339, n.Annotations[sandbox.AnnoExpires])
		if !check(&sandbox.Sandbox{Namespace: *ns, App: n.Labels[sandbox.LabelApp], ExpiresAt: exp}) {
			os.Exit(1)
		}
	case "delete":
		must(sandbox.Delete(ctx, admin, *ns))
		printJSON(map[string]string{"deleted": *ns})
	case "sweep":
		res, err := sandbox.Sweep(ctx, admin, time.Now())
		printJSON(res)
		must(err)
	case "rehearse-check":
		sb, err := sandbox.Create(ctx, admin, sandbox.Config{App: *app}, time.Now())
		must(err)
		ok := check(sb)
		must(sandbox.Delete(context.WithoutCancel(ctx), admin, sb.Namespace))
		if !ok {
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

type targetFlags []sandbox.Target

func (t *targetFlags) String() string { return fmt.Sprint(*t) }

func (t *targetFlags) Set(v string) error {
	name, addr, ok := strings.Cut(v, "=")
	i := strings.LastIndex(addr, ":")
	if !ok || i < 0 {
		return fmt.Errorf("이름=호스트:포트 형식이어야 함: %q", v)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return fmt.Errorf("포트가 숫자가 아님: %q", v)
	}
	*t = append(*t, sandbox.Target{Name: name, Host: addr[:i], Port: port, Expect: sandbox.ExpectBlocked})
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
		os.Exit(1)
	}
}
