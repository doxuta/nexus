package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestNoopSender_ReportsThatItDiscarded pins the CHANGED contract. This test previously asserted
// the opposite — that the noop sender returns nil — which is how a server with no mail configured
// reported every message as sent. A workflow run then showed a green "email sent" step for a mail
// that never left the process, and a password-change alert audited as delivered to a user who was
// never told. Silent success is the more dangerous of the two failure modes here.
func TestNoopSender_ReportsThatItDiscarded(t *testing.T) {
	s := NewNoopSender()
	err := s.Send(context.Background(), Mail{To: "u@x.io", Subject: "hi", Body: "body"})
	if !errors.Is(err, ErrMailNotConfigured) {
		t.Fatalf("NoopSender.Send = %v, want ErrMailNotConfigured", err)
	}
}

func TestBuildMIME_TextOnly(t *testing.T) {
	out := buildMIME("from@x.io", Mail{To: "to@x.io", Subject: "Chào", Body: "Nội dung"})
	mustContain(t, out, "From: from@x.io")
	mustContain(t, out, "To: to@x.io")
	mustContain(t, out, "Content-Type: text/plain")
	mustContain(t, out, "Nội dung")
	if strings.Contains(out, "multipart/alternative") {
		t.Error("text-only mail should not be multipart")
	}
}

func TestBuildMIME_Multipart(t *testing.T) {
	out := buildMIME("from@x.io", Mail{
		To: "to@x.io", Subject: "Sub", Body: "plain", HTMLBody: "<p>html</p>",
	})
	mustContain(t, out, "multipart/alternative")
	mustContain(t, out, "text/plain")
	mustContain(t, out, "text/html")
	mustContain(t, out, "plain")
	mustContain(t, out, "<p>html</p>")
}

func TestEncodeHeader_NonASCIIIsEncoded(t *testing.T) {
	got := encodeHeader("Mật khẩu vừa đổi")
	// Q-encoded UTF-8 header begins with the charset token and never contains raw
	// non-ASCII bytes that would corrupt in transport.
	if !strings.HasPrefix(got, "=?utf-8?q?") {
		t.Errorf("expected RFC2047 Q-encoding, got %q", got)
	}
}

// TestSMTPSender_RefusesPlaintextWhenNoSTARTTLS drives the SMTPSender against an
// in-process fake SMTP server that does NOT advertise STARTTLS. The sender must refuse
// (never send AUTH/credentials over plaintext) and return an error — proving the
// STARTTLS-required guard. We inject the dialer so no real network/TLS is involved.
func TestSMTPSender_RefusesPlaintextWhenNoSTARTTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go fakeSMTPNoSTARTTLS(t, ln)

	s := NewSMTPSender(SMTPConfig{
		Host: "127.0.0.1", Port: 0, Username: "u", Password: "p", From: "from@x.io",
	})
	// Inject the dialer to hit our listener regardless of the cfg host:port string.
	s.dial = func(ctx context.Context, _ string) (net.Conn, error) {
		d := &net.Dialer{Timeout: time.Second}
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}

	err = s.Send(context.Background(), Mail{To: "to@x.io", Subject: "s", Body: "b"})
	if err == nil {
		t.Fatal("expected Send to fail when server lacks STARTTLS (must not send creds in cleartext)")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("expected STARTTLS-related error, got: %v", err)
	}
}

// fakeSMTPNoSTARTTLS plays just enough of SMTP to let smtp.NewClient + EHLO succeed,
// then advertises NO extensions (no STARTTLS), forcing the sender's guard to trip.
func fakeSMTPNoSTARTTLS(t *testing.T, ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s)) }

	w("220 fake ESMTP\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// Single-line greeting => no extensions advertised (no STARTTLS).
			w("250 fake\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			w("221 bye\r\n")
			return
		default:
			w("250 ok\r\n")
		}
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected to find %q in:\n%s", needle, haystack)
	}
}
