#!/usr/bin/env python3
"""Remove only resources owned by the manifest's disposable sandbox stack."""

import argparse
import json
from pathlib import Path
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sandbox"))
from cloud import Aws, AwsError, owned_stack, save_manifest


def absent(aws, service, operation, markers, **arguments):
    try:
        aws.call(service, operation, **arguments)
        return False
    except AwsError as error:
        if any(marker in str(error) for marker in markers):
            return True
        raise


def stop_queries(aws, workgroup):
    token, pending = None, set()
    while True:
        arguments = {"WorkGroup": workgroup, "MaxResults": 50}
        if token:
            arguments["NextToken"] = token
        page = aws.call("athena", "list-query-executions", **arguments)
        for query_id in page.get("QueryExecutionIds", []):
            state = aws.call("athena", "get-query-execution", QueryExecutionId=query_id)["QueryExecution"]["Status"]["State"]
            if state in ("QUEUED", "RUNNING"):
                aws.call("athena", "stop-query-execution", QueryExecutionId=query_id)
                pending.add(query_id)
        token = page.get("NextToken")
        if not token:
            break
    deadline = time.monotonic() + 120
    while pending and time.monotonic() < deadline:
        for query_id in list(pending):
            state = aws.call("athena", "get-query-execution", QueryExecutionId=query_id)["QueryExecution"]["Status"]["State"]
            if state not in ("QUEUED", "RUNNING"):
                pending.remove(query_id)
        if pending:
            time.sleep(2)
    if pending:
        raise RuntimeError("Athena queries are still active. Cleanup state retained.")


def drain_bucket(aws, bucket):
    if absent(aws, "s3api", "head-bucket", ["(404)", "NoSuchBucket", "Not Found"], Bucket=bucket):
        return
    while True:
        versions = aws.call("s3api", "list-object-versions", Bucket=bucket, MaxKeys=1000)
        objects = [{"Key": item["Key"], "VersionId": item["VersionId"]} for name in ("Versions", "DeleteMarkers") for item in versions.get(name, [])]
        if not objects:
            break
        response = aws.call("s3api", "delete-objects", Bucket=bucket, Delete={"Objects": objects, "Quiet": True})
        if response.get("Errors"):
            raise RuntimeError("S3 could not delete every sandbox object.")
    while True:
        listing = aws.call("s3api", "list-multipart-uploads", Bucket=bucket, MaxUploads=1000)
        uploads = listing.get("Uploads", [])
        if not uploads:
            break
        for upload in uploads:
            aws.call("s3api", "abort-multipart-upload", Bucket=bucket, Key=upload["Key"], UploadId=upload["UploadId"])
    if aws.call("s3api", "list-objects-v2", Bucket=bucket, MaxKeys=1).get("KeyCount", 0):
        raise RuntimeError("Sandbox bucket is not empty.")


def verify_resources(aws, resources):
    checks = {
        "AWS::S3::Bucket": ("s3api", "head-bucket", ["(404)", "NoSuchBucket", "Not Found"], "Bucket"),
        "AWS::Lambda::Function": ("lambda", "get-function", ["ResourceNotFoundException"], "FunctionName"),
        "AWS::Glue::Database": ("glue", "get-database", ["EntityNotFoundException"], "Name"),
        "AWS::Athena::WorkGroup": ("athena", "get-work-group", ["is not found", "does not exist", "not found"], "WorkGroup"),
        "AWS::IAM::Role": ("iam", "get-role", ["NoSuchEntity"], "RoleName"),
    }
    verified, remaining = [], []
    for resource in resources:
        kind, identity = resource["ResourceType"], resource.get("PhysicalResourceId")
        if not identity:
            continue
        if kind in checks:
            service, operation, markers, parameter = checks[kind]
            missing = absent(aws, service, operation, markers, **{parameter: identity})
        elif kind == "AWS::Logs::LogGroup":
            groups = aws.call("logs", "describe-log-groups", logGroupNamePrefix=identity).get("logGroups", [])
            missing = all(group["logGroupName"] != identity for group in groups)
        else:
            continue
        (verified if missing else remaining).append(identity)
    if remaining:
        raise RuntimeError("Resources remain after stack deletion: " + ", ".join(remaining))
    return verified


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", default=".scratch/cloud-lab/manifest.json")
    parser.add_argument("--profile")
    args = parser.parse_args()
    path = Path(args.manifest).resolve()
    manifest = json.loads(path.read_text())
    if manifest.get("version") != 1 or not manifest["stack_name"].startswith("athena-bridge-lab-"):
        raise RuntimeError("Not an Athena Bridge sandbox manifest.")
    aws = Aws(args.profile or manifest["profile"], manifest["region"], manifest["account"])
    aws.verify_account()
    stack = owned_stack(aws, manifest)
    if stack and not manifest.get("resources"):
        manifest["resources"] = aws.call("cloudformation", "list-stack-resources", StackName=stack["StackId"])["StackResourceSummaries"]
        save_manifest(path, manifest)
    if manifest.get("stack_id") and not manifest.get("resources"):
        raise RuntimeError("No resource inventory is available. Refusing to claim verified cleanup.")
    if stack and stack["StackStatus"] != "DELETE_COMPLETE":
        manifest["stack_id"] = stack["StackId"]
        if stack["StackStatus"].endswith("IN_PROGRESS") and stack["StackStatus"] != "DELETE_IN_PROGRESS":
            raise RuntimeError("Wait for the current stack operation to settle before cleanup.")
        if stack["StackStatus"] != "DELETE_IN_PROGRESS":
            resources = aws.call("cloudformation", "list-stack-resources", StackName=stack["StackId"])["StackResourceSummaries"]
            manifest["resources"] = resources
            manifest["phase"] = "cleaning"
            save_manifest(path, manifest)
            kinds = {r["ResourceType"]: r["PhysicalResourceId"] for r in resources if r.get("PhysicalResourceId")}
            function = kinds.get("AWS::Lambda::Function")
            if function and not absent(aws, "lambda", "get-function", ["ResourceNotFoundException"], FunctionName=function):
                aws.call("lambda", "put-function-concurrency", FunctionName=function, ReservedConcurrentExecutions=0)
                print("Worker disabled. Waiting for in-flight invocations.", flush=True)
                time.sleep(30)
            workgroup = kinds.get("AWS::Athena::WorkGroup")
            if workgroup and not absent(aws, "athena", "get-work-group", ["is not found", "does not exist", "not found"], WorkGroup=workgroup):
                stop_queries(aws, workgroup)
            if kinds.get("AWS::S3::Bucket"):
                drain_bucket(aws, kinds["AWS::S3::Bucket"])
            aws.call("cloudformation", "delete-stack", StackName=stack["StackId"])
        aws.wait_stack(stack["StackId"], "DELETE_COMPLETE")
    manifest["verified_absent"] = verify_resources(aws, manifest.get("resources", []))
    manifest["phase"] = "cleaned"
    save_manifest(path, manifest)
    print(json.dumps({"cleanup": "verified", "stack": manifest["stack_name"], "verified_absent": manifest["verified_absent"]}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
