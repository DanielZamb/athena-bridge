# Download native result files safely

Request a fresh download URL and save the result bytes unchanged under the CLI download policy.

## Sub-features

- `downloads-1` Untrusted TLS, HTTP URLs, unsupported formats and declared size above the ceiling fail without publication.
- `downloads-2` On Linux, successful local HTTPS keeps exact bytes and private permissions.
- `downloads-3` On Linux, redirects, expired links, truncated streams, mismatched metadata and actual-byte ceilings fail.
- `downloads-4` On Linux, the download server observes no AWS authorization or session token.

## How to get to it (user POV)

`download --query-id local-query --out PATH --format json` with optional `--max-bytes 4`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature downloads
```

Creates a run-owned TLS certificate and local HTTPS server. Linux uses SSL_CERT_FILE for that certificate and checks the complete matrix. Native macOS/Windows uses system trust and verifies TLS rejection plus pre-network protocol/size validation; summary explicitly marks the remaining cases pending. All failures leave no destination or temporary sibling.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

No system certificate trust is changed. Go native macOS/Windows does not use this temporary PEM trust path. Run this recipe on Linux for successful HTTPS and subsequent HTTP response checks. No credential-forwarding claim is made when TLS fails before HTTP.
