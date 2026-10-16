package services

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Linux-only: Go reads SSL_CERT_FILE only on Linux, so the file name (not a t.Skip) keeps the
// test out of other platforms — a --- SKIP would turn run-integration.sh red on a Mac.

func TestSMTPEchoMask_RealSTARTTLS(t *testing.T) {
	caPEM, cert := selfSignedLoopbackCA(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveRejectingSMTP(ln, cert)

	cmd := exec.Command(os.Args[0], "-test.run=^TestSMTPEchoMask_Child$", "-test.v")
	cmd.Env = append(os.Environ(), smtpEchoChildEnv+"="+ln.Addr().String(), "SSL_CERT_FILE="+caFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestSMTPEchoMask_Child") {
		t.Fatalf("child did not run (a -run matching 0 tests is a false green):\n%s", out)
	}
}
