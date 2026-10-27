from collections import Counter, defaultdict
from datetime import datetime, timezone
import json
import re

from fixtures import generate


AI_EVENTS = {"AI_SEARCH_SUBMITTED", "AI_CHAT_SUBMITTED", "AI_SUMMARY_REQUESTED"}
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}", re.ASCII)
WINDOW_START = datetime(2026, 4, 1, tzinfo=timezone.utc)
WINDOW_END = datetime(2026, 10, 1, tzinfo=timezone.utc)
MONTHS = tuple(f"2026-{month:02d}-01" for month in range(4, 10))


def session_key(row):
    return row["account_no"], row["user_id"], row["session_id"]


def expected(tables=None):
    if tables is None:
        tables = generate()
    accounts = {row["account_no"]: row for row in tables["ac_us_accounts_groups"]}
    users = {(row["account_no"], row["user_id"]): row for row in tables["ac_us_users"]}
    sessions = {session_key(row): row for row in tables["ac_us_sessions"]}
    session_ids = {row["session_id"] for row in sessions.values()}
    login_status, session_status = {}, {}
    quarantine = {name: {"total": 0, "by_reason": Counter(), "rows": []} for name in ("logins", "sessions", "events")}

    def quarantine_row(kind, row, reasons, fields):
        audit = quarantine[kind]
        audit["total"] += 1
        audit["by_reason"].update(reasons)
        audit["rows"].append(dict({field: row[field] for field in fields}, reasons=reasons))

    for identity, user in users.items():
        reasons = []
        email = user["user_key"]
        if email is None:
            reasons.append("missing_email")
        elif not isinstance(email, str) or EMAIL.fullmatch(email) is None:
            reasons.append("malformed_email")
        if user["is_trial"] is not True and user["is_trial"] is not False:
            reasons.append("unknown_trial")
        account = accounts.get(identity[0])
        if account is None:
            reasons.append("missing_account")
        elif not isinstance(account["account_type"], str) or not account["account_type"].strip():
            reasons.append("unknown_account")
        if reasons:
            login_status[identity] = "quarantined_login"
            quarantine_row("logins", user, reasons, ("account_no", "user_id"))
            continue
        token = email.rsplit("@", 1)[1].split(".", 1)[0].lower()
        excluded = (user["is_trial"] is True or token in {"examplecorp", "staff", "team"}
                    or "internal example users" in account["account_type"].lower())
        login_status[identity] = "excluded_login" if excluded else "eligible"

    for key, session in sessions.items():
        identity = key[:2]
        if identity not in users:
            status = "missing_user"
        elif login_status[identity] != "eligible":
            status = login_status[identity]
        elif session["product"] != "DEMO":
            status = "excluded_product"
        else:
            start = datetime.fromisoformat(session["session_start"]).replace(tzinfo=timezone.utc)
            status = "included" if WINDOW_START <= start < WINDOW_END else "excluded_window"
        session_status[key] = status
        if status in {"missing_user", "quarantined_login"}:
            quarantine_row("sessions", session, [status], ("account_no", "user_id", "session_id"))

    ai_sessions = set()
    for event in tables["ac_us_client_events"]:
        key = session_key(event)
        if key not in sessions:
            reason = "mismatched_session" if event["session_id"] in session_ids else "orphan_session"
        elif session_status[key] in {"missing_user", "quarantined_login"}:
            reason = "quarantined_session"
        else:
            reason = None
        if reason:
            quarantine_row("events", event, [reason], ("event_id", "account_no", "user_id", "session_id"))
        elif event["event_name"] in AI_EVENTS:
            ai_sessions.add(key)

    monthly, pooled = defaultdict(lambda: [0, 0]), defaultdict(lambda: [0, 0])
    for key, session in sessions.items():
        if session_status[key] != "included":
            continue
        month = session["session_start"][:7] + "-01"
        for counts in (monthly[(key[:2], month)], pooled[key[:2]]):
            counts[0] += 1
            counts[1] += int(key in ai_sessions)

    months = []
    for month in MONTHS:
        counts = [value for (_, key), value in monthly.items() if key == month]
        months.append(dict(month=month, active_users=len(counts), regular_users=sum(2 * ai > total for total, ai in counts)))
    user_months = [dict(account_no=identity[0], user_id=identity[1], month=month,
                        total_sessions=total, ai_sessions=ai, regular=2 * ai > total)
                   for (identity, month), (total, ai) in sorted(monthly.items())]
    user_periods = [dict(account_no=identity[0], user_id=identity[1], total_sessions=total,
                         ai_sessions=ai, regular=2 * ai > total)
                    for identity, (total, ai) in sorted(pooled.items())]
    ai_months = defaultdict(list)
    for (identity, month), (_, used) in sorted(monthly.items()):
        if used:
            ai_months[identity].append(month)
    runs = []
    consecutive = []
    for identity, used_months in sorted(ai_months.items()):
        current = []
        user_runs = []
        for month in used_months:
            if current and int(month[5:7]) != int(current[-1][5:7]) + 1:
                user_runs.append(current)
                current = []
            current.append(month)
        user_runs.append(current)
        for run in user_runs:
            runs.append(dict(account_no=identity[0], user_id=identity[1], start_month=run[0],
                             end_month=run[-1], month_count=len(run), qualifies=len(run) >= 2))
        if any(len(run) >= 2 for run in user_runs):
            consecutive.append(dict(account_no=identity[0], user_id=identity[1]))
    return {
        "synthetic": True,
        "monthly": months,
        "whole_period": {"active_users": len(pooled), "regular_users": sum(2 * ai > total for total, ai in pooled.values())},
        "consecutive_user_count": len(consecutive),
        "consecutive_logins": [row["account_no"] + "/" + row["user_id"] for row in consecutive],
        "consecutive_login_rows": consecutive,
        "per_user_month": user_months,
        "per_user_period": user_periods,
        "maximal_runs": runs,
        "relation_counts": {name: len(rows) for name, rows in tables.items()},
        "login_dispositions": dict(sorted(Counter(login_status.values()).items())),
        "session_dispositions": dict(sorted(Counter(session_status.values()).items())),
        "quarantine": quarantine,
    }


if __name__ == "__main__":
    print(json.dumps(expected(), indent=2))
