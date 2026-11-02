package rehearsal

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// Proxy는 로컬 주소로 들어온 연결을 CNPG 클러스터의 현재 primary Pod 5432번으로 보낸다.
// 엔진은 DSN으로 DB에 붙는데, 샌드박스 DB는 클러스터 안에만 있으므로 port-forward를 거친다.
// 연결마다 primary를 다시 찾으므로 전환(승격, 강등) 뒤에도 -rw 서비스처럼 동작한다.
// port-forward는 kubelet을 거쳐 Pod 안으로 들어가므로 샌드박스 NetworkPolicy를 풀지 않는다.
type Proxy struct {
	cs      kubernetes.Interface
	cfg     *rest.Config
	ns      string
	cluster string
	ln      net.Listener
}

// NewProxy는 127.0.0.1의 빈 포트에서 듣기 시작한다. cfg는 pods/portforward 권한이 있어야 한다(관리자 쪽).
func NewProxy(cs kubernetes.Interface, cfg *rest.Config, ns, cluster string) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{cs: cs, cfg: cfg, ns: ns, cluster: cluster, ln: ln}
	go p.serve()
	return p, nil
}

// Port는 엔진 DSN에 넣을 로컬 포트다.
func (p *Proxy) Port() int { return p.ln.Addr().(*net.TCPAddr).Port }

// Close는 새 연결을 받지 않는다. 열려 있던 연결은 각자 끝날 때 port-forward를 닫는다.
func (p *Proxy) Close() { _ = p.ln.Close() }

func (p *Proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

// handle은 연결마다 port-forward를 따로 연다. client-go의 port-forward는 연결 하나에서 오류가 나면
// 전체를 닫으므로, 여러 연결이 나눠 쓰면 한 연결이 끊길 때 다른 연결까지 끊긴다.
func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	var up net.Conn
	var stop chan struct{}
	for try := 0; try < 3 && up == nil; try++ {
		if try > 0 {
			time.Sleep(time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		pod, err := p.primary(ctx)
		cancel()
		if err != nil {
			continue
		}
		port, s, err := p.forward(pod)
		if err != nil {
			continue
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 10*time.Second)
		if err != nil {
			close(s)
			continue
		}
		up, stop = conn, s
	}
	if up == nil {
		return
	}
	defer close(stop)
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

// primary는 클러스터의 primary(복제본 클러스터면 지정 primary) Pod를 찾는다. -rw 서비스와 같은 기준이다.
func (p *Proxy) primary(ctx context.Context) (string, error) {
	pods, err := p.cs.CoreV1().Pods(p.ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + p.cluster})
	if err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		role := pod.Labels["cnpg.io/instanceRole"]
		if role == "" {
			role = pod.Labels["role"]
		}
		if role == "primary" && pod.Status.Phase == corev1.PodRunning && ready(&pod) {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("%s/%s에 준비된 primary Pod가 없음", p.ns, p.cluster)
}

func ready(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// forward는 Pod 5432번으로 가는 port-forward를 빈 로컬 포트에 연다. stop을 닫으면 끝난다.
func (p *Proxy) forward(pod string) (int, chan struct{}, error) {
	rt, upgrader, err := spdy.RoundTripperFor(p.cfg)
	if err != nil {
		return 0, nil, err
	}
	url := p.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(p.ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, url)
	stop, readyCh := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:5432"}, stop, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-errCh:
		return 0, nil, fmt.Errorf("port-forward %s: %w", pod, err)
	case <-time.After(15 * time.Second):
		close(stop)
		return 0, nil, fmt.Errorf("port-forward %s 시간 초과", pod)
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stop)
		return 0, nil, fmt.Errorf("port-forward 포트를 알 수 없음: %v", err)
	}
	return int(ports[0].Local), stop, nil
}
