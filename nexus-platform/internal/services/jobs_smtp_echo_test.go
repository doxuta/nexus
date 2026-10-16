package services

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/jobs"
	"nexus/platform/internal/notify"
)

// Queue #217 (from #96): the recipient mask must survive a REAL SMTP rejection that echoes the
// address lowercased. Real notify.SMTPSender (STARTTLS, cert verified against system roots) talks
// to an in-process fake that answers "550 <lowercased addr>" at RCPT TO; the job handler's error —
// the text that becomes sys_jobs.last_error — must carry "[redacted]" and never the address.
//
// Go reads SSL_CERT_FILE only on Linux and only once per process, so the sender half runs in a
// re-exec'd copy of this test binary. On macOS/Windows the env var is ignored: skip (CI is Linux).
const (
	smtpEchoChildEnv = "NEXUS_SMTP_ECHO_CHILD_ADDR"
	smtpEchoPII      = "Nguyen.Thi.KiemThu.CaNhan@Gmail.com" // mixed case on purpose
)

// TestSMTPEchoMask_Child is the sender half; it does nothing unless the parent set the env.
func TestSMTPEchoMask_Child(t *testing.T) {
	addr := os.Getenv(smtpEchoChildEnv)
	if addr == "" {
		// Helper-process idiom: return, not Skip — a --- SKIP line is what run-integration.sh
		// (and CI, queue #244) reads as "the environment decided the result".
		return
	}
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	sender := notify.NewSMTPSender(notify.SMTPConfig{Host: host, Port: port, From: "noreply@nexus.test"})

	payload, _ := json.Marshal(map[string]string{"to": smtpEchoPII, "subject": "s", "body": "b"})
	job := jobs.Job{ID: "j1", TenantID: "t1", Type: EmailAlertJobType, Payload: payload, Attempts: 1, MaxAttempts: 1}
	err := NewEmailAlertJobHandler(sender, nil, nil, zap.NewNop())(context.Background(), job)
	if err == nil {
		t.Fatal("handler succeeded; the fake SMTP must reject RCPT TO")
	}
	msg := err.Error()
	if !strings.Contains(msg, "550") {
		t.Fatalf("error %q lacks the SMTP 550 — the rejection never reached the handler (TLS trust?)", msg)
	}
	if strings.Contains(strings.ToLower(msg), strings.ToLower(smtpEchoPII)) {
		t.Fatalf("job error leaks the recipient (any case): %q", msg)
	}
	if !strings.Contains(msg, "<[redacted]>") {
		t.Fatalf("job error %q lacks <[redacted]>", msg)
	}
}

func selfSignedLoopbackCA(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake-smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var smtpAngle = regexp.MustCompile(`<([^>]*)>`)

// serveRejectingSMTP speaks just enough SMTP+STARTTLS and 550s every RCPT TO, echoing the address
// lowercased — what real relays do (mirror of scripts/fake-smtp.py --reject-rcpt).
func serveRejectingSMTP(ln net.Listener, cert tls.Certificate) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			var w net.Conn = c
			r, tlsOn := bufio.NewReader(c), false
			say := func(s string) { _, _ = fmt.Fprintf(w, "%s\r\n", s) }
			say("220 fake-smtp ready")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				switch strings.ToUpper(strings.SplitN(line, " ", 2)[0]) {
				case "EHLO", "HELO":
					if tlsOn {
						say("250 fake-smtp")
					} else {
						say("250-fake-smtp")
						say("250 STARTTLS")
					}
				case "STARTTLS":
					say("220 ready to start TLS")
					tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
					if tc.Handshake() != nil {
						return
					}
					w, r, tlsOn = tc, bufio.NewReader(tc), true
				case "MAIL":
					say("250 ok")
				case "RCPT":
					addr := ""
					if m := smtpAngle.FindStringSubmatch(line); m != nil {
						addr = strings.ToLower(m[1])
					}
					say("550 5.1.1 <" + addr + ">: Recipient address rejected: User unknown")
				case "QUIT":
					say("221 bye")
					return
				default:
					say("250 ok")
				}
			}
		}(conn)
	}
}
