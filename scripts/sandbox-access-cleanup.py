#!/usr/bin/env python3
"""Remove administrator-created sandbox policies after verified resource cleanup."""

import argparse
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sandbox"))
from cloud import Aws, AwsError


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True, help="Administrator profile in the sandbox account")
    parser.add_argument("--manifest", default=".scratch/cloud-lab/manifest.json")
    parser.add_argument("--access-manifest", default=str(Path(__file__).resolve().parents[1] / ".scratch/cloud-lab/access-manifest.json"))
    parser.add_argument("--user", required=True, help="IAM user recorded in the access manifest")
    args = parser.parse_args()
    manifest = json.loads(Path(args.manifest).read_text())
    if manifest.get("phase") != "cleaned":
        raise RuntimeError("Verify sandbox resource cleanup before removing its access grants.")
    access = json.loads(Path(args.access_manifest).read_text())
    if (access["account"], access["user"]) != (manifest["account"], args.user):
        raise RuntimeError("Access ownership does not match the sandbox account and user.")
    owned = set(access.get("created_arns", []))
    aws = Aws(args.profile, manifest["region"], manifest["account"])
    aws.verify_account()
    for name in ("athena-bridge-lab-provisioner", "athena-bridge-lab-worker-boundary"):
        policy_arn = f"arn:aws:iam::{manifest['account']}:policy/{name}"
        if policy_arn not in owned:
            print("Retained policy not created by this run: " + name)
            continue
        try:
            policy = aws.call("iam", "get-policy", PolicyArn=policy_arn)["Policy"]
        except AwsError as error:
            if "NoSuchEntity" in str(error):
                continue
            raise
        versions = aws.call("iam", "list-policy-versions", PolicyArn=policy_arn)["Versions"]
        if len(versions) != 1:
            raise RuntimeError("Policy has been modified. Review its versions before removal. No attachment was changed.")
        if name.endswith("provisioner"):
            entities = aws.call("iam", "list-entities-for-policy", PolicyArn=policy_arn, PolicyUsageFilter="PermissionsPolicy")
            users = entities.get("PolicyUsers", [])
            if entities.get("PolicyRoles") or entities.get("PolicyGroups") or any(user["UserName"] != args.user for user in users):
                raise RuntimeError("Sandbox policy is used by another principal. Refusing to remove it.")
            if users:
                aws.call("iam", "detach-user-policy", UserName=args.user, PolicyArn=policy_arn)
        elif policy.get("PermissionsBoundaryUsageCount", 0) or policy.get("AttachmentCount", 0):
            raise RuntimeError("Worker boundary is still in use. Refusing to remove it.")
        aws.call("iam", "delete-policy", PolicyArn=policy_arn)
        print("Removed " + name)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
