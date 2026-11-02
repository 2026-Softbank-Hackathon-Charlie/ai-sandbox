package mask

import (
	"strings"
	"testing"
)

func TestStringMasksSecrets(t *testing.T) {
	cases := []struct {
		name, in, secret string
	}{
		{"접속 문자열", "dial postgres://shop:S3cretPw@db.shop:5432/shop failed", "S3cretPw"},
		{"Bearer 토큰", "Authorization: Bearer abcdef1234567890xyz", "abcdef1234567890xyz"},
		{"JWT", "token=eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW0ifQ.c2lnbmF0dXJlMTIz", "eyJhbGciOiJSUzI1NiJ9"},
		{"AWS 키", "AWS_ACCESS_KEY_ID AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"Anthropic 키", "key sk-ant-api03-abcdefghijklmnop", "sk-ant-api03-abcdefghijklmnop"},
		{"GitHub 토큰", "ghp_abcdefghijklmnopqrstuvwxyz0123", "ghp_abcdefghijklmnopqrstuvwxyz0123"},
		{"Tailscale 키", "TS_AUTHKEY=tskey-auth-kAbc123-XyZ", "tskey-auth-kAbc123-XyZ"},
		{"YAML 비밀번호", "  password: hunter2hunter2", "hunter2hunter2"},
		{"따옴표 값", `  dbPassword: "p@ss word"`, "p@ss word"},
		{"env 형식", "DB_PASSWORD=topsecret", "topsecret"},
		{"JSON", `{"apiKey":"k-123456","name":"shop"}`, "k-123456"},
		{"client secret", "client_secret = abc.def.ghi", "abc.def.ghi"},
		{"PEM", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nIBAAK\n-----END RSA PRIVATE KEY-----", "MIIEow"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := String(c.in)
			if strings.Contains(out, c.secret) {
				t.Fatalf("비밀값이 남음: %q", out)
			}
			if !strings.Contains(out, Placeholder) {
				t.Fatalf("지운 자리 표시가 없음: %q", out)
			}
		})
	}
}

func TestStringKeepsDiagnosisClues(t *testing.T) {
	// 진단에 필요한 내용은 지우면 안 된다
	for _, line := range []string{
		`persistentvolumeclaim/shop-db-1 Pending: storageclass.storage.k8s.io "gp3" not found`,
		"0/3 nodes are available: 3 Insufficient memory.",
		"Back-off pulling image \"registry.example.com/shop/order:1.4.2\"",
		"dial tcp 10.0.1.5:5432: connect: connection refused",
		"tokenizer warmup done in 12ms", // 'token'이 들어가도 키=값이 아니면 그대로
	} {
		if out := String(line); out != line {
			t.Errorf("바뀌면 안 됨:\n  %q\n→ %q", line, out)
		}
	}
	// 키 이름만 남고 값만 지워진다
	if out := String("password: hunter2hunter2"); out != "password: ***" {
		t.Errorf("키 이름은 남아야 함: %q", out)
	}
	if out := String("postgres://shop:pw@db:5432/shop"); out != "postgres://shop:***@db:5432/shop" {
		t.Errorf("주소와 사용자 이름은 남아야 함: %q", out)
	}
}

func TestSecretKey(t *testing.T) {
	for k, want := range map[string]bool{
		"token": true, "apiKey": true, "DB_PASSWORD": true, "Authorization": true, "client_secret": true,
		"env": false, "name": false, "storageClass": false, "count": false,
	} {
		if got := SecretKey(k); got != want {
			t.Errorf("SecretKey(%q) = %v, want %v", k, got, want)
		}
	}
}
