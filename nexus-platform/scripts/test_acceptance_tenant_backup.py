"""Unit checks for the pure decisions inside scripts/acceptance-tenant-backup.py.

Run: python3 scripts/test_acceptance_tenant_backup.py   (from nexus-platform/)

The end-to-end proof is running the script against a live server; these pin what a reader cannot see
from its output: when a write is refused, what counts as a row total, and that the 403 table covers
every route of the feature (a route added later but missing here would be un-gated by this script).
"""

import importlib.util
import os
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-tenant-backup.py")
_spec = importlib.util.spec_from_file_location("acceptance_tenant_backup", _path)
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)


class Loopback(unittest.TestCase):
    def test_only_this_machine_may_be_written(self):
        for base, want in [
            ("http://localhost:8458/api/v1", True),
            ("http://127.0.0.1:8458/api/v1", True),
            ("http://[::1]:8458/api/v1", True),
            ("http://127.0.0.1.evil.example/api/v1", False),
            ("https://nexus.example.com/api/v1", False),
            ("http://10.0.0.5/api/v1", False),
        ]:
            self.assertEqual(gate.is_loopback(base), want, base)


class TotalOf(unittest.TestCase):
    def test_meta_total_wins_then_list_length(self):
        self.assertEqual(gate.total_of({"meta": {"total": 7}, "data": [1]}), 7)
        self.assertEqual(gate.total_of({"pagination": {"total": 3, "limit": 1}, "data": [1]}), 3)
        self.assertEqual(gate.total_of({"data": [1, 2, 3]}), 3)
        self.assertEqual(gate.total_of({"error": "x"}), -1)


class ForbiddenTable(unittest.TestCase):
    def test_covers_every_route_of_the_feature(self):
        routes = {(m, p.split("/admin/tenants/T")[1].replace("00000000-0000-4000-8000-000000000000", ":id"))
                  for m, p, _ in gate.forbidden_table("T")}
        self.assertEqual(routes, {
            ("GET", "/backups"), ("POST", "/backups"), ("GET", "/backups/:id"), ("GET", "/backups/:id/file"),
            ("POST", "/maintenance"), ("DELETE", "/maintenance"), ("POST", "/restores"),
        })

    def test_includes_the_destructive_restore_body(self):
        bodies = [b for m, p, b in gate.forbidden_table("T") if p.endswith("/restores")]
        self.assertTrue(any(b and b.get("dryRun") is False for b in bodies))


if __name__ == "__main__":
    unittest.main()
