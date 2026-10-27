import json
import os
from pathlib import Path
import subprocess
import tempfile
import time


class AwsError(RuntimeError):
    pass


class Aws:
    def __init__(self, profile, region, account):
        self.profile, self.region, self.account = profile, region, account

    def call(self, service, operation, **arguments):
        environment = dict(os.environ, AWS_PAGER="", AWS_CLI_AUTO_PROMPT="off")
        body = arguments.pop("Body", None) if (service, operation) == ("s3api", "put-object") else None
        with tempfile.NamedTemporaryFile("w", prefix="athena-bridge-", suffix=".json") as request:
            json.dump(arguments, request)
            request.flush()
            command = ["aws", "--profile", self.profile, "--region", self.region, "--output", "json", "--no-cli-pager", "--no-paginate", service, operation, "--cli-input-json", "file://" + request.name]
            if body is not None:
                command += ["--body", body]
            response = subprocess.run(command, capture_output=True, text=True, env=environment, timeout=120)
        if response.returncode:
            raise AwsError(f"{service} {operation}: {response.stderr.strip()}")
        return json.loads(response.stdout) if response.stdout.strip() else {}

    def verify_account(self):
        identity = self.call("sts", "get-caller-identity")
        if identity["Account"] != self.account:
            raise RuntimeError("AWS account does not match the explicitly selected sandbox account.")
        return identity

    def stack(self, identity):
        try:
            return self.call("cloudformation", "describe-stacks", StackName=identity)["Stacks"][0]
        except AwsError as error:
            if "does not exist" in str(error):
                return None
            raise

    def wait_stack(self, identity, desired, timeout=900):
        deadline = time.monotonic() + timeout
        started = time.monotonic()
        deleting = desired == "DELETE_COMPLETE"
        saw_deleting = False
        previous = None
        while time.monotonic() < deadline:
            stack = self.stack(identity)
            status = stack["StackStatus"] if stack else "DELETE_COMPLETE"
            if status != previous:
                print(f"Stack status {status}", flush=True)
                previous = status
            if status == desired:
                return stack
            saw_deleting = saw_deleting or status == "DELETE_IN_PROGRESS"
            failure = (status == "DELETE_FAILED" and (saw_deleting or time.monotonic() - started >= 15)) if deleting else (
                status.endswith("FAILED") or status.startswith("DELETE_") or ("ROLLBACK" in status and "IN_PROGRESS" not in status))
            if failure:
                events = self.call("cloudformation", "describe-stack-events", StackName=identity)["StackEvents"]
                reasons = [e.get("ResourceStatusReason", "") for e in events if "FAILED" in e.get("ResourceStatus", "")]
                raise RuntimeError("Stack failed. " + " ".join(reasons[:5]))
            time.sleep(3)
        raise TimeoutError("Stack operation is still pending. Recovery state has been retained.")


def save_manifest(path, manifest):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as handle:
            json.dump(manifest, handle, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def owned_stack(aws, manifest):
    stack = aws.stack(manifest.get("stack_id") or manifest["stack_name"])
    if stack is None:
        return None
    tags = {tag["Key"]: tag["Value"] for tag in stack.get("Tags", [])}
    if tags.get("AthenaBridgeSandbox") != manifest["sandbox_id"] or stack["StackName"] != manifest["stack_name"]:
        raise RuntimeError("Stack ownership does not match the sandbox manifest.")
    return stack
