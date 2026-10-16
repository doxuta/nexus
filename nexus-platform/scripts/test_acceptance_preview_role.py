"""Unit checks for the pure decisions inside scripts/acceptance-preview-role.py.

Run: python3 scripts/test_acceptance_preview_role.py   (from nexus-platform/)

The end-to-end proof is running the gate against a live server; these cases pin what its output cannot
show: that the comparison is on the full grant tuple (a level change must not compare equal), that the
audit check refuses a trail signed by the synthetic principal, and that writes need a loopback base.
"""

import importlib.util
import os
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-preview-role.py")
_spec = importlib.util.spec_from_file_location("acceptance_preview_role", _path)
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)


class PermSet(unittest.TestCase):
    def test_level_is_part_of_the_key(self):
        staff = gate.perm_set({"data": [{"entityName": "crm_opportunities", "action": "view", "accessLevel": "owner"}]})
        admin_like = gate.perm_set({"data": [{"entityName": "crm_opportunities", "action": "view", "accessLevel": "all"}]})
        self.assertNotEqual(staff, admin_like)

    def test_non_list_payload_is_not_comparable(self):
        self.assertIsNone(gate.perm_set({"data": {"isAdmin": True}}))

    def test_view_levels(self):
        perms = {("a", "view", "all"), ("a", "edit", "owner"), ("b", "view", "owner")}
        self.assertEqual(gate.view_levels(perms), {"a": "all", "b": "owner"})


class AuditTrail(unittest.TestCase):
    SID, BUILDER, PRINCIPAL = "s-1", "u-builder", "p-principal"

    def rows(self, *spec):
        return [{"recordId": self.SID, "action": act, "actorUserId": who} for act, who in spec]

    def test_create_and_delete_by_the_builder(self):
        ok, _ = gate.audit_ok(self.rows(("create", self.BUILDER), ("delete", self.BUILDER)), self.SID, self.BUILDER)
        self.assertTrue(ok)

    def test_missing_delete(self):
        ok, _ = gate.audit_ok(self.rows(("create", self.BUILDER)), self.SID, self.BUILDER)
        self.assertFalse(ok)

    def test_principal_as_actor_is_refused(self):
        ok, detail = gate.audit_ok(self.rows(("create", self.BUILDER), ("delete", self.PRINCIPAL)), self.SID, self.BUILDER)
        self.assertFalse(ok, detail)

    def test_other_sessions_ignored(self):
        rows = self.rows(("create", self.BUILDER), ("delete", self.BUILDER)) + [
            {"recordId": "s-other", "action": "create", "actorUserId": self.PRINCIPAL}]
        ok, _ = gate.audit_ok(rows, self.SID, self.BUILDER)
        self.assertTrue(ok)


class AuditLabel(unittest.TestCase):
    SID = "s-1"

    def rows(self, *labels):
        return [{"recordId": self.SID, "actorLabel": lb} for lb in labels]

    def test_builder_label_with_workshop(self):
        ok, _ = gate.audit_label_ok(self.rows("Xuân Tài (Nexus)", "Xuân Tài (Nexus)"), self.SID, "Nexus")
        self.assertTrue(ok)

    def test_missing_label_is_refused(self):
        ok, _ = gate.audit_label_ok(self.rows("Xuân Tài (Nexus)", None), self.SID)
        self.assertFalse(ok)

    def test_plain_name_without_workshop_is_refused(self):
        ok, _ = gate.audit_label_ok(self.rows("Xuân Tài"), self.SID)
        self.assertFalse(ok)

    def test_wrong_workshop_is_refused(self):
        ok, _ = gate.audit_label_ok(self.rows("Xuân Tài (Khác)"), self.SID, "Nexus")
        self.assertFalse(ok)

    def test_no_rows_is_refused(self):
        ok, _ = gate.audit_label_ok([], self.SID)
        self.assertFalse(ok)


class Loopback(unittest.TestCase):
    def test_only_this_machine(self):
        for base, want in (("http://localhost:8163/api/v1", True),
                           ("http://127.0.0.1:8163/api/v1", True),
                           ("http://[::1]:8090/api/v1", True),
                           ("https://nexus.example.com/api/v1", False),
                           ("http://localhost.evil.example/api/v1", False)):
            self.assertEqual(gate.is_loopback(base), want, base)


class SharedDev(unittest.TestCase):
    def test_refuses_only_the_shared_dev_port(self):
        for base, want in (("http://localhost:8090/api/v1", True),
                           ("http://127.0.0.1:8090/api/v1", True),
                           ("http://localhost:8163/api/v1", False),
                           ("http://localhost/api/v1", False)):
            self.assertEqual(gate.is_shared_dev(base), want, base)


class RouteList(unittest.TestCase):
    """--scan-get reads the route list `go test -run TestPreviewRouteScan_GetRoutes -args -routes-out=<f>` writes."""

    def test_only_protected_routes_are_scanned(self):
        text = "protected /api/v1/:entity\npublic /api/v1/auth/me\n\nprotected /api/v1/audit-log/:entity/:recordId\n"
        self.assertEqual(gate.read_routes(text), ["/:entity", "/audit-log/:entity/:recordId"])

    def test_params_get_real_values_and_the_rest_a_placeholder(self):
        path = gate.fill_params("/audit-log/:entity/:recordId", entity="crm_accounts", rec_id="rec-1", app="crm")
        self.assertEqual(path, "/audit-log/crm_accounts/rec-1")
        self.assertEqual(gate.fill_params("/:entity/:id/relationships/:relName", "crm_accounts", "rec-1", "crm"),
                         "/crm_accounts/rec-1/relationships/x")
        self.assertEqual(gate.fill_params("/apps/:name/nav", "crm_accounts", "rec-1", "crm"), "/apps/crm/nav")
        self.assertEqual(gate.fill_params("/admin/roles/:id", "crm_accounts", "rec-1", "crm"), "/admin/roles/rec-1")

    def test_a_top_level_resource_gets_one_real_id_of_its_own_kind(self):
        ids = {"reports": "rep-1", "dashboards": "dash-1"}
        self.assertEqual(gate.fill_params("/reports/:id/run", "crm_accounts", "rec-1", "crm", ids), "/reports/rep-1/run")
        self.assertEqual(gate.fill_params("/dashboards/:id/run", "crm_accounts", "rec-1", "crm", ids), "/dashboards/dash-1/run")
        # a data record route still gets the record id, and an unknown resource falls back to it
        self.assertEqual(gate.fill_params("/:entity/:id", "crm_accounts", "rec-1", "crm", ids), "/crm_accounts/rec-1")
        self.assertEqual(gate.fill_params("/approvals/:id", "crm_accounts", "rec-1", "crm", ids), "/approvals/rec-1")

    def test_first_id_reads_both_list_shapes_and_survives_empty(self):
        self.assertEqual(gate.first_id({"data": [{"id": "a"}]}, "reports"), "a")
        self.assertEqual(gate.first_id({"data": {"reports": [{"id": "b"}]}}, "reports"), "b")
        self.assertIsNone(gate.first_id({"data": {"approvals": []}}, "approvals"))
        self.assertIsNone(gate.first_id({}, "reports"))

    def test_a_route_with_a_required_query_gets_one(self):
        self.assertIn("q=", gate.fill_params("/search", "crm_accounts", "r", "crm"))


class ScanGet(unittest.TestCase):
    """scan_get against a real local HTTP server: only a 5xx is a failure (403/404/422 are the role speaking);
    a redirect is never followed (SSO start would leave the machine)."""

    @classmethod
    def setUpClass(cls):
        import http.server
        import threading

        seen = []

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append((self.path, self.headers.get("Authorization")))
                code = {"/ok": 200, "/forbidden": 403, "/gone": 404, "/boom": 500, "/bad-gateway": 502, "/jump": 302}.get(self.path, 200)
                self.send_response(code)
                if code == 302:
                    self.send_header("Location", "http://127.0.0.1:1/never")
                self.end_headers()
                self.wfile.write(b"{}")

            def log_message(self, *a):
                pass

        cls.seen = seen
        cls.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        cls.base = f"http://127.0.0.1:{cls.srv.server_address[1]}"
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def test_status_classes_are_counted_for_the_report(self):
        seen = {}
        gate.scan_get(self.base, "PV", ["/ok", "/forbidden", "/gone", "/boom"], seen)
        self.assertEqual(seen, {"2xx": 1, "4xx": 2, "5xx": 1})

    def test_only_5xx_fails_and_every_request_carries_the_token(self):
        bad = gate.scan_get(self.base, "PV", ["/ok", "/forbidden", "/gone", "/boom", "/bad-gateway", "/jump"])
        self.assertEqual(bad, [("/boom", 500), ("/bad-gateway", 502)])
        self.assertTrue(all(auth == "Bearer PV" for _, auth in self.seen))

    def test_an_unreachable_server_is_an_environment_error_not_a_pass(self):
        with self.assertRaises(gate.EnvError):
            gate.scan_get("http://127.0.0.1:1", "PV", ["/ok"])


class PrincipalInDatabase(unittest.TestCase):
    """The GET-writes gate: after the scan no row of the tenant may carry the synthetic principal, in ANY
    string/JSON column of ANY tenant table (a hand-listed set of columns misses updated_by, owner_id...)."""

    TENANT, PRINCIPAL = "11111111-1111-4111-8111-111111111111", "99999999-9999-5999-8999-999999999999"
    COLS = [
        ("sys_views", "id", "char"), ("sys_views", "updated_by", "char"),
        ("sys_preview_sessions", "principal_id", "char"),
        ("obj_crm_accounts", "owner_id", "char"), ("obj_crm_accounts", "data", "json"),
        ("audit_log", "actor_user_id", "char"),
    ]

    def fake(self, dirty):
        """A run_sql double: answers the column listing, then fakes COUNT(*) per (table, column) from `dirty`."""
        import re
        self.sql = []

        def run(sql):
            self.sql.append(sql)
            if "information_schema.columns" in sql:
                return [list(c) for c in self.COLS]
            rows = []
            for t, c in re.findall(r"SELECT '(\w+)','(\w+)',COUNT\(\*\)", sql):
                rows.append([t, c, str(dirty.get((t, c), 0))])
            return rows
        return run

    def test_a_dirty_updated_by_is_reported_with_table_and_column(self):
        hits = gate.principal_hits(self.fake({("sys_views", "updated_by"): 1}), self.TENANT, self.PRINCIPAL)
        self.assertEqual(hits, [("sys_views", "updated_by", 1)])

    def test_audit_log_is_not_exempt_and_the_session_table_principal_column_is(self):
        hits = gate.principal_hits(self.fake({("audit_log", "actor_user_id"): 2, ("sys_preview_sessions", "principal_id"): 5}),
                                   self.TENANT, self.PRINCIPAL)
        self.assertEqual(hits, [("audit_log", "actor_user_id", 2)])

    def test_clean_database_reports_nothing(self):
        self.assertEqual(gate.principal_hits(self.fake({}), self.TENANT, self.PRINCIPAL), [])

    def test_sql_is_tenant_scoped_json_aware_and_the_exempt_column_is_never_queried(self):
        gate.principal_hits(self.fake({}), self.TENANT, self.PRINCIPAL)
        counts = "\n".join(s for s in self.sql if "COUNT(*)" in s)
        self.assertIn(f"tenant_id = '{self.TENANT}'", counts)
        self.assertIn(f"`owner_id` = '{self.PRINCIPAL}'", counts)
        self.assertIn(f"JSON_SEARCH(`data`, 'one', '{self.PRINCIPAL}') IS NOT NULL", counts)
        self.assertNotIn("principal_id", counts)

    def test_untrusted_values_never_reach_the_sql(self):
        for tenant, principal in (("x' OR '1'='1", self.PRINCIPAL), (self.TENANT, "p'; DROP TABLE users;--")):
            with self.assertRaises(ValueError):
                gate.principal_hits(self.fake({}), tenant, principal)

    def test_odd_table_or_column_names_are_refused(self):
        bad = self.fake({})

        def run(sql):
            return [["users`; DROP", "id", "char"]] if "information_schema.columns" in sql else bad(sql)

        with self.assertRaises(ValueError):
            gate.principal_hits(run, self.TENANT, self.PRINCIPAL)


class DsnIsTheServersDb(unittest.TestCase):
    """A clean count only means something if the DSN is the DB the scanned server writes."""

    TENANT, SID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"

    def test_anchor_counts_the_open_session_scoped_by_tenant_and_id(self):
        seen = []

        def run(sql):
            seen.append(sql)
            return [["1"]]
        self.assertEqual(gate.session_anchor(run, self.TENANT, self.SID), 1)
        self.assertIn(f"tenant_id = '{self.TENANT}'", seen[0])
        self.assertIn(f"id = '{self.SID}'", seen[0])
        self.assertIn("ended_at IS NULL", seen[0])

    def test_session_missing_from_the_dsn_db_counts_zero(self):
        self.assertEqual(gate.session_anchor(lambda sql: [["0"]], self.TENANT, self.SID), 0)

    def test_anchor_refuses_unvalidated_values(self):
        with self.assertRaises(ValueError):
            gate.session_anchor(lambda sql: [["1"]], self.TENANT, "x' OR '1'='1")

    def test_a_db_without_tenant_tables_is_an_env_error_not_a_pass(self):
        with self.assertRaises(gate.EnvError):
            gate.principal_hits(lambda sql: [], self.TENANT, "99999999-9999-5999-8999-999999999999")

    def test_stats_report_the_surface_scanned(self):
        stats = {}
        cols = [["a", "x", "char"], ["a", "y", "json"], ["b", "z", "varchar"]]
        gate.principal_hits(lambda sql: cols if "information_schema.columns" in sql else [], self.TENANT,
                            "99999999-9999-5999-8999-999999999999", stats)
        self.assertEqual(stats, {"tables": 2, "columns": 3})


class MysqlDsn(unittest.TestCase):
    def test_go_style_dsn_parts(self):
        d = gate.parse_dsn("root:s3cret@tcp(127.0.0.1:3307)/nexus_wt_x?charset=utf8mb4&parseTime=True")
        self.assertEqual((d["user"], d["password"], d["host"], d["port"], d["db"]), ("root", "s3cret", "127.0.0.1", "3307", "nexus_wt_x"))

    def test_empty_password_and_default_port(self):
        d = gate.parse_dsn("root:@tcp(localhost)/nexus_wt_x")
        self.assertEqual((d["password"], d["port"]), ("", "3306"))

    def test_the_shared_dev_database_is_refused(self):
        with self.assertRaises(ValueError):
            gate.parse_dsn("root:@tcp(127.0.0.1:3306)/nexus_platform")

    def test_garbage_is_refused(self):
        with self.assertRaises(ValueError):
            gate.parse_dsn("not a dsn")


if __name__ == "__main__":
    unittest.main()
