import importlib.util
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from cloud import Aws, AwsError, owned_stack, save_manifest
from iam import policies
from stack import template


def cleanup_module():
    spec = importlib.util.spec_from_file_location("sandbox_cleanup_tests", Path(__file__).resolve().parents[1] / "scripts/sandbox-cleanup.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class LifecycleTests(unittest.TestCase):
    def test_live_verifier_rejects_source_and_manifest_fixture_drift(self):
        spec = importlib.util.spec_from_file_location("sandbox_verify_tests", Path(__file__).resolve().parents[1] / "scripts/sandbox-verify.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        import hashlib
        from fixtures import write
        with tempfile.TemporaryDirectory() as directory:
            write(directory)
            hashes = {path.stem: hashlib.sha256(path.read_bytes()).hexdigest() for path in Path(directory).glob("*.jsonl")}
        self.assertEqual(module.check_fixture_source(hashes), hashes)
        hashes["ac_us_users"] = "0" * 64
        with self.assertRaisesRegex(AssertionError, "regenerated"):
            module.check_fixture_source(hashes)
        with self.assertRaisesRegex(AssertionError, "regenerated"):
            module.check_fixture_source({})

    def test_access_cleanup_retains_borrowed_policies(self):
        spec = importlib.util.spec_from_file_location("access_cleanup_test", Path(__file__).resolve().parents[1] / "scripts/sandbox-access-cleanup.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        class NoPolicyMutation:
            def __init__(self, *arguments):
                pass

            def verify_account(self):
                pass

            def call(self, *arguments, **keywords):
                raise AssertionError("Borrowed policies must not be accessed or removed")
        with tempfile.TemporaryDirectory() as directory:
            resource = Path(directory) / "resources.json"
            access = Path(directory) / "access.json"
            resource.write_text(json.dumps({"phase": "cleaned", "account": "123456789012", "region": "us-east-1"}))
            access.write_text(json.dumps({"account": "123456789012", "user": "sandbox-user", "created_arns": []}))
            arguments = ["cleanup", "--profile", "fake-admin", "--user", "sandbox-user", "--manifest", str(resource), "--access-manifest", str(access)]
            with patch.object(module, "Aws", NoPolicyMutation), patch("sys.argv", arguments), contextlib.redirect_stdout(io.StringIO()):
                module.main()

    def test_delete_retry_waits_past_the_previous_failure(self):
        class Retried(Aws):
            def __init__(self):
                self.states = iter([{"StackStatus": "ROLLBACK_COMPLETE"}, {"StackStatus": "DELETE_FAILED"}, {"StackStatus": "DELETE_IN_PROGRESS"}, None])

            def stack(self, identity):
                return next(self.states)
        with patch("cloud.time.sleep"):
            self.assertIsNone(Retried().wait_stack("owned", "DELETE_COMPLETE", timeout=1))

    def test_create_does_not_wait_for_a_deleted_stack(self):
        class Deleted(Aws):
            def __init__(self):
                pass

            def stack(self, identity):
                return {"StackStatus": "DELETE_COMPLETE"}

            def call(self, *arguments, **keywords):
                return {"StackEvents": []}
        with self.assertRaisesRegex(RuntimeError, "Stack failed"):
            Deleted().wait_stack("deleted", "CREATE_COMPLETE", timeout=1)

    def test_reviewed_policy_files_and_template_ownership_names(self):
        directory = Path(__file__).with_name("iam")
        for name, document in policies("123456789012", "us-east-1").items():
            self.assertEqual(json.loads((directory / name).read_text()), document)
        resources = template("athena-bridge-lab-example")["Resources"]
        self.assertEqual(len(resources), 11)
        self.assertEqual(resources["WorkerRole"]["Properties"]["RoleName"], "athena-bridge-lab-example-worker-role")
        self.assertIn("PermissionsBoundary", resources["WorkerRole"]["Properties"])

    def test_owned_stack_does_not_adopt_a_different_deployment(self):
        class Unrelated:
            def stack(self, identity):
                return {"StackName": "athena-bridge-lab-one", "Tags": [{"Key": "AthenaBridgeSandbox", "Value": "another-deployment"}]}
        with self.assertRaisesRegex(RuntimeError, "ownership"):
            owned_stack(Unrelated(), {"sandbox_id": "mine", "stack_name": "athena-bridge-lab-one"})

    def test_manifest_is_published_with_private_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "manifest.json"
            save_manifest(path, {"phase": "intent"})
            self.assertEqual(json.loads(path.read_text()), {"phase": "intent"})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            save_manifest(path, {"phase": "ready"})
            self.assertEqual(json.loads(path.read_text()), {"phase": "ready"})
            self.assertEqual([item.name for item in Path(directory).iterdir()], ["manifest.json"])

    def test_access_denied_is_not_treated_as_resource_absence(self):
        class Denied:
            def call(self, *arguments, **keywords):
                raise AwsError("AccessDenied")
        with self.assertRaises(AwsError):
            cleanup_module().absent(Denied(), "s3api", "head-bucket", ["NoSuchBucket", "(404)"], Bucket="example")

    def test_partial_s3_deletion_does_not_claim_cleanup_success(self):
        class PartialDeletion:
            def call(self, service, operation, **keywords):
                if operation == "head-bucket":
                    return {}
                if operation == "list-object-versions":
                    return {"Versions": [{"Key": "data/rows.jsonl", "VersionId": "null"}]}
                if operation == "delete-objects":
                    return {"Errors": [{"Code": "AccessDenied"}]}
                raise AssertionError(operation)
        with self.assertRaisesRegex(RuntimeError, "could not delete"):
            cleanup_module().drain_bucket(PartialDeletion(), "owned-bucket")


if __name__ == "__main__":
    unittest.main()
