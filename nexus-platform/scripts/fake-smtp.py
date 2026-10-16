#!/usr/bin/env python3
"""A throwaway SMTP submission server with STARTTLS, for end-to-end checks of the mail path.

It speaks just enough SMTP for notify.SMTPSender (EHLO → STARTTLS → EHLO → MAIL → RCPT → DATA →
QUIT) and either ACCEPTS every message or, with --reject-rcpt, answers RCPT TO the way real relays
reject an unknown mailbox — echoing the address back, lowercased:

    550 5.1.1 <nguyen.thi@gmail.com>: Recipient address rejected: User unknown

That echo is exactly what queue #96 needs: the only way the rendered recipient could leak into
sys_jobs.last_error is through a server that repeats it. Every command line is printed to stdout so
the run shows what the client actually sent.

The sender verifies the certificate against the system roots, so the Nexus server under test must
trust --cert: on Linux run it with SSL_CERT_FILE=<ca.pem> (macOS ignores that variable — run the
server in a Linux container). Make a CA + a leaf for the host name the server dials, e.g.:

    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=fake-smtp-ca -keyout ca.key -out ca.pem
    openssl req -newkey rsa:2048 -nodes -subj /CN=host.docker.internal -keyout smtp.key -out smtp.csr
    printf 'subjectAltName=DNS:host.docker.internal' > san.ext
    openssl x509 -req -in smtp.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 1 -extfile san.ext -out smtp.pem

Usage: python3 scripts/fake-smtp.py --cert smtp.pem --key smtp.key [--port 2525] [--reject-rcpt]
ponytail: stdlib only, one connection at a time — a test fixture, not a mail server.
"""
import argparse
import re
import socket
import ssl

ap = argparse.ArgumentParser()
ap.add_argument("--host", default="127.0.0.1")
ap.add_argument("--port", type=int, default=2525)
ap.add_argument("--cert", required=True)
ap.add_argument("--key", required=True)
ap.add_argument("--reject-rcpt", action="store_true", help="550 every RCPT TO, echoing the address lowercased")
args = ap.parse_args()

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(args.cert, args.key)


def serve(conn):
    f = conn.makefile("rwb")

    def say(line):
        f.write((line + "\r\n").encode())
        f.flush()

    say("220 fake-smtp ESMTP ready")
    tls = False
    while True:
        raw = f.readline()
        if not raw:
            return
        line = raw.decode(errors="replace").rstrip("\r\n")
        print(("TLS " if tls else "    ") + "C: " + line, flush=True)
        verb = line.split(" ", 1)[0].upper()
        if verb in ("EHLO", "HELO"):
            if tls:
                say("250 fake-smtp")
            else:
                say("250-fake-smtp")
                say("250 STARTTLS")
        elif verb == "STARTTLS" and not tls:
            say("220 2.0.0 ready to start TLS")
            conn = ctx.wrap_socket(conn, server_side=True)
            f = conn.makefile("rwb")
            tls = True
        elif verb == "MAIL":
            say("250 2.1.0 ok")
        elif verb == "RCPT":
            m = re.search(r"<([^>]*)>", line)
            addr = (m.group(1) if m else "").lower()
            if args.reject_rcpt:
                say(f"550 5.1.1 <{addr}>: Recipient address rejected: User unknown")
            else:
                say("250 2.1.5 ok")
        elif verb == "DATA":
            say("354 end with <CRLF>.<CRLF>")
            while f.readline().rstrip(b"\r\n") != b".":
                pass
            say("250 2.0.0 queued")
        elif verb == "RSET":
            say("250 2.0.0 ok")
        elif verb == "QUIT":
            say("221 2.0.0 bye")
            return
        else:
            say("502 5.5.2 not implemented")


srv = socket.create_server((args.host, args.port))
print(f"fake-smtp on {args.host}:{args.port} ({'REJECT every RCPT, echoing it' if args.reject_rcpt else 'accept all'})",
      flush=True)
while True:
    c, _ = srv.accept()
    try:
        serve(c)
    except (OSError, ssl.SSLError) as e:
        print(f"    connection error: {e}", flush=True)
    finally:
        c.close()
