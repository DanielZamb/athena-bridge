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

    def station_month(self, station, month, result=None):
        result = self.result if result is None else result
        return next((row for row in result["station_months"] if (row["station_id"], row["month"]) == (station, month)), None)

    def test_deterministic_relations_match_metadata_and_literal_counts(self):
        self.assertEqual(generate(), self.tables)
        self.assertEqual(self.result["relation_counts"], {
            "stations": 12, "sensors": 36, "readings": 4146, "maintenance_visits": 36, "dataset_notes": 5,
        })
        self.assertEqual(set(SCHEMAS), set(TABLE_COMMENTS))
        self.assertTrue(all("SYNTHETIC" in comment for comment in TABLE_COMMENTS.values()))
        for name, rows in self.tables.items():
            fields = {field for field, _, _ in SCHEMAS[name]}
            self.assertTrue(all(set(row) == fields for row in rows), name)
        keys = {
            "stations": ("station_id",), "sensors": ("sensor_id",), "readings": ("reading_id",),
            "maintenance_visits": ("visit_id",), "dataset_notes": ("topic",),
        }
        for name, fields in keys.items():
            self.assertEqual(len({tuple(row[field] for field in fields) for row in self.tables[name]}), len(self.tables[name]), name)
        # The live verifier orders readings by these string columns, so they must be unique.
        readings = self.tables["readings"]
        self.assertEqual(len({(row["sensor_id"], row["observed_at"], row["metric"]) for row in readings}), len(readings))

    def test_written_ndjson_is_bounded_and_preserves_exact_cells(self):
        with tempfile.TemporaryDirectory(prefix="athena-fixtures-") as directory:
            self.assertEqual(Path(directory).stat().st_mode & 0o777, 0o700)
            write(directory)
            paths = sorted(Path(directory).glob("*.jsonl"))
            self.assertEqual(len(paths), 5)
            self.assertLess(sum(path.stat().st_size for path in paths), 8 * 1024 * 1024)
            for path in paths:
                self.assertEqual([json.loads(line) for line in path.read_text().splitlines()], self.tables[path.stem])
        readings = self.tables["readings"]
        self.assertEqual(readings[0]["reading_id"], 9007199254740993)
        self.assertEqual(readings[1]["value"], "8.2500")
        self.assertIsNone(next(row for row in readings if row["reading_id"] == 9007199254741006)["value"])
        self.assertIsNone(self.tables["stations"][0]["retired_on"])

    def test_monthly_counts_and_quarantine_are_literal(self):
        self.assertEqual(self.result["monthly"], [
            {"month": "2026-04-01", "readings": 681, "reporting_stations": 12},
            {"month": "2026-05-01", "readings": 678, "reporting_stations": 12},
            {"month": "2026-06-01", "readings": 675, "reporting_stations": 12},
            {"month": "2026-07-01", "readings": 618, "reporting_stations": 11},
            {"month": "2026-08-01", "readings": 618, "reporting_stations": 11},
            {"month": "2026-09-01", "readings": 619, "reporting_stations": 11},
        ])
        self.assertEqual(self.result["quarantine"], {
            "after_retirement": 2, "missing_value": 252, "outside_window": 2, "unknown_sensor": 1,
        })

    def test_observed_at_controls_utc_window_and_month(self):
        september = self.station_month("ST01", "2026-09-01")
        self.assertEqual(september["readings"], 58)
        self.assertEqual(september["metrics"]["air_temp_c"], {"min": "12.0000", "max": "14.5000"})
        self.assertIsNone(self.station_month("ST01", "2026-10-01"))
        self.assertIsNone(self.station_month("ST01", "2026-03-01"))

    def test_retired_station_stops_counting_on_its_retirement_date(self):
        self.assertEqual(self.station_month("ST12", "2026-06-01")["readings"], 54)
        self.assertIsNone(self.station_month("ST12", "2026-07-01"))
        self.assertIsNone(self.station_month("ST12", "2026-08-01"))

    def test_null_value_removal_changes_only_its_station_month(self):
        tables = deepcopy(self.tables)
        reading = next(row for row in tables["readings"] if row["reading_id"] == 9007199254741006)
        reading["value"] = "99.0000"
        result = expected(tables)
        self.assertEqual(result["quarantine"]["missing_value"], 251)
        self.assertEqual(self.station_month("ST01", "2026-04-01", result)["readings"],
                         self.station_month("ST01", "2026-04-01")["readings"] + 1)
        self.assertEqual(self.station_month("ST01", "2026-04-01", result)["metrics"]["air_temp_c"]["max"], "99.0000")
        self.assertEqual(result["monthly"][1:], self.result["monthly"][1:])

    def test_reassigning_unknown_sensor_reading_counts_it(self):
        tables = deepcopy(self.tables)
        next(row for row in tables["readings"] if row["sensor_id"] == "ST99-T")["sensor_id"] = "ST02-T"
        result = expected(tables)
        self.assertNotIn("unknown_sensor", result["quarantine"])
        self.assertEqual(result["monthly"][1]["readings"], 679)

    def test_dataset_notes_state_every_rule(self):
        notes = {row["topic"]: row["note"] for row in self.tables["dataset_notes"]}
        self.assertEqual(set(notes), {"synthetic", "window", "valid_reading", "quarantine", "station_month"})


if __name__ == "__main__":
    unittest.main()
