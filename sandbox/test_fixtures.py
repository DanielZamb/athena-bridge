from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest

from fixtures import generate, SCHEMAS, TABLE_COMMENTS, write
from oracle import expected


class FixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tables = generate()
        cls.result = expected(cls.tables)

    def month_row(self, account, user, month, result=None):
        result = self.result if result is None else result
        return next(row for row in result["per_user_month"]
                    if (row["account_no"], row["user_id"], row["month"]) == (account, user, month))

    def periods(self, result=None):
        result = self.result if result is None else result
        return {(row["account_no"], row["user_id"]): row for row in result["per_user_period"]}

    def runs(self, account, user):
        return [(row["start_month"], row["end_month"], row["month_count"], row["qualifies"])
                for row in self.result["maximal_runs"] if (row["account_no"], row["user_id"]) == (account, user)]

    def test_deterministic_relations_match_metadata_and_literal_counts(self):
        self.assertEqual(generate(), self.tables)
        self.assertEqual(self.result["relation_counts"], {
            "ac_us_accounts_groups": 18, "ac_us_users": 1115, "ac_us_sessions": 4263,
            "ac_us_client_events": 14298, "fixture_contract": 16,
        })
        self.assertEqual(set(SCHEMAS), set(TABLE_COMMENTS))
        for name, rows in self.tables.items():
            fields = {field for field, _, _ in SCHEMAS[name]}
            self.assertTrue(all(set(row) == fields for row in rows), name)
        keys = {
            "ac_us_accounts_groups": ("account_no",), "ac_us_users": ("account_no", "user_id"),
            "ac_us_sessions": ("account_no", "user_id", "session_id"), "ac_us_client_events": ("event_id",),
        }
        for name, fields in keys.items():
            self.assertEqual(len({tuple(row[field] for field in fields) for row in self.tables[name]}), len(self.tables[name]), name)
        self.assertEqual(len({row["session_id"] for row in self.tables["ac_us_sessions"]}), 4262)

    def test_written_ndjson_is_bounded_and_preserves_exact_cells(self):
        with tempfile.TemporaryDirectory(prefix="athena-fixtures-") as directory:
            self.assertEqual(Path(directory).stat().st_mode & 0o777, 0o700)
            write(directory)
            paths = sorted(Path(directory).glob("*.jsonl"))
            self.assertEqual(len(paths), 5)
            self.assertLess(sum(path.stat().st_size for path in paths), 8 * 1024 * 1024)
            for path in paths:
                self.assertEqual([json.loads(line) for line in path.read_text().splitlines()], self.tables[path.stem])
        events = self.tables["ac_us_client_events"]
        self.assertEqual(events[0]["event_id"], 9007199254740993)
        self.assertEqual(events[2]["latency_ms"], "1.2300")
        self.assertIsNone(events[0]["latency_ms"])

    def test_monthly_and_pooled_counts_are_literal(self):
        self.assertEqual(self.result["monthly"], [
            {"month": "2026-04-01", "active_users": 857, "regular_users": 490},
            {"month": "2026-05-01", "active_users": 608, "regular_users": 244},
            {"month": "2026-06-01", "active_users": 487, "regular_users": 245},
            {"month": "2026-07-01", "active_users": 487, "regular_users": 364},
            {"month": "2026-08-01", "active_users": 607, "regular_users": 484},
            {"month": "2026-09-01", "active_users": 369, "regular_users": 246},
        ])
        self.assertEqual(self.result["whole_period"], {"active_users": 980, "regular_users": 612})
        self.assertEqual(self.result["consecutive_user_count"], 488)
        core = [login for login in self.result["consecutive_logins"] if not login.startswith("S")]
        self.assertEqual(core, ["A100/disjoint", "A100/u01", "A100/u02", "A101/u06", "A101/u07", "A101/u08", "A101/u09", "A101/u10"])
        scale = [row for row in self.result["consecutive_login_rows"] if row["account_no"].startswith("S")]
        self.assertEqual(len(scale), 480)
        self.assertEqual({int(row["user_id"].split("-")[1]) % 9 for row in scale}, {0, 1, 5, 8})
        self.assertEqual(len({(row["account_no"], row["user_id"]) for row in scale}), 480)
        scale_logins = [f"S{account:03d}/member-{block + offset:03d}"
                        for account in range(12) for block in range(0, 90, 9) for offset in (0, 1, 5, 8)]
        self.assertEqual(self.result["consecutive_logins"], core + scale_logins)

    def test_original_small_cohort_retains_its_answers(self):
        identities = {
            ("A100", user) for user in ("u01", "u02", "u03", "u04", "u05", "shared", "internal-team", "internal-staff", "internal-examplecorp")
        } | {
            ("A101", user) for user in ("u06", "u07", "u08", "u09", "u10", "shared", "inactive")
        } | {("I100", "internal-account"), ("I101", "internal-mixed"), ("T-100", "trial")}
        tables = {name: [row for row in rows if "user_id" not in row or (row["account_no"], row["user_id"]) in identities]
                  for name, rows in self.tables.items()}
        result = expected(tables)
        self.assertEqual([(row["active_users"], row["regular_users"]) for row in result["monthly"]],
                         [(9, 5), (7, 3), (7, 5), (6, 3), (6, 3), (7, 4)])
        self.assertEqual(result["whole_period"], {"active_users": 12, "regular_users": 7})
        self.assertEqual(result["consecutive_logins"], ["A100/u01", "A100/u02", "A101/u06", "A101/u07", "A101/u08", "A101/u09", "A101/u10"])

    def test_named_scale_patterns_have_distinct_literal_pooled_answers(self):
        periods = self.periods()
        for member, total, ai, regular in (
            (0, 6, 6, True), (1, 6, 3, False), (2, 6, 0, False), (4, 1, 1, True),
            (5, 4, 4, True), (6, 3, 3, True), (7, 2, 0, False), (8, 6, 6, True),
        ):
            self.assertEqual(periods[("S000", f"member-{member:03d}")], {
                "account_no": "S000", "user_id": f"member-{member:03d}", "total_sessions": total,
                "ai_sessions": ai, "regular": regular,
            })
        self.assertNotIn(("S000", "member-003"), periods)
        self.assertNotIn(("A101", "inactive"), periods)

    def test_half_is_not_regular_and_one_of_one_is_regular(self):
        self.assertEqual(self.month_row("A100", "u02", "2026-04-01"), {
            "account_no": "A100", "user_id": "u02", "month": "2026-04-01",
            "total_sessions": 2, "ai_sessions": 1, "regular": False,
        })
        self.assertEqual(self.month_row("A100", "u04", "2026-04-01")["regular"], True)
        self.assertEqual(self.periods()[("A100", "u02")]["ai_sessions"], 6)
        self.assertEqual(self.runs("A100", "u02"), [("2026-04-01", "2026-09-01", 6, True)])
        self.assertEqual(self.runs("A100", "u04"), [("2026-04-01", "2026-04-01", 1, False)])

    def test_maximal_runs_break_at_gaps_and_do_not_duplicate_logins(self):
        self.assertEqual(self.runs("A100", "disjoint"), [
            ("2026-04-01", "2026-05-01", 2, True), ("2026-07-01", "2026-09-01", 3, True),
        ])
        self.assertEqual(self.runs("A100", "u05"), [
            ("2026-04-01", "2026-04-01", 1, False), ("2026-06-01", "2026-06-01", 1, False),
        ])
        self.assertEqual(self.runs("S000", "member-005"), [
            ("2026-04-01", "2026-05-01", 2, True), ("2026-07-01", "2026-08-01", 2, True),
        ])
        self.assertEqual(self.result["consecutive_logins"].count("A100/disjoint"), 1)
        self.assertNotIn("A100/u05", self.result["consecutive_logins"])

    def test_eligibility_uses_static_trial_flag_and_domain_token(self):
        periods = self.periods()
        self.assertEqual(periods[("T-100", "trial-label-only")], {
            "account_no": "T-100", "user_id": "trial-label-only", "total_sessions": 1, "ai_sessions": 1, "regular": True,
        })
        self.assertEqual(next(row["account_type"] for row in self.tables["ac_us_accounts_groups"] if row["account_no"] == "T-100"), "Trial")
        self.assertTrue(periods[("A100", "domain-token-control")]["regular"])
        self.assertEqual(periods[("A101", "u10")]["total_sessions"], 6)
        for identity in (("T-100", "trial"), ("I100", "internal-account"), ("I101", "internal-mixed"),
                         ("A100", "internal-team"), ("A100", "internal-staff"), ("A100", "internal-examplecorp")):
            self.assertNotIn(identity, periods)

    def test_shared_emails_and_session_ids_keep_composite_identity(self):
        periods = self.periods()
        self.assertEqual((periods[("A100", "shared")]["total_sessions"], periods[("A101", "shared")]["total_sessions"]), (2, 1))
        self.assertEqual((periods[("A100", "collision-ai")]["ai_sessions"], periods[("A101", "collision-passive")]["ai_sessions"]), (1, 0))
        self.assertEqual(self.month_row("A100", "u03", "2026-04-01")["ai_sessions"], 0)
        self.assertEqual(len([row for row in self.tables["ac_us_users"] if row["user_key"] == "member-000@example.invalid"]), 12)

    def test_passive_duplicate_and_eventless_sessions(self):
        periods = self.periods()
        self.assertEqual((periods[("A100", "silent")]["total_sessions"], periods[("A100", "silent")]["ai_sessions"]), (1, 0))
        self.assertFalse(any(row["user_id"] == "silent" for row in self.tables["ac_us_client_events"]))
        self.assertEqual((periods[("A100", "u01")]["total_sessions"], periods[("A100", "u01")]["ai_sessions"]), (18, 12))
        self.assertEqual(periods[("A100", "u03")]["ai_sessions"], 0)
        for name in ("AI_SEARCH_SUBMITTED", "AI_CHAT_SUBMITTED", "AI_SUMMARY_REQUESTED"):
            self.assertTrue(any(row["event_name"] == name for row in self.tables["ac_us_client_events"]))

    def test_session_start_controls_utc_window_month_and_product_scope(self):
        periods = self.periods()
        self.assertEqual((periods[("A100", "utc-boundaries")]["total_sessions"], periods[("A100", "utc-boundaries")]["ai_sessions"]), (2, 2))
        self.assertEqual(self.runs("A100", "utc-boundaries"), [
            ("2026-04-01", "2026-04-01", 1, False), ("2026-09-01", "2026-09-01", 1, False),
        ])
        self.assertEqual((self.month_row("A101", "u07", "2026-09-01")["total_sessions"],
                          self.month_row("A101", "u07", "2026-09-01")["ai_sessions"]), (4, 3))
        self.assertEqual((periods[("A100", "product-control")]["total_sessions"], periods[("A100", "product-control")]["ai_sessions"]), (1, 0))
        self.assertEqual(self.result["session_dispositions"], {
            "excluded_login": 72, "excluded_product": 1, "excluded_window": 4,
            "included": 4176, "missing_user": 2, "quarantined_login": 8,
        })

    def test_quarantine_reports_overlapping_login_reasons_and_relation_categories(self):
        self.assertEqual(self.result["login_dispositions"], {"eligible": 1101, "excluded_login": 6, "quarantined_login": 8})
        quarantine = self.result["quarantine"]
        self.assertEqual({kind: audit["total"] for kind, audit in quarantine.items()}, {"logins": 8, "sessions": 10, "events": 37})
        self.assertEqual(quarantine["logins"]["by_reason"], {
            "missing_email": 2, "malformed_email": 3, "unknown_trial": 2, "missing_account": 2, "unknown_account": 1,
        })
        self.assertEqual(quarantine["sessions"]["by_reason"], {"missing_user": 2, "quarantined_login": 8})
        self.assertEqual(quarantine["events"]["by_reason"], {"orphan_session": 1, "mismatched_session": 2, "quarantined_session": 34})
        multi = next(row for row in quarantine["logins"]["rows"] if row["user_id"] == "multiple-unknowns")
        self.assertEqual(multi, {"account_no": "MISSING", "user_id": "multiple-unknowns", "reasons": ["missing_email", "unknown_trial", "missing_account"]})
        self.assertTrue(all((row["account_no"], row["user_id"]) not in self.periods() for row in quarantine["sessions"]["rows"]))
        contract = {row["setting"]: row["definition"] for row in self.tables["fixture_contract"]}
        for setting in ("quarantine_logins", "quarantine_sessions", "quarantine_events", "email_validation", "metrics"):
            self.assertIn(setting, contract)

    def test_raw_event_mutation_changes_ai_answer_but_duplicate_removal_does_not(self):
        tables = deepcopy(self.tables)
        events = tables["ac_us_client_events"]
        duplicates = [row for row in events if row["session_id"] == "A100-u04-202604-0" and row["event_name"] == "AI_SEARCH_SUBMITTED"]
        self.assertEqual(len(duplicates), 2)
        events.remove(duplicates[0])
        self.assertEqual(self.month_row("A100", "u04", "2026-04-01", expected(tables))["ai_sessions"], 1)
        duplicates[1]["event_name"] = "AI_PANEL_OPENED"
        result = expected(tables)
        self.assertEqual(self.month_row("A100", "u04", "2026-04-01", result)["ai_sessions"], 0)
        self.assertEqual(result["monthly"][0], {"month": "2026-04-01", "active_users": 857, "regular_users": 489})
        self.assertEqual(result["whole_period"], {"active_users": 980, "regular_users": 611})

    def test_raw_session_mutation_recomputes_month_without_trusting_month_hint(self):
        tables = deepcopy(self.tables)
        session = next(row for row in tables["ac_us_sessions"] if row["user_id"] == "u04")
        session["session_start"] = "2026-05-01 00:00:00"
        session["session_end"] = "2026-05-01 00:20:00"
        result = expected(tables)
        self.assertEqual(result["monthly"][:2], [
            {"month": "2026-04-01", "active_users": 856, "regular_users": 489},
            {"month": "2026-05-01", "active_users": 609, "regular_users": 245},
        ])
        self.assertEqual(self.month_row("A100", "u04", "2026-05-01", result)["ai_sessions"], 1)
        self.assertEqual(result["whole_period"], {"active_users": 980, "regular_users": 612})

    def test_repairing_mismatched_event_only_changes_its_exact_session(self):
        tables = deepcopy(self.tables)
        event = next(row for row in tables["ac_us_client_events"] if row["session_id"] == "A100-u03-202604-0" and row["user_id"] == "u04")
        event["user_id"] = "u03"
        result = expected(tables)
        self.assertEqual(self.month_row("A100", "u03", "2026-04-01", result), {
            "account_no": "A100", "user_id": "u03", "month": "2026-04-01", "total_sessions": 2, "ai_sessions": 1, "regular": False,
        })
        self.assertEqual(result["quarantine"]["events"]["by_reason"]["mismatched_session"], 1)
        self.assertEqual(self.month_row("A100", "u04", "2026-04-01", result)["ai_sessions"], 1)

    def test_each_submitted_or_requested_event_can_independently_mark_ai_use(self):
        for name in ("AI_SEARCH_SUBMITTED", "AI_CHAT_SUBMITTED", "AI_SUMMARY_REQUESTED"):
            tables = deepcopy(self.tables)
            tables["ac_us_client_events"].append({
                "event_id": 1, "account_no": "A100", "user_id": "silent", "session_id": "A100-silent-202604-0",
                "event_time": "2026-04-05 12:01:00", "event_name": name, "latency_ms": None,
            })
            result = expected(tables)
            self.assertEqual(self.month_row("A100", "silent", "2026-04-01", result)["ai_sessions"], 1, name)
            self.assertEqual(result["whole_period"], {"active_users": 980, "regular_users": 613}, name)


if __name__ == "__main__":
    unittest.main()
