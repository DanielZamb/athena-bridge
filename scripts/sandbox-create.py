#!/usr/bin/env python3
"""Create or resume a disposable real AWS Athena Bridge sandbox."""

import argparse
import hashlib
import json
from pathlib import Path
import secrets
import shlex
import sys
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sandbox"))
from cloud import Aws, owned_stack, save_manifest
from fixtures import write
from stack import template


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--account", required=True)
    parser.add_argument("--region", default="us-east-1")
    parser.add_argument("--manifest", default=".scratch/cloud-lab/manifest.json")
    args = parser.parse_args()
    aws = Aws(args.profile, args.region, args.account)
    aws.verify_account()
    path = Path(args.manifest).resolve()
    if path.exists():
        manifest = json.loads(path.read_text())
        if (manifest["account"], manifest["region"]) != (args.account, args.region) or manifest.get("phase") in ("cleaning", "cleaned"):
            raise RuntimeError("Use a fresh manifest for a different or already cleaned sandbox.")
    else:
        sandbox_id = uuid.uuid4().hex[:12]
        manifest = {"version": 1, "sandbox_id": sandbox_id, "account": args.account, "region": args.region, "profile": args.profile,
                    "stack_name": "athena-bridge-lab-" + sandbox_id, "phase": "intent"}
        save_manifest(path, manifest)
    manifest["profile"] = args.profile
    stack = owned_stack(aws, manifest)
    body = json.dumps(template(manifest["stack_name"]), sort_keys=True)
    desired = "CREATE_COMPLETE"
    if stack is None:
        print("Creating stack " + manifest["stack_name"], flush=True)
        response = aws.call("cloudformation", "create-stack", StackName=manifest["stack_name"],
            TemplateBody=body, Capabilities=["CAPABILITY_NAMED_IAM"],
            Parameters=[{"ParameterKey": "CursorKey", "ParameterValue": secrets.token_hex(32)}],
            Tags=[{"Key": "AthenaBridgeSandbox", "Value": manifest["sandbox_id"]}, {"Key": "Purpose", "Value": "synthetic-cli-evaluation"}])
        manifest.update(stack_id=response["StackId"], phase="creating")
        save_manifest(path, manifest)
    else:
        manifest["stack_id"] = stack["StackId"]
        save_manifest(path, manifest)
        if stack["StackStatus"].startswith("DELETE_"):
            raise RuntimeError("The sandbox is being or has been deleted. Use a fresh manifest.")
        if stack["StackStatus"].endswith("IN_PROGRESS"):
            desired = "UPDATE_COMPLETE" if stack["StackStatus"].startswith("UPDATE_") else "CREATE_COMPLETE"
            stack = aws.wait_stack(stack["StackId"], desired)
        if stack["StackStatus"] not in ("CREATE_COMPLETE", "UPDATE_COMPLETE"):
            raise RuntimeError("Clean up the failed stack before creating a fresh sandbox.")
        current = aws.call("cloudformation", "get-template", StackName=stack["StackId"], TemplateStage="Original")["TemplateBody"]
        current = json.loads(current) if isinstance(current, str) else current
        if current != json.loads(body):
            aws.call("cloudformation", "update-stack", StackName=stack["StackId"], TemplateBody=body, Capabilities=["CAPABILITY_NAMED_IAM"],
                Parameters=[{"ParameterKey": "CursorKey", "UsePreviousValue": True}])
            desired = "UPDATE_COMPLETE"
        else:
            desired = stack["StackStatus"]
    stack = aws.wait_stack(manifest["stack_id"], desired)
    manifest["template_hash"] = hashlib.sha256(body.encode()).hexdigest()
    manifest["outputs"] = {entry["OutputKey"]: entry["OutputValue"] for entry in stack["Outputs"]}
    manifest["phase"] = "seeding"
    save_manifest(path, manifest)
    directory = path.parent / "fixtures"
    write(directory)
    manifest["fixture_hashes"] = {}
    for fixture in sorted(directory.glob("*.jsonl")):
        aws.call("s3api", "put-object", Bucket=manifest["outputs"]["Bucket"], Key=f"data/{fixture.stem}/rows.jsonl", Body=str(fixture), ContentType="application/x-ndjson")
        manifest["fixture_hashes"][fixture.stem] = hashlib.sha256(fixture.read_bytes()).hexdigest()
    manifest["phase"] = "ready"
    save_manifest(path, manifest)
    connection = path.parent / "connection.sh"
    values = {"AWS_PROFILE": args.profile, "AWS_REGION": args.region, "ATHENA_BRIDGE_FUNCTION": manifest["outputs"]["Function"], "ATHENA_BRIDGE_DATABASE": manifest["outputs"]["Database"]}
    connection.write_text("".join(f"export {key}={shlex.quote(value)}\n" for key, value in values.items()))
    connection.chmod(0o600)
    print(json.dumps({"manifest": str(path), "connection": str(connection), **manifest["outputs"]}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
