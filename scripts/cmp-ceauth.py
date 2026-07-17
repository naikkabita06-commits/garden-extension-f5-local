#!/usr/bin/env python3
import os
import hmac
import hashlib
import time


def generate_ce_auth(api_key_id: str, secret_key: str, expiry_time: int = 299) -> str:
    current_timestamp = int(time.time())
    expiry_timestamp = current_timestamp + expiry_time
    input_data = f"{api_key_id}.{expiry_timestamp}".encode()
    signature = hmac.new(secret_key.encode(), input_data, hashlib.sha256).hexdigest()
    return f"{api_key_id}.{expiry_timestamp}.{signature}"


def main() -> None:
    creds_file = os.environ.get("CMP_API_CREDENTIALS_FILE")
    if not creds_file:
        default_creds_file = os.path.expanduser("~/.cmp_api_creds")
        if os.path.exists(default_creds_file):
            creds_file = default_creds_file

    api_key_id = os.environ.get("CMP_API_KEY_ID")
    api_key_id_file = os.environ.get("CMP_API_KEY_ID_FILE")

    secret_key = os.environ.get("CMP_API_SECRET")
    secret_file = os.environ.get("CMP_API_SECRET_FILE")
    expiry = int(os.environ.get("CMP_CE_AUTH_EXPIRY", "299"))

    # Option 0: read both id + secret from a single credentials file.
    # Supported formats:
    #   - line1: <api-id>
    #     line2: <secret>
    #   - key=value lines: api_id=... and secret=...
    if creds_file:
        try:
            with open(creds_file, "r", encoding="utf-8") as f:
                lines = [ln.strip() for ln in f.read().splitlines() if ln.strip() and not ln.strip().startswith("#")]
        except OSError as e:
            raise SystemExit(f"failed to read CMP_API_CREDENTIALS_FILE={creds_file!r}: {e}")

        kv = {}
        for ln in lines:
            if "=" in ln:
                k, v = ln.split("=", 1)
                kv[k.strip().lower()] = v.strip()

        if "api_id" in kv and "secret" in kv:
            api_key_id = kv["api_id"]
            secret_key = kv["secret"]
        elif len(lines) >= 2 and all("=" not in ln for ln in lines[:2]):
            api_key_id = lines[0]
            secret_key = lines[1]
        else:
            raise SystemExit(
                "CMP_API_CREDENTIALS_FILE format invalid. Use either: "
                "(1) two lines: <api-id> then <secret>, or (2) api_id=... and secret=..."
            )

    # Option 1: read API id from a file (if not set via env/creds_file)
    if not api_key_id and api_key_id_file:
        try:
            with open(api_key_id_file, "r", encoding="utf-8") as f:
                api_key_id = f.read().strip()
        except OSError as e:
            raise SystemExit(f"failed to read CMP_API_KEY_ID_FILE={api_key_id_file!r}: {e}")

    if not api_key_id:
        raise SystemExit("CMP_API_KEY_ID is required (or set CMP_API_KEY_ID_FILE / CMP_API_CREDENTIALS_FILE)")

    # Prefer file-based secret to avoid placing secrets in env vars.
    if not secret_key and secret_file:
        try:
            with open(secret_file, "r", encoding="utf-8") as f:
                secret_key = f.read().strip()
        except OSError as e:
            raise SystemExit(f"failed to read CMP_API_SECRET_FILE={secret_file!r}: {e}")

    if not secret_key:
        raise SystemExit("CMP_API_SECRET is required (or set CMP_API_SECRET_FILE / CMP_API_CREDENTIALS_FILE)")

    # IMPORTANT: print ONLY the token (cmp-curl.sh expects stdout == token)
    print(generate_ce_auth(api_key_id, secret_key, expiry))


if __name__ == "__main__":
    main()
