from collections import Counter, defaultdict
from decimal import Decimal
import json

from fixtures import generate


WINDOW_START = "2026-04-01 00:00:00"
WINDOW_END = "2026-10-01 00:00:00"


def expected(tables=None):
    """Independent answer for the rules in dataset_notes."""
    if tables is None:
        tables = generate()
    stations = {row["station_id"]: row for row in tables["stations"]}
    sensors = {row["sensor_id"]: row for row in tables["sensors"]}
    quarantine = Counter()
    station_months = defaultdict(lambda: {"readings": 0, "metrics": {}})
    for reading in tables["readings"]:
        sensor = sensors.get(reading["sensor_id"])
        retired = stations[sensor["station_id"]]["retired_on"] if sensor else None
        if sensor is None:
            reason = "unknown_sensor"
        elif retired is not None and reading["observed_at"][:10] >= retired:
            reason = "after_retirement"
        elif not WINDOW_START <= reading["observed_at"] < WINDOW_END:
            reason = "outside_window"
        elif reading["value"] is None:
            reason = "missing_value"
        else:
            reason = None
        if reason:
            quarantine[reason] += 1
            continue
        cell = station_months[(sensor["station_id"], reading["observed_at"][:7] + "-01")]
        cell["readings"] += 1
        value = Decimal(reading["value"])
        low, high = cell["metrics"].get(reading["metric"], (value, value))
        cell["metrics"][reading["metric"]] = (min(low, value), max(high, value))

    rows = [dict(station_id=station, month=month, readings=cell["readings"],
                 metrics={metric: {"min": f"{low:.4f}", "max": f"{high:.4f}"} for metric, (low, high) in sorted(cell["metrics"].items())})
            for (station, month), cell in sorted(station_months.items())]
    monthly = Counter()
    reporting = defaultdict(set)
    for row in rows:
        monthly[row["month"]] += row["readings"]
        reporting[row["month"]].add(row["station_id"])
    return {
        "synthetic": True,
        "relation_counts": {name: len(table) for name, table in tables.items()},
        "monthly": [dict(month=month, readings=monthly[month], reporting_stations=len(reporting[month])) for month in sorted(monthly)],
        "station_months": rows,
        "quarantine": dict(sorted(quarantine.items())),
    }


if __name__ == "__main__":
    print(json.dumps(expected(), indent=2))
