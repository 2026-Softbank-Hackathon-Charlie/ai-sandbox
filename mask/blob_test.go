package mask

import (
	"strings"
	"testing"
)

// TestStringKeysBlobsAndOpenPEM은 더 넓힌 키 이름, 이름 없이 나온 AWS 비밀 키와 긴 base64·hex 덩어리, 끝나지 않은 PEM을 지우고,
// 다이제스트와 git 커밋은 그대로 두는지 본다.
func TestStringKeysBlobsAndOpenPEM(t *testing.T) {
	awsSecret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40자
	for _, in := range []string{
		"DB_PASS=hunter2pw", "passphrase: s3cr3t-words", "salt=Qm9vYmFy", "APP_KEY=base64:Zm9vYmFy", "ENCRYPTION_KEY: abc123",
		"signing_key=abc", "MY_KEY=abc123",
		"aws " + awsSecret + " end",
		"blob " + strings.Repeat("Ab3", 20) + " end",
		"hex " + strings.Repeat("ab12", 13) + " end",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n(잘림)",
	} {
		out := String(in)
		if out == in || strings.Contains(out, "hunter2pw") || strings.Contains(out, awsSecret) || strings.Contains(out, "b3BlbnNzaC1") {
			t.Errorf("지워야 함: %q -> %q", in, out)
		}
	}
	for _, in := range []string{
		"FROM ghcr.io/x/app:1.2@sha256:" + strings.Repeat("ab", 32),
		"commit 9fceb02d0ae598e95dc970b74767f19372d61af8",
		"sum " + strings.Repeat("0f", 32) + "  app.tar.gz",
		"/usr/local/lib/python3.12/site-packages/some/long/path/name",
		"PRIMARY_KEYS=id", "key: value",
	} {
		if out := String(in); out != in {
			t.Errorf("그대로 둬야 함: %q -> %q", in, out)
		}
	}
}
