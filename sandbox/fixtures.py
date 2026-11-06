import json
from decimal import Decimal
from pathlib import Path


SCHEMAS = {
    "stations": [
        ("station_id", "string", "Synthetic weather station identifier."),
        ("region", "string", "Fictional region name."),
        ("elevation_m", "decimal(10,2)", "Elevation in meters."),
        ("commissioned_on", "string", "UTC date the station began reporting."),
        ("retired_on", "string", "UTC date the station stopped reporting, or SQL null while active."),
    ],
    "sensors": [
        ("sensor_id", "string", "Sensor identifier, prefixed by its station."),
        ("station_id", "string", "Host station; joins to stations."),
        ("kind", "string", "temperature, humidity or wind."),
        ("installed_on", "string", "UTC installation date."),
    ],
    "readings": [
        ("reading_id", "bigint", "Exact reading identifier. Includes integers greater than 2^53."),
        ("sensor_id", "string", "Joins to sensors; unknown sensors are quarantined."),
        ("observed_at", "string", "UTC observation timestamp. Its month is the reporting month."),
        ("metric", "string", "air_temp_c, rel_humidity_pct or wind_speed_ms."),
        ("value", "decimal(20,4)", "Measured value, or SQL null when the sensor sent no value."),
    ],
    "maintenance_visits": [
        ("visit_id", "string", "Visit identifier."),
        ("station_id", "string", "Visited station."),
        ("visited_on", "string", "UTC visit date."),
        ("outcome", "string", "calibrated, replaced or no_access."),
        ("notes", "string", "Free text, or SQL null."),
    ],
    "dataset_notes": [
        ("topic", "string", "Fixture rule name."),
        ("note", "string", "Rule for this sandbox dataset."),
    ],
}

TABLE_COMMENTS = {
    "stations": "SYNTHETIC. One row per weather station.",
    "sensors": "SYNTHETIC. One row per sensor. Each station hosts one sensor of each kind.",
    "readings": "SYNTHETIC sensor readings, including null values, an unknown sensor and readings after a station retired.",
    "maintenance_visits": "SYNTHETIC field visits to stations.",
    "dataset_notes": "SYNTHETIC rules for interpreting this dataset. Read this table first.",
}

REGIONS = ("north", "coast", "valley", "ridge")
KINDS = (("temperature", "T", "air_temp_c", Decimal("4.0000")),
         ("humidity", "H", "rel_humidity_pct", Decimal("40.0000")),
         ("wind", "W", "wind_speed_ms", Decimal("1.5000")))
MONTHS = tuple(range(4, 10))
READINGS_PER_MONTH = 20
RETIRED = {"ST12": "2026-07-01"}


def station_ids():
    return [f"ST{index:02d}" for index in range(1, 13)]


def generate():
    tables = {name: [] for name in SCHEMAS}
    for index, station in enumerate(station_ids()):
        tables["stations"].append(dict(station_id=station, region=REGIONS[index % 4],
            elevation_m=f"{120 + index * 37.5:.2f}", commissioned_on="2025-0{}-01".format(1 + index % 9),
            retired_on=RETIRED.get(station)))
        for kind, suffix, _, _ in KINDS:
            tables["sensors"].append(dict(sensor_id=f"{station}-{suffix}", station_id=station, kind=kind,
                                          installed_on="2026-01-15"))

    def add_reading(sensor, observed_at, metric, value):
        tables["readings"].append(dict(reading_id=9007199254740993 + len(tables["readings"]),
            sensor_id=sensor, observed_at=observed_at, metric=metric, value=value))

    for index, station in enumerate(station_ids()):
        last_month = 6 if station in RETIRED else MONTHS[-1]
        for _, suffix, metric, base in KINDS:
            for month in range(MONTHS[0], last_month + 1):
                for step in range(READINGS_PER_MONTH):
                    observed = f"2026-{month:02d}-{1 + step:02d} {(index + step) % 24:02d}:30:00"
                    value = None if (index + month + step) % 17 == 0 else \
                        f"{base + Decimal(index) / 4 + Decimal(month) + Decimal(step % 7) * Decimal('0.2500'):.4f}"
                    add_reading(f"{station}-{suffix}", observed, metric, value)
    # Edge cases: UTC month boundary, retirement, outside the window, and an unknown sensor.
    add_reading("ST01-T", "2026-09-30 23:59:59", "air_temp_c", "12.0000")
    add_reading("ST01-T", "2026-10-01 00:00:00", "air_temp_c", "11.5000")
    add_reading("ST01-T", "2026-03-31 23:59:59", "air_temp_c", "6.2500")
    add_reading("ST12-T", "2026-07-02 08:30:00", "air_temp_c", "15.0000")
    add_reading("ST12-W", "2026-08-10 08:30:00", "wind_speed_ms", "3.0000")
    add_reading("ST99-T", "2026-05-05 05:30:00", "air_temp_c", "9.0000")

    outcomes = ("calibrated", "replaced", "no_access")
    for index, station in enumerate(station_ids()):
        for visit, month in enumerate(MONTHS[index % 2::2]):
            tables["maintenance_visits"].append(dict(visit_id=f"V-{station}-{visit}", station_id=station,
                visited_on=f"2026-{month:02d}-{10 + index:02d}", outcome=outcomes[(index + visit) % 3],
                notes=None if (index + visit) % 4 == 0 else f"Routine check {visit + 1}."))

    notes = {
        "synthetic": "All data is fictional. Totals are test results, not measurements.",
        "window": "UTC. Count readings with observed_at >= 2026-04-01 00:00:00 and < 2026-10-01 00:00:00. The month of observed_at is the reporting month.",
        "valid_reading": "A reading is valid when its sensor exists, its station had not retired by the observation date, it is inside the window, and value is not null.",
        "quarantine": "Classify invalid readings once, in this order: unknown_sensor, after_retirement, outside_window, missing_value.",
        "station_month": "For each station and month, report the valid reading count and the minimum and maximum value per metric.",
    }
    tables["dataset_notes"] = [dict(topic=key, note=value) for key, value in notes.items()]
    return tables


def write(directory):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    for table, rows in generate().items():
        (directory / f"{table}.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows))


if __name__ == "__main__":
    import sys
    write(sys.argv[1])
