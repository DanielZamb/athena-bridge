import json
from pathlib import Path
import unittest

from iam import policies
from stack import template


class WorkerPermissionTests(unittest.TestCase):
    def test_runtime_actions_match_sandbox_allowlist(self):
        statements = policies("123456789012", "us-east-1")["worker-boundary.json"]["Statement"]
        resources = template("athena-bridge-lab-example")["Resources"]
        role = resources["WorkerRole"]["Properties"]
        grant = role["Policies"][0]["PolicyDocument"]["Statement"]
        expected = {"athena:StartQueryExecution", "athena:GetQueryExecution", "athena:GetQueryResults",
            "athena:StopQueryExecution", "athena:GetWorkGroup",
            "glue:GetDatabase", "glue:GetTable", "glue:GetTables", "glue:GetPartitions",
            "s3:ListBucket", "s3:GetBucketLocation", "s3:GetObject", "s3:PutObject",
            "logs:CreateLogStream", "logs:PutLogEvents"}
        for policy in [statements, grant]:
            self.assertEqual({action for statement in policy for action in statement["Action"]}, expected)
            for statement in policy:
                if "athena:StopQueryExecution" in statement["Action"]:
                    self.assertIn("workgroup/", json.dumps(statement["Resource"]))
        self.assertEqual(json.loads(Path(__file__).with_name("iam").joinpath("worker-boundary.json").read_text()),
            policies("123456789012", "us-east-1")["worker-boundary.json"])
        self.assertEqual(resources["Worker"]["Properties"]["Environment"]["Variables"]["WORKER_ID"], "athena-bridge-lab-example-worker")

    def test_provisioner_retains_cleanup_rights_removed_from_worker(self):
        provisioner = policies("123456789012", "us-east-1")["provisioner-policy.json"]
        actions = {action for statement in provisioner["Statement"] if statement["Effect"] == "Allow" for action in statement["Action"]}
        self.assertTrue({"s3:ListBucketMultipartUploads", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts",
            "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:ListBucketVersions", "s3:DeleteBucket",
            "athena:ListQueryExecutions", "athena:StopQueryExecution", "athena:DeleteWorkGroup",
            "glue:DeleteTable", "glue:DeleteDatabase", "lambda:DeleteFunction", "iam:DeleteRole",
            "logs:DeleteLogGroup", "cloudformation:DeleteStack"}.issubset(actions))
        self.assertEqual(json.loads(Path(__file__).with_name("iam").joinpath("provisioner-policy.json").read_text()), provisioner)


if __name__ == "__main__":
    unittest.main()
