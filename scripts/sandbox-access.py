#!/usr/bin/env python3
"""Apply the reviewed sandbox policies using an administrator AWS profile."""

import argparse
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "sandbox"))
from cloud import Aws, AwsError, save_manifest
from iam import policies


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--account", required=True)
    parser.add_argument("--region", default="us-east-1")
    parser.add_argument("--user", required=True, help="IAM user receiving sandbox provisioning access")
    parser.add_argument("--update-owned", action="store_true", help="Update only a policy created by this script's access manifest")
    args = parser.parse_args()
    aws = Aws(args.profile, args.region, args.account)
    aws.verify_account()
    path = ROOT / ".scratch/cloud-lab/access-manifest.json"
    manifest = json.loads(path.read_text()) if path.exists() else {"account": args.account, "region": args.region, "user": args.user, "created_arns": []}
    if (manifest["account"], manifest["user"]) != (args.account, args.user):
        raise RuntimeError("An access manifest already exists for another account or user.")
    save_manifest(path, manifest)
    for filename, document in policies(args.account, args.region).items():
        reviewed = json.loads((ROOT / "sandbox/iam" / filename).read_text())
        if reviewed != document:
            raise RuntimeError("Regenerate and review the IAM policies for the selected account and region first.")
        name = "athena-bridge-lab-" + ("worker-boundary" if filename == "worker-boundary.json" else "provisioner")
        policy_arn = f"arn:aws:iam::{args.account}:policy/{name}"
        try:
            policy = aws.call("iam", "get-policy", PolicyArn=policy_arn)["Policy"]
        except AwsError as error:
            if "NoSuchEntity" not in str(error):
                raise
            aws.call("iam", "create-policy", PolicyName=name, PolicyDocument=json.dumps(document),
                Tags=[{"Key": "Purpose", "Value": "athena-bridge-synthetic-evaluation"}])
            manifest["created_arns"].append(policy_arn)
            save_manifest(path, manifest)
            print("Created " + name, flush=True)
        else:
            current = aws.call("iam", "get-policy-version", PolicyArn=policy_arn, VersionId=policy["DefaultVersionId"])["PolicyVersion"]["Document"]
            if current != document:
                if not args.update_owned or policy_arn not in manifest["created_arns"]:
                    raise RuntimeError("Existing policy differs from the reviewed file. Use --update-owned only for this run's policies.")
                aws.call("iam", "create-policy-version", PolicyArn=policy_arn, PolicyDocument=json.dumps(document), SetAsDefault=True)
                versions = aws.call("iam", "list-policy-versions", PolicyArn=policy_arn)["Versions"]
                for version in versions:
                    if not version["IsDefaultVersion"]:
                        aws.call("iam", "delete-policy-version", PolicyArn=policy_arn, VersionId=version["VersionId"])
                print("Updated owned " + name, flush=True)
            print("Verified existing " + name, flush=True)
    aws.call("iam", "attach-user-policy", UserName=args.user, PolicyArn=f"arn:aws:iam::{args.account}:policy/athena-bridge-lab-provisioner")
    manifest["attached"] = True
    save_manifest(path, manifest)
    print("Scoped sandbox permissions attached to " + args.user, flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
