"""Generate account-scoped provisioner permissions and the worker boundary."""

import argparse
import json
from pathlib import Path


def policies(account, region):
    prefix = "athena-bridge-lab-"
    arn = lambda service, resource: f"arn:aws:{service}:{region}:{account}:{resource}"
    role = f"arn:aws:iam::{account}:role/{prefix}*"
    boundary = f"arn:aws:iam::{account}:policy/athena-bridge-lab-worker-boundary"
    bucket = f"arn:aws:s3:::{prefix}*"
    catalog = arn("glue", "catalog")
    database = arn("glue", "database/athena_bridge_lab_*")
    table = arn("glue", "table/athena_bridge_lab_*/*")
    workgroup = arn("athena", "workgroup/" + prefix + "*")
    logs = arn("logs", "log-group:/aws/lambda/" + prefix + "*")
    allow = lambda actions, resources, **extra: {"Effect": "Allow", "Action": actions, "Resource": resources, **extra}
    glue_reads = ["glue:GetDatabase", "glue:GetDatabases", "glue:GetTable", "glue:GetTables", "glue:GetPartition", "glue:GetPartitions", "glue:BatchGetPartition"]
    worker = {"Version": "2012-10-17", "Statement": [
        allow(["athena:StartQueryExecution", "athena:GetQueryExecution", "athena:GetQueryResults", "athena:StopQueryExecution", "athena:GetWorkGroup"], workgroup),
        allow(["glue:GetDatabase", "glue:GetTable", "glue:GetTables", "glue:GetPartitions"], [catalog, database, table]),
        allow(["s3:ListBucket", "s3:GetBucketLocation"], bucket),
        allow(["s3:GetObject"], bucket + "/data/*"),
        allow(["s3:GetObject", "s3:PutObject"], bucket + "/results/*"),
        allow(["logs:CreateLogStream", "logs:PutLogEvents"], logs + ":*"),
    ]}
    provisioner = {"Version": "2012-10-17", "Statement": [
        allow(["cloudformation:CreateStack", "cloudformation:UpdateStack", "cloudformation:DescribeStacks", "cloudformation:DescribeStackEvents", "cloudformation:ListStackResources", "cloudformation:DeleteStack", "cloudformation:GetTemplate"], arn("cloudformation", "stack/" + prefix + "*/*")),
        allow(["cloudformation:ValidateTemplate", "cloudformation:GetTemplateSummary"], "*"),
        allow(["iam:CreateRole"], role, Condition={"StringEquals": {"iam:PermissionsBoundary": boundary}}),
        allow(["iam:GetRole", "iam:DeleteRole", "iam:PutRolePolicy", "iam:DeleteRolePolicy", "iam:GetRolePolicy", "iam:ListRolePolicies", "iam:ListAttachedRolePolicies", "iam:TagRole", "iam:UntagRole"], role),
        allow(["iam:PassRole"], role, Condition={"StringEquals": {"iam:PassedToService": "lambda.amazonaws.com"}}),
        {"Effect": "Deny", "Action": ["iam:PutRolePermissionsBoundary", "iam:DeleteRolePermissionsBoundary"], "Resource": role},
        allow(["lambda:CreateFunction", "lambda:DeleteFunction", "lambda:GetFunction", "lambda:GetFunctionConfiguration", "lambda:UpdateFunctionCode", "lambda:UpdateFunctionConfiguration", "lambda:InvokeFunction", "lambda:PutFunctionConcurrency", "lambda:DeleteFunctionConcurrency", "lambda:GetFunctionConcurrency", "lambda:TagResource", "lambda:UntagResource", "lambda:ListTags"], arn("lambda", "function:" + prefix + "*")),
        allow(["s3:CreateBucket", "s3:DeleteBucket", "s3:GetBucketLocation", "s3:ListBucket", "s3:ListBucketVersions", "s3:ListBucketMultipartUploads", "s3:GetBucketPublicAccessBlock", "s3:PutBucketPublicAccessBlock", "s3:GetEncryptionConfiguration", "s3:PutEncryptionConfiguration", "s3:GetLifecycleConfiguration", "s3:PutLifecycleConfiguration", "s3:GetBucketTagging", "s3:PutBucketTagging", "s3:GetBucketVersioning"], bucket),
        allow(["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"], bucket + "/*"),
        allow(glue_reads + ["glue:CreateDatabase", "glue:DeleteDatabase", "glue:CreateTable", "glue:DeleteTable", "glue:UpdateTable", "glue:UpdateDatabase"], [catalog, database, table, arn("glue", "userDefinedFunction/athena_bridge_lab_*/*")]),
        allow(["athena:CreateWorkGroup", "athena:DeleteWorkGroup", "athena:GetWorkGroup", "athena:UpdateWorkGroup", "athena:ListQueryExecutions", "athena:GetQueryExecution", "athena:StopQueryExecution", "athena:TagResource", "athena:UntagResource", "athena:ListTagsForResource"], workgroup),
        allow(["logs:CreateLogGroup", "logs:DeleteLogGroup", "logs:PutRetentionPolicy", "logs:DeleteRetentionPolicy", "logs:TagLogGroup", "logs:UntagLogGroup", "logs:TagResource", "logs:UntagResource", "logs:ListTagsForResource"], [logs, logs + ":*"]),
        allow(["logs:DescribeLogGroups"], "*"),
    ]}
    return {"worker-boundary.json": worker, "provisioner-policy.json": provisioner}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--account", required=True)
    parser.add_argument("--region", default="us-east-1")
    parser.add_argument("--out", default="sandbox/iam")
    args = parser.parse_args()
    directory = Path(args.out)
    directory.mkdir(parents=True, exist_ok=True)
    for name, policy in policies(args.account, args.region).items():
        (directory / name).write_text(json.dumps(policy, indent=2) + "\n")
