from pathlib import Path

from fixtures import SCHEMAS, TABLE_COMMENTS


MAX_SCAN_BYTES = 10485760
RESULT_RETENTION_DAYS = 7


def template(name):
    database = name.replace("-", "_")
    sub = lambda value: {"Fn::Sub": value}
    ref = lambda value: {"Ref": value}
    resources = {
        "Bucket": {"Type": "AWS::S3::Bucket", "Properties": {
            "PublicAccessBlockConfiguration": {key: True for key in ["BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"]},
            "BucketEncryption": {"ServerSideEncryptionConfiguration": [{"ServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}}]},
            "LifecycleConfiguration": {"Rules": [{"Id": "ExpireSandboxData", "Status": "Enabled", "ExpirationInDays": RESULT_RETENTION_DAYS, "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}}]},
        }},
        "Database": {"Type": "AWS::Glue::Database", "Properties": {"CatalogId": ref("AWS::AccountId"), "DatabaseInput": {"Name": database, "Description": "Disposable synthetic DEMO data. Not production data."}}},
        "Workgroup": {"Type": "AWS::Athena::WorkGroup", "Properties": {
            "Name": name, "RecursiveDeleteOption": True,
            "Description": "Disposable synthetic Athena Bridge evaluation",
            "WorkGroupConfiguration": {"EnforceWorkGroupConfiguration": True, "BytesScannedCutoffPerQuery": MAX_SCAN_BYTES,
                "PublishCloudWatchMetricsEnabled": False, "EngineVersion": {"SelectedEngineVersion": "Athena engine version 3"},
                "ResultConfiguration": {"OutputLocation": sub("s3://${Bucket}/results/"), "EncryptionConfiguration": {"EncryptionOption": "SSE_S3"}}},
        }},
        "WorkerRole": {"Type": "AWS::IAM::Role", "Properties": {
            "RoleName": name + "-worker-role",
            "PermissionsBoundary": sub("arn:${AWS::Partition}:iam::${AWS::AccountId}:policy/athena-bridge-lab-worker-boundary"),
            "AssumeRolePolicyDocument": {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"Service": "lambda.amazonaws.com"}, "Action": "sts:AssumeRole"}]},
            "Policies": [{"PolicyName": "SyntheticSandboxOnly", "PolicyDocument": {"Version": "2012-10-17", "Statement": [
                {"Effect": "Allow", "Action": ["athena:StartQueryExecution", "athena:GetQueryExecution", "athena:GetQueryResults", "athena:StopQueryExecution", "athena:GetWorkGroup"], "Resource": sub("arn:${AWS::Partition}:athena:${AWS::Region}:${AWS::AccountId}:workgroup/${Workgroup}")},
                {"Effect": "Allow", "Action": ["glue:GetDatabase", "glue:GetTable", "glue:GetTables", "glue:GetPartitions"], "Resource": [sub("arn:${AWS::Partition}:glue:${AWS::Region}:${AWS::AccountId}:catalog"), sub(f"arn:${{AWS::Partition}}:glue:${{AWS::Region}}:${{AWS::AccountId}}:database/{database}"), sub(f"arn:${{AWS::Partition}}:glue:${{AWS::Region}}:${{AWS::AccountId}}:table/{database}/*")]},
                {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"], "Resource": sub("arn:${AWS::Partition}:s3:::${Bucket}")},
                {"Effect": "Allow", "Action": ["s3:GetObject"], "Resource": sub("arn:${AWS::Partition}:s3:::${Bucket}/data/*")},
                {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"], "Resource": sub("arn:${AWS::Partition}:s3:::${Bucket}/results/*")},
                {"Effect": "Allow", "Action": ["logs:CreateLogStream", "logs:PutLogEvents"], "Resource": sub(f"arn:${{AWS::Partition}}:logs:${{AWS::Region}}:${{AWS::AccountId}}:log-group:/aws/lambda/{name}-worker:*")},
            ]}}],
        }},
        "Worker": {"Type": "AWS::Lambda::Function", "Properties": {
            "FunctionName": name + "-worker", "Runtime": "python3.13", "Handler": "index.handler", "Timeout": 25, "MemorySize": 128,
            "Role": {"Fn::GetAtt": ["WorkerRole", "Arn"]}, "Code": {"ZipFile": Path(__file__).with_name("worker.py").read_text()},
            "Environment": {"Variables": {"WORKER_ID": name + "-worker", "DATABASE": database, "WORKGROUP": ref("Workgroup"), "BUCKET": ref("Bucket"), "CURSOR_KEY": ref("CursorKey"),
                "MAX_SCAN_BYTES": str(MAX_SCAN_BYTES), "RESULT_RETENTION_DAYS": str(RESULT_RETENTION_DAYS)}},
        }},
        "WorkerLogs": {"Type": "AWS::Logs::LogGroup", "Properties": {"LogGroupName": sub("/aws/lambda/${Worker}"), "RetentionInDays": 1}},
    }
    for index, (table, columns) in enumerate(SCHEMAS.items()):
        resources[f"Table{index}"] = {"Type": "AWS::Glue::Table", "Properties": {
            "CatalogId": ref("AWS::AccountId"), "DatabaseName": ref("Database"),
            "TableInput": {"Name": table, "Description": TABLE_COMMENTS[table], "TableType": "EXTERNAL_TABLE", "Parameters": {"classification": "json", "EXTERNAL": "TRUE"},
                "StorageDescriptor": {"Columns": [{"Name": field, "Type": kind, "Comment": comment} for field, kind, comment in columns],
                    "Location": sub(f"s3://${{Bucket}}/data/{table}/"), "InputFormat": "org.apache.hadoop.mapred.TextInputFormat",
                    "OutputFormat": "org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat", "SerdeInfo": {"SerializationLibrary": "org.openx.data.jsonserde.JsonSerDe"}}},
        }}
    return {"AWSTemplateFormatVersion": "2010-09-09", "Description": "Disposable synthetic Athena Bridge sandbox with complete stack ownership.",
        "Parameters": {"CursorKey": {"Type": "String", "NoEcho": True, "MinLength": 64}}, "Resources": resources,
        "Outputs": {"Bucket": {"Value": ref("Bucket")}, "Database": {"Value": database}, "Workgroup": {"Value": ref("Workgroup")}, "Function": {"Value": ref("Worker")}}}
