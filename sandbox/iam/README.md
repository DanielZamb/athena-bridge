# Grant temporary sandbox access

The checked-in policies use the fictional account `123456789012` and region `us-east-1`. They permit resources whose names start with `athena-bridge-lab-` and Glue databases whose names start with `athena_bridge_lab_`. The deployment role must carry the supplied worker boundary. Its inline policy narrows access further to the current stack.

These policies cover a disposable synthetic lab. They do not establish access to production data, Lake Formation grants, encryption keys, or organization controls. Ordinary CLI callers need `lambda:InvokeFunction` for the selected worker or alias. The worker execution role separately owns Athena, Glue, and S3 permissions.

The worker boundary permits Athena start, status, results, cancellation, and workgroup reads; Glue metadata reads; synthetic source reads; result reads and writes; and log writes. Result identity uses S3 object revision metadata through `HeadObject`, authorized by `s3:GetObject`. Metadata search uses bounded Glue reads. `worker_info` reports explicit configuration and makes zero IAM calls.

## Generate and review account-specific policies

These steps contact AWS and can create billable resources. Run them only when you intend to create a live sandbox. Use an administrator AWS profile and an existing IAM user dedicated to sandbox provisioning.

From the repository root, generate the policies for your account:

```sh
python3 sandbox/iam.py --account YOUR_ACCOUNT_ID --region us-east-1
```

Review both JSON files before applying them. Keep the account-specific versions out of public commits. The access helper requires the reviewed files to match its generated permissions for the selected account and region.

```sh
python3 scripts/sandbox-access.py \
  --profile admin --account YOUR_ACCOUNT_ID --region us-east-1 --user YOUR_SANDBOX_USER
```

The helper checks the selected account before writing. Existing policies must match the reviewed files. The helper records policies it creates in `.scratch/cloud-lab/access-manifest.json`. It does not replace a different policy unless you explicitly request an update to one owned by that manifest.

Configure an AWS profile named `sandbox` for the provisioning user, then create a sandbox:

```sh
python3 scripts/sandbox-create.py \
  --profile sandbox --account YOUR_ACCOUNT_ID --region us-east-1 \
  --manifest .scratch/my-new-lab/manifest.json
```

## Remove temporary access

First remove the sandbox's data and resources with the provisioning profile:

```sh
python3 scripts/sandbox-cleanup.py --profile sandbox \
  --manifest .scratch/my-new-lab/manifest.json
```

After cleanup reports verified removal, remove the temporary IAM grants with the administrator profile:

```sh
python3 scripts/sandbox-access-cleanup.py --profile admin --user YOUR_SANDBOX_USER \
  --manifest .scratch/my-new-lab/manifest.json
```

The removal helper retains policies absent from the access manifest's `created_arns`. It refuses to remove policies used by another principal or a boundary still in use. Retain the access manifest until cleanup completes.

CloudFormation stack records, Athena execution metadata, CloudTrail history, and billing records follow AWS retention. Resource cleanup removes sandbox data, result objects, the bucket, Glue resources, workgroup, Lambda, execution role, and log group.
