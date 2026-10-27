import json
from pathlib import Path


SCHEMAS = {
    "ac_us_accounts_groups": [
        ("account_no", "string", "Synthetic customer account identifier."),
        ("account_type", "string", "Internal accounts contain Internal Example Users, case insensitive."),
    ],
    "ac_us_users": [
        ("account_no", "string", "Part of the login identity with user_id."),
        ("user_id", "string", "Login identifier, unique within an account."),
        ("user_key", "string", "Fictional email or null. Validate per fixture_contract before eligibility; equal emails can have separate account logins."),
        ("is_trial", "boolean", "Static synthetic trial flag. Null is unknown and quarantined, never treated as false."),
        ("is_freemium", "boolean", "Freemium is eligible unless is_trial is true."),
    ],
    "ac_us_sessions": [
        ("session_id", "string", "Established synthetic session ID, unique with account_no and user_id. Do not infer sessions from event counts."),
        ("account_no", "string", "Customer account."),
        ("user_id", "string", "Account-scoped login."),
        ("session_start", "string", "UTC timestamp. Session start determines the reporting month."),
        ("session_end", "string", "UTC timestamp. A session can cross a month boundary."),
        ("reporting_month", "string", "YYYY-MM-01 in UTC, attributed by session_start."),
        ("product", "string", "Synthetic product scope is DEMO."),
    ],
    "ac_us_client_events": [
        ("event_id", "bigint", "Exact event identifier. Includes integers greater than 2^53."),
        ("session_id", "string", "Join to ac_us_sessions on account_no, user_id and session_id; unmatched events are quarantined."),
        ("account_no", "string", "Customer account."),
        ("user_id", "string", "Account-scoped login."),
        ("event_time", "string", "UTC event timestamp. Session attribution can differ at boundaries."),
        ("event_name", "string", "AI_SEARCH_SUBMITTED, AI_CHAT_SUBMITTED, AI_SUMMARY_REQUESTED qualify. Opens do not."),
        ("latency_ms", "decimal(20,4)", "Synthetic decimal or SQL null."),
    ],
    "fixture_contract": [
        ("setting", "string", "Synthetic fixture assumption name."),
        ("definition", "string", "Assumption for this sandbox, not a verified production rule."),
    ],
}

TABLE_COMMENTS = {
    "ac_us_accounts_groups": "SYNTHETIC. One row per account. Internal Example Users is a case-insensitive substring.",
    "ac_us_users": "SYNTHETIC. One row per account_no/user_id. Trial flag is authoritative here only. Internal email domain first tokens are examplecorp, staff, team.",
    "ac_us_sessions": "SYNTHETIC established sessions, keyed by account_no/user_id/session_id. All reporting follows session_start in UTC, including events crossing month boundaries. Read fixture_contract.",
    "ac_us_client_events": "SYNTHETIC events, including deliberate orphan and mismatched rows. Multiple AI events may share one session. Only submitted searches, submitted chats and requested summaries count as AI use.",
    "fixture_contract": "SYNTHETIC fixture assumptions and known production uncertainties. Read this table before interpreting results.",
}


EMPTY = ((0, 0),) * 6
CASE_RECIPES = {
    "mostly_ai": ((3, 2),) * 6,
    "exact_half": ((2, 1),) * 6,
    "zero_ai": ((2, 0),) * 6,
    "one_of_one": ((1, 1),) + EMPTY[1:],
    "gapped": ((1, 1), (0, 0), (1, 1), (0, 0), (0, 0), (0, 0)),
    "middle_run": ((0, 0),) + ((1, 1),) * 3 + ((0, 0),) * 2,
    "late_run": ((0, 0),) * 4 + ((4, 3),) * 2,
    "mixed_share": ((3, 2), (3, 1)) + EMPTY[2:],
    "changing_share": ((2, 0), (2, 1), (2, 2)) + ((2, 0),) * 3,
    "all_ai": ((1, 1),) * 6,
    "shared_first": ((2, 1),) + EMPTY[1:],
    "shared_last": EMPTY[:5] + ((1, 1),),
    "excluded": ((2, 2),) * 6,
    "inactive": EMPTY,
    "disjoint_runs": ((1, 1), (1, 1), (0, 0), (1, 1), (1, 1), (1, 1)),
    "single_session": ((1, 0),) + EMPTY[1:],
    "scale_steady": ((1, 1),) * 6,
    "scale_half": ((2, 1),) * 3 + EMPTY[3:],
    "scale_zero": ((1, 0),) * 6,
    "scale_disjoint": ((1, 1), (1, 1), (0, 0), (1, 1), (1, 1), (0, 0)),
    "scale_gaps": ((1, 1), (0, 0)) * 3,
    "scale_passive": ((1, 0),) * 2 + EMPTY[2:],
    "scale_end": EMPTY[:3] + ((2, 2),) * 3,
}

CORE_LOGINS = (
    ("mostly_ai", "A100", "u01", "one@example.invalid", False, False),
    ("exact_half", "A100", "u02", "two@example.invalid", False, False),
    ("zero_ai", "A100", "u03", "three@example.invalid", False, False),
    ("one_of_one", "A100", "u04", "four@example.invalid", False, False),
    ("gapped", "A100", "u05", "five@example.invalid", False, False),
    ("middle_run", "A101", "u06", "six@example.invalid", False, False),
    ("late_run", "A101", "u07", "seven@example.invalid", False, False),
    ("mixed_share", "A101", "u08", "eight@example.invalid", False, False),
    ("changing_share", "A101", "u09", "nine@example.invalid", False, False),
    ("all_ai", "A101", "u10", "ten@example.invalid", False, True),
    ("shared_first", "A100", "shared", "same@example.invalid", False, False),
    ("shared_last", "A101", "shared", "same@example.invalid", False, False),
    ("excluded", "I100", "internal-account", "outside@example.invalid", False, False),
    ("excluded", "I101", "internal-mixed", "outside2@example.invalid", False, False),
    ("excluded", "A100", "internal-team", "person@TEAM.example.invalid", False, False),
    ("excluded", "A100", "internal-staff", "person@staff.example.invalid", False, False),
    ("excluded", "A100", "internal-examplecorp", "person@examplecorp.example.invalid", False, False),
    ("excluded", "T-100", "trial", "trial@example.invalid", True, False),
    ("inactive", "A101", "inactive", "inactive@example.invalid", False, False),
    ("disjoint_runs", "A100", "disjoint", "disjoint@example.invalid", False, False),
    ("single_session", "A100", "silent", "silent@example.invalid", False, False),
    ("one_of_one", "A100", "null-email", None, False, False),
    ("one_of_one", "A100", "malformed-email", "not-an-email", False, False),
    ("one_of_one", "A100", "double-at", "person@@example.invalid", False, False),
    ("one_of_one", "A100", "empty-domain-token", "person@.invalid", False, False),
    ("one_of_one", "A100", "unknown-trial", "unknown@example.invalid", None, False),
    ("one_of_one", "MISSING", "missing-account", "missing@example.invalid", False, False),
    ("one_of_one", "UNKNOWN", "unknown-account", "unknown-account@example.invalid", False, False),
    ("one_of_one", "MISSING", "multiple-unknowns", None, None, False),
    ("one_of_one", "T-100", "trial-label-only", "label@example.invalid", False, False),
    ("one_of_one", "A100", "domain-token-control", "team@teampartners.example.invalid", False, False),
    ("inactive", "A100", "utc-boundaries", "utc@example.invalid", False, False),
    ("single_session", "A100", "product-control", "product@example.invalid", False, False),
    ("inactive", "A100", "collision-ai", "collision-ai@example.invalid", False, False),
    ("inactive", "A101", "collision-passive", "collision-passive@example.invalid", False, False),
)

SCALE_CASES = (
    "scale_steady", "scale_half", "scale_zero", "inactive", "one_of_one",
    "scale_disjoint", "scale_gaps", "scale_passive", "scale_end",
)

SESSION_OVERRIDES = {
    ("A101", "u07", 9, 0): ("2026-09-30 23:55:00", "2026-10-01 00:15:00"),
}
EVENT_TIME_OVERRIDES = {
    ("A101-u07-202609-0", "AI_SEARCH_SUBMITTED"): "2026-10-01 00:05:00",
}
EVENT_NAME_OVERRIDES = {("A100", "silent"): ()}

SPECIAL_SESSIONS = (
    dict(account="A100", user="u05", sid="outside-window-3", start="2026-03-15 12:00:00", end="2026-03-15 12:20:00", product="DEMO", names=("AI_SEARCH_SUBMITTED",)),
    dict(account="A100", user="u05", sid="outside-window-10", start="2026-10-15 12:00:00", end="2026-10-15 12:20:00", product="DEMO", names=("AI_SEARCH_SUBMITTED",)),
    dict(account="A100", user="utc-boundaries", sid="utc-before", start="2026-03-31 23:59:59", end="2026-04-01 00:20:00", product="DEMO", names=("AI_CHAT_SUBMITTED",)),
    dict(account="A100", user="utc-boundaries", sid="utc-start", start="2026-04-01 00:00:00", end="2026-04-01 00:20:00", product="DEMO", names=("AI_SUMMARY_REQUESTED",)),
    dict(account="A100", user="utc-boundaries", sid="utc-end", start="2026-09-30 23:59:59", end="2026-10-01 00:20:00", product="DEMO", names=("AI_CHAT_SUBMITTED",)),
    dict(account="A100", user="utc-boundaries", sid="utc-after", start="2026-10-01 00:00:00", end="2026-10-01 00:20:00", product="DEMO", names=("AI_SEARCH_SUBMITTED",)),
    dict(account="A100", user="product-control", sid="other-product-control", start="2026-04-05 12:00:00", end="2026-04-05 12:20:00", product="OTHER", names=("AI_SEARCH_SUBMITTED",)),
    dict(account="A100", user="collision-ai", sid="reused-session-id", start="2026-04-05 12:00:00", end="2026-04-05 12:20:00", product="DEMO", names=("AI_CHAT_SUBMITTED",)),
    dict(account="A101", user="collision-passive", sid="reused-session-id", start="2026-04-05 12:00:00", end="2026-04-05 12:20:00", product="DEMO", names=("AI_PANEL_OPENED",)),
    dict(account="A100", user="absent-user", sid="orphan-user-session", start="2026-04-05 12:00:00", end="2026-04-05 12:20:00", product="DEMO", names=("AI_SEARCH_SUBMITTED",)),
    dict(account="MISSING", user="absent-user", sid="orphan-account-session", start="2026-04-05 12:00:00", end="2026-04-05 12:20:00", product="DEMO", names=("AI_SEARCH_SUBMITTED",)),
)


def generate():
    tables = {name: [] for name in SCHEMAS}
    tables["ac_us_accounts_groups"] = [
        {"account_no": "A100", "account_type": "Subscriber"},
        {"account_no": "A101", "account_type": "Freemium ; Subscriber"},
        {"account_no": "I100", "account_type": "All_Groups ; Internal Example Users ; QA"},
        {"account_no": "I101", "account_type": "internal example users"},
        {"account_no": "T-100", "account_type": "Trial"},
        {"account_no": "UNKNOWN", "account_type": None},
    ] + [dict(account_no=f"S{account:03d}", account_type="Subscriber") for account in range(12)]
    logins = list(CORE_LOGINS)
    for account in range(12):
        for member in range(90):
            logins.append((SCALE_CASES[member % len(SCALE_CASES)], f"S{account:03d}",
                           f"member-{member:03d}", f"member-{member:03d}@example.invalid", False, member % 5 == 0))

    def add_session(account, user, sid, start, end, product, names):
        tables["ac_us_sessions"].append(dict(session_id=sid, account_no=account, user_id=user,
            session_start=start, session_end=end, reporting_month=start[:7] + "-01", product=product))
        for name in names:
            tables["ac_us_client_events"].append(dict(event_id=9007199254740993 + len(tables["ac_us_client_events"]),
                session_id=sid, account_no=account, user_id=user, event_time=EVENT_TIME_OVERRIDES.get((sid, name), start),
                event_name=name, latency_ms="1.2300" if name.endswith("SUBMITTED") else None))

    for case, account, user, email, trial, freemium in logins:
        tables["ac_us_users"].append(dict(account_no=account, user_id=user, user_key=email, is_trial=trial, is_freemium=freemium))
        for month, (total, ai) in enumerate(CASE_RECIPES[case], 4):
            for index in range(total):
                start, end = SESSION_OVERRIDES.get((account, user, month, index),
                    (f"2026-{month:02d}-{5 + index:02d} 12:00:00", f"2026-{month:02d}-{5 + index:02d} 12:20:00"))
                names = ["PAGE_VIEW", "AI_PANEL_OPENED"]
                if index < ai:
                    names += [["AI_SEARCH_SUBMITTED", "AI_CHAT_SUBMITTED", "AI_SUMMARY_REQUESTED"][index % 3]] * 2
                names = EVENT_NAME_OVERRIDES.get((account, user), names)
                add_session(account, user, f"{account}-{user}-2026{month:02d}-{index}", start, end, "DEMO", names)
    for session in SPECIAL_SESSIONS:
        add_session(**session)
    for sid, account, user in (
        ("event-without-session", "A100", "u03"),
        ("A100-u03-202604-0", "A100", "u04"),
        ("reused-session-id", "A101", "collision-ai"),
    ):
        tables["ac_us_client_events"].append(dict(event_id=9007199254740993 + len(tables["ac_us_client_events"]),
            session_id=sid, account_no=account, user_id=user, event_time="2026-04-05 12:01:00",
            event_name="AI_SEARCH_SUBMITTED", latency_ms=None))
    settings = {
        "synthetic": "All data is fictional. Totals are test results, never business measurements.",
        "scope": "DEMO only is a fixture assumption. Define production product scope separately.",
        "identity": "Count account_no plus user_id. Equal email across accounts represents separate logins here; user_id is account-scoped.",
        "sessions": "ac_us_sessions supplies established sessions keyed by (account_no, user_id, session_id). Session IDs can repeat across logins. This is not the missing production ETL.",
        "calendar": "UTC. Filter session_start >= 2026-04-01 00:00:00 and < 2026-10-01 00:00:00. Derive its month from session_start; reporting_month is a redundant aid. Event timestamps and session_end do not change attribution.",
        "ai_use": "Any AI_SEARCH_SUBMITTED, AI_CHAT_SUBMITTED or AI_SUMMARY_REQUESTED event matching all three session key fields marks that session as AI once. AI_PANEL_OPENED and PAGE_VIEW do not. Define production AI activity separately.",
        "eligibility": "After quarantine checks, exclude account_type containing Internal Example Users case-insensitively, first email-domain tokens examplecorp/staff/team case-insensitively, and is_trial=true. Freemium alone is eligible. Trial-like account labels do not exclude a user whose is_trial is false. Trial flags are static synthetic assumptions.",
        "quarantine_logins": "Quarantine unknown email (null), malformed email, is_trial other than boolean true/false, missing account dimension, or unknown account_type (null/blank). Audit each reason separately; a login can have multiple reasons. Quarantine precedes eligibility exclusions.",
        "email_validation": "Synthetic ASCII shape only: local part [A-Za-z0-9._%+-]+, one @, at least two dot-separated domain labels. Each label starts/ends alphanumeric with interior hyphens allowed; final label is at least two letters. No whitespace. Null is missing_email; any other failure is malformed_email. This does not establish production identity rules.",
        "quarantine_sessions": "Exclude and separately audit sessions with no matching (account_no,user_id) user dimension or a quarantined login. Other sessions are classified by login exclusion, product and UTC window in that order.",
        "quarantine_events": "Join events on (account_no,user_id,session_id). If no exact session exists, classify orphan_session when session_id is absent everywhere, otherwise mismatched_session. Events attached to quarantined sessions are quarantined_session. Audit categories separately. Do not repair or reassign events.",
        "metrics": "Active means at least one eligible in-window DEMO session. Regular means strictly more than half of sessions are AI, monthly or pooled across the whole window. Consecutive means AI use in at least two adjacent calendar months, even at or below 50%. Return maximal AI-month runs; gaps break runs.",
        "grain": "Account and login dimensions have unique keys. Sessions have unique composite keys. Events have unique event_id and many rows per session. Deduplicate AI membership before counting sessions. Inactive logins have no active user-month rows.",
        "cases": "Original u01-u10/shared/internal/trial/inactive cases remain. disjoint has two runs; silent has no events; utc-boundaries tests exact window edges; product-control has OTHER AI use; collision-ai/collision-passive share a session ID. Quality case user IDs describe defects.",
        "scale_recipes": "S000 through S011 each have member-000 through member-089, shared emails and account-scoped IDs. Cycle steady AI, exact half, zero AI, inactive, one-of-one, disjoint runs, gapped months, passive opens and late AI runs.",
        "production_gaps": "Unverified production scope, AI predicates, session ETL, trial eligibility, identity and calendar require independent validation.",
    }
    tables["fixture_contract"] = [dict(setting=k, definition=v) for k, v in settings.items()]
    return tables


def write(directory):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    for table, rows in generate().items():
        (directory / f"{table}.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows))


if __name__ == "__main__":
    import sys
    write(sys.argv[1])
