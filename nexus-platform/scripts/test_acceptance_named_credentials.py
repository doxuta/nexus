"""Unit checks for the pure decisions inside scripts/acceptance-named-credentials.py.

Run: python3 scripts/test_acceptance_named_credentials.py   (from nexus-platform/)

The end-to-end proof is running the gate against a live server; these cases pin what its output
cannot show: that the signature it verifies against is the right formula (a wrong verifier would
fail a correct server or pass a broken one), that it refuses a non-loopback server, and that the
self-healing sweep can only ever select rows this script minted and abandoned.
"""

import base64
import importlib.util
import os
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-named-credentials.py")
_spec = importlib.util.spec_from_file_location("acceptance_named_credentials", _path)
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)


class Signatures(unittest.TestCase):
    def test_v1_is_hmac_sha256_hex_of_raw_body(self):
        # RFC 4231 test case 2 — the same primitive the Go sender uses (hex of HMAC-SHA256).
        self.assertEqual(gate.sig_v1("Jefe", b"what do ya want for nothing?"),
                         "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843")

    def test_v2_signs_timestamp_dot_body(self):
        body = b'{"workflowId":"w"}'
        self.assertEqual(gate.sig_v2("k" * 32, "1790651431", body),
                         gate.sig_v1("k" * 32, b"1790651431." + body))
        self.assertNotEqual(gate.sig_v2("k" * 32, "1790651431", body), gate.sig_v1("k" * 32, body))


class MasterKeyID(unittest.TestCase):
    """S1a.7 expects the id the server logs; the S7 precheck recomputes it from .env the same way."""

    def test_label_dot_fingerprint_of_the_key(self):
        # Same vector as crypto TestKeyring_KeyIDFormula (key = bytes 0x00..0x1f).
        key = base64.b64encode(bytes(range(32))).decode()
        self.assertEqual(gate.master_key_id("v1", key), "v1.327c8670")

    def test_same_label_different_key_differs(self):
        a = base64.b64encode(b"a" * 32).decode()
        b = base64.b64encode(b"b" * 32).decode()
        self.assertNotEqual(gate.master_key_id("v1", a), gate.master_key_id("v1", b))


class Loopback(unittest.TestCase):
    def test_only_this_machine(self):
        for base, want in (("http://localhost:8090/api/v1", True),
                           ("http://127.0.0.1:8162/api/v1", True),
                           ("http://[::1]:8090/api/v1", True),
                           ("https://nexus.example.com/api/v1", False),
                           ("http://10.0.0.5:8090/api/v1", False),
                           ("http://localhost.evil.example/api/v1", False)):
            self.assertEqual(gate.is_loopback_base(base), want, base)


class SweepMarker(unittest.TestCase):
    NOW = 1790651431

    def test_only_our_abandoned_marks(self):
        old = self.NOW - gate.STALE_AFTER - 1
        for text, want in (
            (f"nc-nghiemthu-{old}-f915c0e0@acceptance.invalid", True),
            (f"zz nghiệm thu kho khoá nc-nghiemthu-{old}-f915c0e0", True),
            (f"nc-nghiemthu-{self.NOW - 60}-f915c0e0@acceptance.invalid", False),  # a run in progress
            (f"xnc-nghiemthu-{old}-f915c0e0", False),                             # lookalike prefix
            (f"nc-nghiemthu-{old}-f915c0e0extra", False),                         # lookalike suffix
            (f"nghiemthu-{old}-f915c0e0@acceptance.invalid", False),              # acceptance-acl.py's accounts
            ("admin@example.com", False),
            ("", False),
            (None, False),
        ):
            self.assertEqual(gate.stale_mark(text, self.NOW), want, text)

    def test_new_mark_is_fresh_and_matches(self):
        mark = gate.new_mark()
        self.assertRegex(mark, gate.MARK_RE)
        self.assertFalse(gate.stale_mark(mark, self.NOW))


class CloneDSN(unittest.TestCase):
    """--slice S1a boots private servers: they must only ever point at a nexus_wt_* clone."""

    SHARED = "root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local"

    def test_repoints_only_the_database(self):
        self.assertEqual(gate.clone_dsn(self.SHARED, "nexus_wt_named_credentials_crypto_store"),
                         "root:@tcp(127.0.0.1:3306)/nexus_wt_named_credentials_crypto_store"
                         "?charset=utf8mb4&parseTime=True&loc=Local")
        self.assertEqual(gate.clone_dsn("u:p@tcp(db:3306)/nexus_platform", "nexus_wt_x"),
                         "u:p@tcp(db:3306)/nexus_wt_x")

    def test_refuses_anything_but_a_clone(self):
        for db in ("nexus_platform", "nexus_wt_", "nexus_wt_X", "nexus_wt_a;drop", "other_db", ""):
            with self.assertRaises(ValueError, msg=db):
                gate.clone_dsn(self.SHARED, db)

    def test_refuses_an_unrecognised_dsn(self):
        with self.assertRaises(ValueError):
            gate.clone_dsn("mysql://root@localhost/nexus_platform", "nexus_wt_x")


class CloneSQL(unittest.TestCase):
    """--slice S1b reads/writes the clone through the mysql CLI (no placeholders): it must refuse the
    shared database and any spliced value that is not a UUID — before docker is ever invoked."""

    class Args:
        mysql_container = "nexus-mysql"

        def __init__(self, db):
            self.db = db

    def test_refuses_the_shared_db(self):
        for db in ("nexus_platform", "", "nexus_wt_a;drop"):
            with self.assertRaises(gate.EnvError, msg=db):
                gate.clone_sql(self.Args(db), "SELECT 1")

    def test_refuses_a_non_uuid_value(self):
        for v in ("x' OR '1'='1", "", "1b4e28ba-2fa1-11d2-883f-0016d3cca427'--", "1B4E28BA-2FA1-11D2-883F-0016D3CCA427"):
            with self.assertRaises(gate.EnvError, msg=v):
                gate.clone_sql(self.Args("nexus_wt_x"), "SELECT '%s'", v)


class LegacyActionsSQL(unittest.TestCase):
    """--slice S2a/S3a plant a rule saved before S3a (an inline key no API write accepts any more) with
    one UPDATE through the same placeholder-less CLI: only a UUID pair and JSON drawn from a fixed
    safe alphabet may be spliced — no quote, backslash or percent (clone_sql formats with %)."""

    T, W = "1b4e28ba-2fa1-11d2-883f-0016d3cca427", "6fa459ea-ee8a-3ca4-894e-db77e160355e"

    def test_pins_tenant_and_rule(self):
        acts = [{"type": "outbound_webhook", "url": "http://u:ab12@127.0.0.1:9/x?token=cd34&src=nexus",
                 "secret": "ef56", "headers": {"Authorization": "Bearer 78gh"}}]
        sql = gate.legacy_actions_sql(self.T, self.W, acts)
        self.assertIn(f"WHERE tenant_id = '{self.T}' AND id = '{self.W}'", sql)
        self.assertTrue(sql.startswith("UPDATE sys_workflows SET actions = '"), sql)
        self.assertNotIn("%", sql)

    def test_refuses_unsafe_json(self):
        for v in ("a'b", "a\\b", "50%", "é"):
            with self.assertRaises(ValueError, msg=v):
                gate.legacy_actions_sql(self.T, self.W, [{"type": "outbound_webhook", "url": "http://h/x", "secret": v}])

    def test_refuses_a_non_uuid_id(self):
        for t, w in (("x' OR '1'='1", self.W), (self.T, ""), (self.T, self.W + "'--")):
            with self.assertRaises(ValueError):
                gate.legacy_actions_sql(t, w, [{"type": "outbound_webhook", "url": "http://h/x"}])


if __name__ == "__main__":
    unittest.main()
