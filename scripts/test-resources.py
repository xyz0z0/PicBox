#!/usr/bin/env python3
"""Run the actual static binary in isolated 1-CPU, no-swap Docker containers.

Python stdlib only; Docker image must already exist. Never mounts project data.
The load generator runs outside the container. Linux local cgroup v2 peak/current
are used when accessible; otherwise report sampled Docker usage explicitly.

Coverage follows the current original-React UI build (static shell + JSON API):
static shell/assets and the public runtime config endpoint, upload bytes around the
16MP pixel limit, backpressure, usage/tags, batch visibility, export HEAD + a small
streamed ZIP, visibility, soft delete, private trash, restore bytes, permanent purge,
and the original change-password semantics (current session survives, other sessions
die, API keys survive). It does not run a browser and does not claim UI interaction.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import struct
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import zlib


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def png(width, height):
    def chunk(kind, data):
        return (struct.pack(">I", len(data)) + kind + data
                + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF))
    # An RGBA fixture with a large decoded allocation and a small wire size.
    row = b"\0" + bytes([44, 112, 168, 255]) * width
    encoder = zlib.compressobj(6)
    compressed = b"".join(encoder.compress(row) for _ in range(height)) + encoder.flush()
    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", compressed) + chunk(b"IEND", b""))


class Client:
    def __init__(self, base):
        self.base = base
        self.cookie = ""

    def request(self, path, method="GET", body=None, headers=None, auth=True):
        request_headers = dict(headers or {})
        if auth and self.cookie:
            request_headers["Cookie"] = self.cookie
        request = urllib.request.Request(self.base + path, data=body,
                                         headers=request_headers, method=method)
        try:
            response = urllib.request.urlopen(request, timeout=120)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def json(self, path, method="GET", value=None, **kwargs):
        payload = None if value is None else json.dumps(value).encode()
        status, headers, raw = self.request(path, method, payload,
                                            {"Content-Type": "application/json"}, **kwargs)
        return status, headers, json.loads(raw) if raw else None

    def upload(self, image, bearer=None):
        boundary = "picbox-" + secrets.token_hex(12)
        body = (f'--{boundary}\r\nContent-Disposition: form-data; name="tags"\r\n\r\n'
                '["resource-test"]\r\n'
                f'--{boundary}\r\nContent-Disposition: form-data; name="file"; '
                'filename="fixture.png"\r\nContent-Type: image/png\r\n\r\n').encode()
        body += image + f"\r\n--{boundary}--\r\n".encode()
        headers = {"Content-Type": f"multipart/form-data; boundary={boundary}"}
        if bearer:
            headers["Authorization"] = "Bearer " + bearer
        status, _, raw = self.request("/api/images", "POST", body, headers, auth=not bearer)
        return status, json.loads(raw)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def read_number(path):
    try:
        return int(path.read_text().strip())
    except (OSError, ValueError):
        return None


def local_cgroup(info):
    # PID/cgroup lookup is valid only if this Docker daemon is on this host.
    try:
        pid = info["State"]["Pid"]
        for row in Path(f"/proc/{pid}/cgroup").read_text().splitlines():
            if row.startswith("0::"):
                candidate = Path("/sys/fs/cgroup") / row[3:].lstrip("/")
                # Verify membership to avoid attributing an unrelated local PID.
                if info["Id"] in str(candidate) and (candidate / "memory.current").exists():
                    return candidate
    except OSError:
        pass
    return None


def check_static_shell(client):
    """The page is a static shell; assets are what the browser would load."""
    status, headers, body = client.request("/", auth=False)
    require(status == 200, f"static shell failed: {status}")
    require(headers["Content-Type"].startswith("text/html"), "static shell is not HTML")
    csp = headers.get("Content-Security-Policy", "")
    require("script-src 'self'" in csp, "CSP script-src changed")
    require("unsafe-eval" not in csp, "CSP allows unsafe-eval")
    require('id="tab-trash"' not in body.decode("utf-8", "replace"),
            "static shell still contains the removed simplified preview page")
    assets = sorted(set(re.findall(r'(?:src|href)="(/assets/[^"]+)"',
                                   body.decode("utf-8", "replace"))))
    require(assets, "static shell references no /assets/* bundle")
    kinds = set()
    for asset in assets:
        status, headers, payload = client.request(asset, auth=False)
        require(status == 200 and payload, f"asset {asset} not served")
        content_type = headers["Content-Type"]
        kinds.add("js" if asset.endswith(".js") else
                  "css" if asset.endswith(".css") else "other")
        require(content_type.split(";")[0] in
                ("text/javascript", "text/css", "image/svg+xml"),
                f"asset {asset} MIME changed: {content_type}")
    require({"js", "css"} <= kinds, "bundle is missing JS or CSS assets")
    status, _, _ = client.request("/api/does-not-exist", auth=False)
    require(status == 404, "unknown API did not return 404")
    # 上传上限由服务端运行时下发，前端不需要为此重建。
    status, _, config = client.json("/api/config")
    require(status == 200 and isinstance(config.get("uploadMaxBytes"), int)
            and config["uploadMaxBytes"] > 0, f"runtime config failed: {config}")
    return {"assets": len(assets), "csp": "script-src 'self'",
            "upload_max_bytes": config["uploadMaxBytes"]}


def run_case(image, memory, fixtures):
    name = f"picbox-test-{secrets.token_hex(5)}"
    with tempfile.TemporaryDirectory(prefix="picbox-resource-") as directory:
        # Bind only this fresh temporary directory; run as its owning UID/GID.
        os.chmod(directory, 0o700)
        container = None
        sampler = None
        stopped = threading.Event()
        samples = []
        cg = None
        started = time.monotonic()
        try:
            container = docker("create", "--name", name, "--cpus=1",
                               f"--memory={memory}m", f"--memory-swap={memory}m",
                               "--pids-limit=128", "--read-only", "--cap-drop=ALL",
                               "--security-opt=no-new-privileges:true",
                               "--user", f"{os.getuid()}:{os.getgid()}",
                               "--mount", f"type=bind,src={directory},dst=/data",
                               "-p", "127.0.0.1::3001", image)
            docker("start", container)
            info = json.loads(docker("inspect", container))[0]
            require(info["HostConfig"]["Memory"] == memory * 1024 * 1024, "memory limit absent")
            require(info["HostConfig"]["MemorySwap"] == memory * 1024 * 1024, "swap not disabled")
            port = info["NetworkSettings"]["Ports"]["3001/tcp"][0]["HostPort"]
            client = Client(f"http://127.0.0.1:{port}")
            for attempt in range(100):
                try:
                    if client.json("/api/health")[0] == 200:
                        break
                except (OSError, ValueError):
                    pass
                time.sleep(0.1)
            else:
                raise AssertionError("health check never became ready")
            startup_ms = round((time.monotonic() - started) * 1000)
            cg = local_cgroup(info)
            idle = read_number(cg / "memory.current") if cg else None

            def sample():
                while not stopped.wait(0.1):
                    if cg:
                        value = read_number(cg / "memory.current")
                        if value is not None:
                            samples.append(value)

            sampler = threading.Thread(target=sample, daemon=True)
            sampler.start()
            timings = []
            password = secrets.token_urlsafe(18)
            static_shell = check_static_shell(client)
            status, headers, _ = client.json("/api/auth/register", "POST",
                                             {"email": "resource@example.com", "password": password})
            require(status == 200, "first registration failed")
            client.cookie = headers["Set-Cookie"].split(";", 1)[0]
            require(client.json("/api/auth/me")[0] == 200, "me failed")
            status, _, created = client.json("/api/apikeys", "POST", {"name": "resource-test"})
            require(status == 200, "key creation failed")
            key = created["key"]["rawKey"]
            key_id = created["key"]["id"]

            for label, data in fixtures.items():
                before = time.monotonic()
                status, uploaded = client.upload(data, bearer=key)
                require(status == 200, f"{label}: upload failed: {status} {uploaded}")
                timings.append({"fixture": label, "upload_ms": round((time.monotonic() - before) * 1000)})
                path = "/api/images/" + uploaded["id"]
                status, headers, received = client.request(path, auth=False)
                require(status == 200 and hashlib.sha256(received).digest() == hashlib.sha256(data).digest(),
                        "original bytes changed")
                etag = headers["ETag"]
                require(client.request(path, "HEAD", headers={"If-None-Match": etag})[0] == 304,
                        "conditional HEAD failed")
                status, headers, thumb = client.request(path + "?variant=thumb", auth=False)
                require(status == 200 and headers["Content-Type"] == "image/webp" and len(thumb) > 0,
                        "thumbnail failed")
                with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                    results = list(pool.map(lambda _: client.request(path, auth=False)[0], range(24)))
                require(all(value == 200 for value in results), "concurrent downloads failed")

            large = fixtures["4000x4000-rgba"]
            # Retain several decoded-image cycles to exercise GC under the limit.
            for _ in range(3):
                status, _ = client.upload(large)
                require(status == 200, "repeat large upload failed")
            barrier = threading.Barrier(4)

            def concurrent_upload(_):
                barrier.wait()
                return client.upload(large)[0]

            with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                burst = list(pool.map(concurrent_upload, range(4)))
            require(200 in burst and 429 in burst and all(s in (200, 429) for s in burst),
                    f"upload backpressure failed: {burst}")
            status, _, listing = client.json("/api/images?mine=true&limit=2")
            require(status == 200 and len(listing["items"]) == 2 and listing["nextCursor"], "pagination failed")
            target = listing["items"][0]["id"]
            path = "/api/images/" + target

            # 原 UI 的概览/上传弹窗依赖 usage；标签候选依赖 tags。
            status, _, usage = client.json("/api/images/usage")
            require(status == 200 and usage["usedBytes"] > 0 and isinstance(usage["quotaBytes"], int),
                    f"usage endpoint failed: {usage}")
            status, _, tags = client.json("/api/images/tags")
            require(status == 200 and "resource-test" in tags["tags"], f"tags endpoint failed: {tags}")

            require(client.json(path, "PATCH", {"visible": False})[0] == 200, "hide failed")
            require(client.request(path, auth=False)[1]["Content-Type"].startswith("image/svg+xml"), "hidden leak")
            require(client.json(path, "PATCH", {"visible": True})[0] == 200, "show failed")
            status, _, batch = client.json("/api/images/batch", "PATCH",
                                           {"ids": [target], "action": "visibility", "visible": False})
            require(status == 200 and batch["updatedCount"] == 1, f"batch hide failed: {batch}")
            require(client.request(path, auth=False)[1]["Content-Type"].startswith("image/svg+xml"),
                    "batch hidden leak")
            status, _, batch = client.json("/api/images/batch", "PATCH",
                                           {"ids": [target], "action": "visibility", "visible": True})
            require(status == 200 and batch["updatedCount"] == 1, f"batch show failed: {batch}")

            original_digest = hashlib.sha256(client.request(path)[2]).digest()
            require(client.json(path, "DELETE")[0] == 200, "soft delete failed")
            require(client.request(path, auth=False)[0] == 404 and client.request(path)[0] == 200, "trash access failed")
            status, _, trash = client.json("/api/images?mine=true&deleted=true&limit=20")
            require(status == 200 and trash["total"] == 1 and trash["items"][0]["id"] == target,
                    "private trash listing failed")
            require(client.request("/api/images?mine=true&deleted=true", auth=False,
                                   headers={"Authorization": "Bearer " + key})[0] == 401,
                    "API key accessed private trash")
            require(client.json(path + "/restore", "POST")[0] == 200, "trash restore failed")
            status, _, restored = client.request(path, auth=False)
            require(status == 200 and hashlib.sha256(restored).digest() == original_digest,
                    "restored original bytes changed")

            # 导出：HEAD 只预检不消耗冷却，GET 是真实流式 ZIP。
            status, headers, _ = client.request("/api/images/export?scope=all", "HEAD")
            require(status == 200, f"export HEAD failed: {status}")
            require(headers.get("X-Export-Count") and int(headers["X-Export-Count"]) >= 1,
                    "export HEAD count missing")
            require(headers.get("X-Export-Cooldown-Hours") == "12",
                    "export cooldown header changed")
            status, headers, archive = client.request("/api/images/export?scope=all")
            require(status == 200 and headers["Content-Type"] == "application/zip",
                    f"export GET failed: {status}")
            require(archive.startswith(b"PK\x03\x04") and b"manifest.json" in archive,
                    "export archive is not a ZIP with a manifest")
            require(len(archive) > 0, "export archive empty")

            require(client.json(path, "DELETE")[0] == 200, "second soft delete failed")
            status, _, purged = client.json(path + "?permanent=true", "DELETE")
            require(status == 200 and purged.get("cleanupPending") is False,
                    "permanent delete did not complete file cleanup")
            require(client.request(path)[0] == 404, "permanently deleted image remained readable")
            status, _, trash = client.json("/api/images?mine=true&deleted=true")
            require(status == 200 and trash["total"] == 0, "permanently deleted row remained in trash")
            require(client.json("/api/apikeys/" + key_id, "DELETE")[0] == 200, "key revoke failed")
            require(client.upload(fixtures["64x64-rgba"], bearer=key)[0] == 401, "revoked key accepted")

            # 原版 change-password：当前会话保持、其他旧会话失效、API Key 保留。
            status, _, created = client.json("/api/apikeys", "POST", {"name": "password-survival-test"})
            require(status == 200, "second key creation failed")
            second_key = created["key"]["rawKey"]
            old_cookie = client.cookie
            other = Client(client.base)
            status, headers, _ = other.json("/api/auth/login", "POST",
                                            {"email": "resource@example.com", "password": password})
            require(status == 200, "second session login failed")
            other.cookie = headers["Set-Cookie"].split(";", 1)[0]

            new_password = secrets.token_urlsafe(18)
            status, headers, raw = client.request("/api/auth/change-password", "POST",
                                                  json.dumps({"oldPassword": password,
                                                              "newPassword": new_password}).encode(),
                                                  {"Content-Type": "application/json"})
            require(status == 200, f"change-password failed: {status} {raw[:200]}")
            new_cookie = headers["Set-Cookie"].split(";", 1)[0]
            require(new_cookie != old_cookie, "change-password did not re-sign the cookie")
            client.cookie = new_cookie
            require(client.json("/api/auth/me")[0] == 200, "current session lost after change-password")
            require(other.json("/api/auth/me")[0] == 401, "other session survived change-password")
            require(client.request("/api/auth/me", auth=False,
                                   headers={"Cookie": old_cookie})[0] == 401,
                    "old cookie survived change-password")
            require(client.upload(fixtures["64x64-rgba"], bearer=second_key)[0] == 200,
                    "change-password revoked API keys")
            require(client.json("/api/auth/login", "POST", {
                "email": "resource@example.com", "password": password})[0] != 200,
                "old password survived change")
            status, headers, _ = client.json("/api/auth/login", "POST", {
                "email": "resource@example.com", "password": new_password})
            require(status == 200, "new password login failed")
            client.cookie = headers["Set-Cookie"].split(";", 1)[0]
            require(client.json("/api/auth/me")[0] == 200, "new session failed")
            require(client.json("/api/health")[0] == 200, "final health failed")
            stopped.set()
            sampler.join(timeout=2)
            final = json.loads(docker("inspect", container))[0]
            require(final["State"]["Running"] and not final["State"]["OOMKilled"], "container stopped/OOM")
            current = read_number(cg / "memory.current") if cg else None
            peak = read_number(cg / "memory.peak") if cg else None
            stats = json.loads(docker("stats", "--no-stream", "--format", "{{json .}}", container))
            return {"memory_limit_mib": memory, "cpu_limit": 1, "swap_bytes": 0,
                    "startup_to_health_ms": startup_ms, "idle_cgroup_bytes": idle,
                    "kernel_cgroup_peak_bytes": peak, "sampled_cgroup_peak_bytes": max(samples, default=None),
                    "final_cgroup_bytes": current, "docker_final_memory": stats["MemUsage"],
                    "oom_killed": final["State"]["OOMKilled"], "fixture_timings": timings,
                    "burst_upload_statuses": burst, "static_shell": static_shell,
                    "export": {"head_status": 200, "archive_bytes": len(archive)},
                    "ui_flows": ["static-shell-and-assets", "runtime-config", "api-usage", "api-tags",
                                 "batch-visibility", "export-head-and-zip",
                                 "trash-list", "restore-original", "permanent-delete",
                                 "change-password-keeps-session-and-keys",
                                 "other-session-revoked", "new-password-login"],
                    "result": "passed"}
        except Exception:
            if container:
                print(docker("logs", container))
            raise
        finally:
            stopped.set()
            if sampler:
                sampler.join(timeout=2)
            if container:
                subprocess.run(["docker", "rm", "-f", container], check=False, stdout=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="picbox:resource-test")
    parser.add_argument("--memory", nargs="+", type=int, default=[256, 512])
    parser.add_argument("--output", type=Path, default=Path("resource-results.json"))
    args = parser.parse_args()
    if any(value < 64 for value in args.memory):
        parser.error("memory must be at least 64 MiB")
    docker("image", "inspect", args.image)
    fixtures = {f"{w}x{h}-rgba": png(w, h) for w, h in [(64, 64), (4000, 3000), (4000, 4000)]}
    report = {"image_id": json.loads(docker("image", "inspect", args.image))[0]["Id"],
              "docker_version": docker("version", "--format", "{{.Server.Version}}"),
              "timestamp_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "memory_metric_note": "cgroup includes file cache; Docker stats may subtract cache. Peak excludes host OS.",
              "cases": []}
    for memory in args.memory:
        result = run_case(args.image, memory, fixtures)
        report["cases"].append(result)
        args.output.write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(result, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
