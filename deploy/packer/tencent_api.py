"""Tencent JSON API client using TC3-HMAC-SHA256 and bounded HTTPS requests."""

from datetime import datetime, timezone
import hashlib
import hmac
import json
import os
import time
import urllib.request


class TencentError(RuntimeError):
    def __init__(self, action, error, request_id):
        self.code = error["Code"]
        super().__init__(f"{action}: {self.code}: {error.get('Message', '')} (request {request_id})")


class TencentAPI:
    def __init__(self):
        self.secret_id = os.environ["TENCENTCLOUD_SECRET_ID"]
        self.secret_key = os.environ["TENCENTCLOUD_SECRET_KEY"]
        self.token = os.environ.get("TENCENTCLOUD_SECURITY_TOKEN", "")

    def call(self, action, params, region="", service="cvm", version="2017-03-12"):
        timestamp = int(time.time())
        date = datetime.fromtimestamp(timestamp, timezone.utc).strftime("%Y-%m-%d")
        host = f"{service}.tencentcloudapi.com"
        payload = json.dumps(params, separators=(",", ":")).encode()
        content_type = "application/json; charset=utf-8"
        headers = f"content-type:{content_type}\nhost:{host}\nx-tc-action:{action.lower()}\n"
        signed = "content-type;host;x-tc-action"
        digest = lambda value: hashlib.sha256(value).hexdigest()
        canonical = f"POST\n/\n\n{headers}\n{signed}\n{digest(payload)}"
        scope = f"{date}/{service}/tc3_request"
        to_sign = f"TC3-HMAC-SHA256\n{timestamp}\n{scope}\n{digest(canonical.encode())}"
        sign = lambda key, value: hmac.new(key, value.encode(), hashlib.sha256).digest()
        key = sign(sign(sign(("TC3" + self.secret_key).encode(), date), service), "tc3_request")
        signature = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
        authorization = f"TC3-HMAC-SHA256 Credential={self.secret_id}/{scope}, SignedHeaders={signed}, Signature={signature}"
        headers = {"Content-Type": content_type, "Host": host, "X-TC-Action": action,
                   "X-TC-Version": version, "X-TC-Timestamp": str(timestamp), "Authorization": authorization}
        if region:
            headers["X-TC-Region"] = region
        if self.token:
            headers["X-TC-Token"] = self.token
        request = urllib.request.Request(f"https://{host}/", payload, headers, method="POST")
        with urllib.request.urlopen(request, timeout=60) as response:
            result = json.load(response)["Response"]
        if "Error" in result:
            raise TencentError(action, result["Error"], result.get("RequestId", "unknown"))
        return result

    def pages(self, action, params, region, field):
        offset = 0
        while True:
            response = self.call(action, dict(params, Offset=offset, Limit=100), region)
            entries = response[field]
            yield from entries
            offset += len(entries)
            if offset >= response["TotalCount"]:
                return
            if not entries:
                raise RuntimeError(f"{action} returned an empty page before TotalCount")
