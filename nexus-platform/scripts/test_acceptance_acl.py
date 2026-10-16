"""Unit checks for the pure decisions inside scripts/acceptance-acl.py.

Run: python3 scripts/test_acceptance_acl.py   (from nexus-platform/)

The end-to-end proof of the gate is running it against a live server; these cases pin the
decisions a reader cannot see from the output: WHETHER a 403 may be re-judged by another role
(keep_judge — the security-relevant one), which role the gate then picks, and when a 200 counts
as "readable".
"""

import importlib.util
import os
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-acl.py")
_spec = importlib.util.spec_from_file_location("acceptance_acl", _path)
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)


def role(name, *grants):
    return {"id": "id-" + name, "name": name,
            "actions": [{"entityName": e, "action": a, "accessLevel": lvl} for e, a, lvl in grants]}


class ViewCandidates(unittest.TestCase):
    def test_no_role_grants_view_means_no_candidate(self):
        roles = [
            role("crm_sales_rep", ("crm_accounts", "view", "all")),
            role("writer_only", ("tk_tickets", "create", "all"), ("tk_tickets", "edit", "all")),
            role("revoked", ("tk_tickets", "view", "none")),
        ]
        self.assertEqual(gate.view_candidates(roles, "tk_tickets"), [])

    def test_wildcard_grant_is_not_a_candidate(self):
        # A '*' role sees every table; picking it as "the third party" scores green through a role
        # that is nobody's staff role for this table (#98 L3). --role can still name it explicitly.
        roles = [role("super", ("*", "view", "all")),
                 role("tk_agent", ("tk_tickets", "view", "all"))]
        self.assertEqual([r["name"] for r in gate.view_candidates(roles, "tk_tickets")], ["tk_agent"])
        self.assertEqual(gate.view_candidates([role("super", ("*", "view", "all"))], "tk_tickets"), [])

    def test_widest_view_then_least_power_then_name(self):
        roles = [
            role("tk_manager", ("tk_tickets", "view", "all"), ("tk_tickets", "create", "all"),
                 ("tk_tickets", "edit", "all"), ("tk_tickets", "delete", "all")),
            role("owner_only", ("tk_tickets", "view", "owner")),
            role("tk_agent", ("tk_tickets", "view", "all"), ("tk_tickets", "create", "all"),
                 ("tk_tickets", "edit", "all")),
            role("team_view", ("tk_tickets", "view", "team")),
            role("app_ticketing_staff", ("tk_tickets", "view", "all"), ("tk_tickets", "create", "owner"),
                 ("tk_tickets", "edit", "owner"), ("tk_customers", "view", "all")),
        ]
        got = [r["name"] for r in gate.view_candidates(roles, "tk_tickets")]
        self.assertEqual(got, ["app_ticketing_staff", "tk_agent", "tk_manager", "team_view", "owner_only"])


class GrantsOn(unittest.TestCase):
    # A 403 only switches the judge when the default account's role has NOTHING on the entity.
    # A role that still holds create/edit but lost `view` is a regression of that very role.
    def test_cases(self):
        rep = role("crm_sales_rep", ("crm_quotes", "create", "all"), ("crm_quotes", "edit", "owner"),
                   ("crm_accounts", "view", "all"))
        self.assertEqual(len(gate.grants_on(rep, "crm_quotes")), 2)
        self.assertEqual(gate.grants_on(rep, "tk_tickets"), [])
        self.assertEqual(len(gate.grants_on(role("w", ("*", "create", "all")), "tk_tickets")), 1)
        self.assertEqual(gate.grants_on(None, "tk_tickets"), [])  # the account holds no role at all


class KeepJudge(unittest.TestCase):
    """The ONE security decision of the gate: may a 403 on an expected entity be re-judged by a
    holder of another role? Every case but one must keep the default staff account as the judge
    (entity stays red); a wrong "switch" scores the gate green on a real regression."""

    # northwind today: the sales app shares crm_accounts/crm_quotes with crm; crm_new is a CRM table a
    # slice added to the crm nav but forgot to grant; `candidates` sits on no nav at all — and so
    # does a table created with POST /admin/entities {appId} (grants app_crm_* WITHOUT a nav item).
    APPS = {
        "crm": {"crm_accounts", "crm_contacts", "crm_quotes", "crm_new"},
        "sales": {"products", "crm_accounts", "crm_quotes"},
        "ticketing": {"tk_tickets", "tk_customers"},
    }
    REP = role("crm_sales_rep", ("crm_accounts", "view", "all"), ("crm_contacts", "view", "all"),
               ("crm_quotes", "create", "all"), ("crm_quotes", "edit", "owner"))
    # PUT /admin/roles/:id REPLACES the grant set: its only grant on crm_accounts is gone.
    REP_LOST_ACCOUNTS = role("crm_sales_rep", ("crm_contacts", "view", "all"),
                             ("crm_quotes", "create", "all"))

    def test_cases(self):
        cases = [
            # (own role, entity, staff code, apps) -> None = switch the judge, else why not
            ((self.REP, "tk_tickets", 403, self.APPS), None),  # a sales rep is not a ticket agent
            ((self.REP, "candidates", 403, self.APPS), "off-nav"),  # on no nav: nothing says whose it is
            ((self.REP, "tk_tickets", 200, self.APPS), "not-403"),  # 200-empty is the rep's own empty screen
            ((None, "tk_tickets", 403, self.APPS), "no-role"),  # account lost its role_id
            ((role("empty"), "tk_tickets", 403, self.APPS), "no-role"),  # role emptied by a REPLACE
            ((self.REP, "crm_quotes", 403, self.APPS), "own-grant"),  # keeps create/edit, lost view
            ((role("w", ("orphan_x", "create", "all")), "orphan_x", 403, self.APPS), "own-grant"),
            ((role("super", ("*", "create", "all")), "tk_tickets", 403, self.APPS), "own-grant"),
            ((self.REP_LOST_ACCOUNTS, "crm_accounts", 403, self.APPS), "same-app:crm,sales"),
            ((self.REP, "crm_new", 403, self.APPS), "same-app:crm"),  # new CRM table, grant forgotten
            ((self.REP, "products", 403, self.APPS), "same-app:sales"),  # declare --role to judge it
            ((self.REP, "tk_tickets", 403, {}), "off-nav"),  # empty catalog must not fail open
            ((self.REP, "tk_tickets", 403, {"crm": self.APPS["crm"]}), "off-nav"),  # short app map, same
        ]
        for args, want in cases:
            with self.subTest(role=(args[0] or {}).get("name"), entity=args[1], code=args[2]):
                self.assertEqual(gate.keep_judge(*args), want)


class Readable(unittest.TestCase):
    def test_cases(self):
        cases = [
            # (admin code, admin total, staff code, staff total) -> (readable, blind)
            ((200, 26, 200, 26), (True, False)),
            ((200, 27, 200, 3), (True, False)),     # owner scope: fewer rows is still a real screen
            ((200, 26, 200, 0), (False, True)),     # the empty-screen failure
            ((200, 0, 200, 0), (True, False)),      # empty for everybody is not a permission bug
            ((200, 26, 403, "FORBIDDEN"), (False, False)),
        ]
        for args, want in cases:
            with self.subTest(args=args):
                self.assertEqual(gate.readable(*args), want)


class HolderAccounts(unittest.TestCase):
    """#98: one FIXED acceptance account per role, reused and disabled between runs — never a new
    row per run (soft-delete + a unique (tenant, email) that counts deleted rows left one dead
    `users` row per role per run)."""

    def test_email_is_fixed_per_role(self):
        a, b = role("tk_agent"), role("app_ticketing_staff")
        self.assertEqual(gate.holder_emails(a), gate.holder_emails(dict(a)))  # same role → same login
        self.assertNotEqual(gate.holder_emails(a)[0], gate.holder_emails(b)[0])
        emails = gate.holder_emails(a)
        self.assertEqual(emails[0], "nghiemthu-id-tk_agent@acceptance.invalid")
        self.assertEqual(len(set(emails)), len(emails))  # fallback slots when a slot is burnt by a soft delete
        self.assertTrue(all(e.endswith("@acceptance.invalid") for e in emails))

    def test_live_holders_are_acceptance_accounts_not_yet_parked(self):
        # Parked = disabled AND no role. A disabled account that still holds a role pins it: app
        # delete keeps a seeded role anyone is assigned to (AppService.cleanupSeededRoles), so every
        # app that went through the gate left its app_<name>_staff behind.
        users = [
            {"id": "a", "email": "nghiemthu-r1@acceptance.invalid", "status": "active", "roleId": "r1"},
            {"id": "b", "email": "nghiemthu-9f3c@acceptance.invalid", "status": "active"},  # killed old-format run
            {"id": "c", "email": "nghiemthu-r2@acceptance.invalid", "status": "disabled"},
            {"id": "d", "email": "test@example.com", "status": "active", "roleId": "r1"},
            {"id": "e", "email": "someone@acceptance.invalid.example.com", "status": "active"},
            {"id": "f", "email": "nghiemthu-r3@acceptance.invalid", "status": "disabled", "roleId": "r3"},
            {"id": "g", "email": "nghiemthu-r4@acceptance.invalid", "status": "disabled", "roleId": ""},
        ]
        self.assertEqual(gate.live_holders(users), ["a", "b", "f"])


class DisableLiveHolders(unittest.TestCase):
    """The cleanup must never stop half-way and must SAY what is still live: it used to print a
    warning and exit 0, and a connection error inside the DELETE loop sys.exit(2)-ed past the ids
    that were left."""

    def setUp(self):
        self._real = gate._req
        self.calls = []

    def tearDown(self):
        gate._req = self._real

    def fake(self, list_code, users, put_codes):
        def _req(method, url, token=None, body=None, exit_on_error=True):
            self.assertFalse(exit_on_error, "cleanup must not exit mid-way")
            self.calls.append((method, url.rsplit("/", 1)[-1], body))
            if method == "GET":
                return list_code, ({"data": {"users": users}} if list_code == 200 else {})
            return put_codes.get(url.rsplit("/", 1)[-1], 200), {}
        gate._req = _req

    USERS = [
        {"id": "a", "email": "nghiemthu-r1@acceptance.invalid", "status": "active", "roleId": "r1"},
        {"id": "b", "email": "nghiemthu-r2@acceptance.invalid", "status": "active", "roleId": "r2"},
        {"id": "c", "email": "nghiemthu-r3@acceptance.invalid", "status": "disabled"},
        {"id": "d", "email": "test@example.com", "status": "active", "roleId": "r1"},
        {"id": "e", "email": "nghiemthu-r5@acceptance.invalid", "status": "disabled", "roleId": "r5"},
    ]
    # Disabled AND role cleared: a parked account pins no role, so deleting the app whose seeded
    # role the gate judged with can still remove that role.
    PARK = {"status": "disabled", "roleId": ""}

    def test_parks_every_live_holder(self):
        self.fake(200, self.USERS, {})
        self.assertEqual(gate.disable_live_holders("http://x", "tok"), [])
        self.assertEqual([c for c in self.calls if c[0] == "PUT"],
                         [("PUT", "a", self.PARK), ("PUT", "b", self.PARK), ("PUT", "e", self.PARK)])

    def test_a_failed_disable_is_reported_and_the_rest_still_run(self):
        self.fake(200, self.USERS, {"a": 0})  # 0 = connection error, as _req reports it
        self.assertEqual(gate.disable_live_holders("http://x", "tok"), ["a"])
        self.assertIn(("PUT", "b", self.PARK), self.calls)

    def test_unlistable_falls_back_to_known_ids_and_says_so(self):
        self.fake(500, [], {})
        left = gate.disable_live_holders("http://x", "tok", ["a", "b"])
        self.assertEqual([c[1:] for c in self.calls if c[0] == "PUT"], [("a", self.PARK), ("b", self.PARK)])
        self.assertEqual(len(left), 1)
        self.assertIn("/admin/users", left[0])


class EnableHolder(unittest.TestCase):
    """The other half of parking: the enabling PUT must give a parked account (roleId cleared)
    its role back, or the gate would judge with a role-less account."""

    def setUp(self):
        self._req, self._login = gate._req, gate.login
        self.puts = []

    def tearDown(self):
        gate._req, gate.login = self._req, self._login

    def test_parked_account_gets_its_role_back(self):
        r = role("app_x_staff")
        parked = {"id": "u1", "email": gate.holder_emails(r)[0], "status": "disabled", "isAdmin": False}

        def _req(method, url, token=None, body=None, exit_on_error=True):
            if method == "GET":
                return 200, {"data": {"users": [parked]}}
            self.puts.append(body)
            return 200, {"data": {"user": dict(parked, status="active", roleId=body["roleId"])}}
        gate._req = _req
        gate.login = lambda base, tenant, email, password: ("tok-" + email, {})

        class A:
            base, tenant = "http://x", "t"
        holders = []
        self.assertEqual(gate._enable_holder(A, "adm", r, holders), "tok-" + parked["email"])
        self.assertEqual(holders, ["u1"])
        self.assertEqual(len(self.puts), 1)
        self.assertEqual((self.puts[0]["status"], self.puts[0]["roleId"]), ("active", r["id"]))
        self.assertNotIn("isAdmin", self.puts[0])


class Guards(unittest.TestCase):
    def test_loopback(self):
        for base, want in [
            ("http://localhost:8090/api/v1", True),
            ("http://127.0.0.1:8191/api/v1", True),
            ("http://[::1]:8090/api/v1", True),
            ("https://nexus.example.com/api/v1", False),
            ("http://10.0.0.5:8090/api/v1", False),
            ("http://127.evil.example/api/v1", False),  # a NAME starting with 127. is not loopback
        ]:
            with self.subTest(base=base):
                self.assertEqual(gate.is_loopback(base), want)

    def test_sigterm_and_sighup_run_finally(self):
        # They used to kill the gate without running `finally` (exit 143/129, accounts left live).
        import signal
        for sig in (signal.SIGTERM, signal.SIGHUP):
            with self.subTest(sig=sig):
                ran = []
                with self.assertRaises(SystemExit) as cm:
                    try:
                        gate._exit_on_signal(sig, None)
                    finally:
                        ran.append(True)
                self.assertEqual(cm.exception.code, 128 + sig)
                self.assertEqual(ran, [True])


class AdminToken(unittest.TestCase):
    """--admin-token-env (specs/062-customer-workspace L1b T029a; specs/065 D-17 / FR-040 reuse it).

    The admin side of the gate can be a builder's workspace-session token read from an environment
    variable — the token never rides argv (visible to every `ps`) and is never printed. With it the
    gate does NOT log in for the admin side; `--admin` together with it is a contradiction (exit 2),
    as is an empty/missing variable.
    """

    SECRET = "ws.token.value-that-must-never-be-printed"

    def run_cli(self, *args, env=None):
        import subprocess
        import sys
        e = {k: v for k, v in os.environ.items() if k not in ("NEXUS_WS_TOKEN", "NEXUS_ADMIN_PASS")}
        e.update(env or {})
        # Port 9 (discard) on loopback: nothing listens, so a call that DOES go out fails fast and
        # the exit code tells us the gate got past argument validation.
        return subprocess.run([sys.executable, _path, "--base", "http://127.0.0.1:9/api/v1", *args],
                              env=e, capture_output=True, text=True, timeout=60)

    def test_resolve_uses_token_without_login(self):
        kind, value = gate.resolve_admin(None, "NEXUS_WS_TOKEN", {"NEXUS_WS_TOKEN": self.SECRET})
        self.assertEqual((kind, value), ("token", self.SECRET))

    def test_resolve_without_flag_logs_in_as_the_default_admin(self):
        self.assertEqual(gate.resolve_admin(None, "", {}), ("login", "admin@example.com"))
        self.assertEqual(gate.resolve_admin("boss@x.vn", "", {}), ("login", "boss@x.vn"))

    def test_resolve_refuses_admin_together_with_token_env(self):
        # An explicit --admin (even the default's own value) next to --admin-token-env is ambiguous.
        for explicit in ("admin@example.com", "boss@x.vn"):
            with self.subTest(admin=explicit), self.assertRaises(SystemExit) as cm:
                gate.resolve_admin(explicit, "NEXUS_WS_TOKEN", {"NEXUS_WS_TOKEN": self.SECRET})
            self.assertEqual(cm.exception.code, 2)

    def test_resolve_refuses_empty_or_missing_env(self):
        for env in ({}, {"NEXUS_WS_TOKEN": ""}, {"NEXUS_WS_TOKEN": "   "}):
            with self.subTest(env=env), self.assertRaises(SystemExit) as cm:
                gate.resolve_admin(None, "NEXUS_WS_TOKEN", env)
            self.assertEqual(cm.exception.code, 2)

    def test_cli_exit_2_with_a_clear_message_and_no_token_in_output(self):
        for args, env in [
            (["--admin-token-env", "NEXUS_WS_TOKEN", "--admin", "admin@example.com"], {"NEXUS_WS_TOKEN": self.SECRET}),
            (["--admin-token-env", "NEXUS_WS_TOKEN"], {}),  # variable not set
            (["--admin-token-env", "NEXUS_WS_TOKEN"], {"NEXUS_WS_TOKEN": ""}),
        ]:
            with self.subTest(args=args, env=bool(env)):
                r = self.run_cli(*args, env=env)
                self.assertEqual(r.returncode, 2, r.stderr)
                self.assertIn("NEXUS_WS_TOKEN", r.stderr)
                self.assertNotIn(self.SECRET, r.stdout + r.stderr)

    def test_cli_with_token_env_never_prints_the_token_and_sends_no_admin_login(self):
        # Server down: the staff login (or the first call) fails with exit 2 — and the token the run
        # was given appears nowhere in its output.
        r = self.run_cli("--admin-token-env", "NEXUS_WS_TOKEN", "--staff", "nv@x.vn",
                         env={"NEXUS_WS_TOKEN": self.SECRET, "NEXUS_STAFF_PASS": "x"})
        self.assertEqual(r.returncode, 2, r.stderr)
        self.assertNotIn(self.SECRET, r.stdout + r.stderr)


if __name__ == "__main__":
    unittest.main()
