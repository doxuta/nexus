"""Unit checks for the rep's book in scripts/seed-demo.py (#30 item 7).

Run: python3 scripts/test_seed_demo.py   (from nexus-platform/)

The CRM pack's sample rows belong to the installer, and the rep's role sees its own rows on every CRM table but
accounts and products, so the seed hands the rep one account's records and two leads. These cases pin which rows:
one account and everything filed under it, never another account's. The end-to-end proof is the demo CI job,
which runs acceptance-acl.py as the rep on every seeded CRM table.
"""

import importlib.util
import os
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "seed-demo.py")
_spec = importlib.util.spec_from_file_location("seed_demo", _path)
seed = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seed)


def row(id, **data):
    return {"id": id, "data": data}


def tenant():
    """Two accounts with a record of every kind each, as GET /<entity> returns them."""
    return {
        "crm_accounts": [row("a1", name=seed.REP_ACCOUNT), row("a2", name="Acme Corp")],
        "crm_contacts": [row("c1", account="a1"), row("c2", account="a2")],
        "crm_opportunities": [row("o1", account="a1"), row("o2", account="a2")],
        "crm_activities": [row("t1", account="a1"), row("t2", account="a2")],
        "crm_quotes": [row("q1", account="a1"), row("q2", account="a2")],
        "crm_quote_lines": [row("l1", quote="q1"), row("l2", quote="q1"), row("l3", quote="q2")],
        "crm_contracts": [row("k1", account="a1"), row("k2", account="a2")],
        "crm_leads": [row("d1", full_name=seed.REP_LEADS[0]), row("d2", full_name=seed.REP_LEADS[1]),
                      row("d3", full_name="Hank Wilson")],
    }


class RepBook(unittest.TestCase):
    def test_one_account_and_what_is_filed_under_it(self):
        self.assertEqual(seed.rep_book(tenant()), {
            "crm_accounts": {"a1"},
            "crm_contacts": {"c1"},
            "crm_opportunities": {"o1"},
            "crm_activities": {"t1"},
            "crm_quotes": {"q1"},
            "crm_quote_lines": {"l1", "l2"},
            "crm_contracts": {"k1"},
            "crm_leads": {"d1", "d2"},
        })

    def test_covers_every_table_the_rep_reads_by_owner_or_team(self):
        # A table missing here is a table the rep opens empty on a fresh demo.
        self.assertEqual(set(seed.rep_book(tenant())), set(seed.BOOK))

    def test_a_complete_book_has_no_gap(self):
        rows = tenant()
        self.assertEqual(seed.book_gaps(rows, seed.rep_book(rows)), [])

    def test_without_the_account_the_book_is_empty_so_the_seed_fails(self):
        rows = tenant()
        rows["crm_accounts"] = [row("a2", name="Acme Corp")]
        self.assertEqual(seed.book_gaps(rows, seed.rep_book(rows)),
                         [e for e in seed.BOOK if e != "crm_leads"])

    def test_a_renamed_lead_fails_the_seed_though_its_table_is_not_empty(self):
        rows = tenant()
        rows["crm_leads"][1] = row("d2", full_name="Frank N.")
        self.assertEqual(seed.book_gaps(rows, seed.rep_book(rows)), [f"lead {seed.REP_LEADS[1]}"])


if __name__ == "__main__":
    unittest.main()
