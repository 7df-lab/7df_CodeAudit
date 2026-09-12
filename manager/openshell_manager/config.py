"""Configuration for the OpenShell manager microservice.

Resolution order everywhere: environment variable > the GLOBAL config file >
built-in default. The global config file is the SINGLE source of truth shared
with the engine repo (its ``openshell_manager_client`` reads the same file for
``url``/``token``/``tokenFile`` so both sides cannot drift apart):

    openshell-manager/config.json
    {
      "url":             "http://127.0.0.1:18800",   # what ENGINE clients use
      "bind":            "127.0.0.1",                # what the service binds
      "port":            18800,
      "tokenFile":       ".token",                   # relative to service root
      "gatewayEndpoint": "host.docker.internal:8080",
      "libPath":         "libs/OpenShell/python"     # relative to service root
    }

The service is a thin transport over the OpenShell Gateway SDK: it adds no
domain logic and never stores credentials (secrets pass through to the
gateway, which keeps them in its encrypted provider store).
"""
from __future__ import annotations

import json
import os
import time
from pathlib import Path

SERVICE_ROOT = Path(__file__).resolve().parent.parent
# 中性缺省(经 hosts 别名解析, 零 DNS 依赖); 有自定义域名时 env/config 覆盖(2026-09-08 内网域清中性)
DEFAULT_GATEWAY_ENDPOINT = "host.docker.internal:8080"
# B3-3 审计修复：上传上限缺省 2 GiB（原 0=不限，防误操作上限形同虚设）。
# 上传是流式转发（内存恒定 <1 MiB），上限纯防"误指 50GB 归档"类误操作。
DEFAULT_MAX_UPLOAD_BYTES = 2 * 1024 * 1024 * 1024  # 2147483648

_config_cache: dict | None = None


def config_file_path() -> Path:
    env = os.environ.get("OPENSHELL_MANAGER_CONFIG", "").strip()
    return Path(env) if env else SERVICE_ROOT / "config.json"


def _file_config() -> dict:
    global _config_cache
    if _config_cache is None:
        path = config_file_path()
        try:
            raw = path.read_text(encoding="utf-8") if path.is_file() else "{}"
            parsed = json.loads(raw)
            _config_cache = parsed if isinstance(parsed, dict) else {}
        except Exception:  # noqa: BLE001 - a broken config must not kill env-only use
            _config_cache = {}
    return _config_cache


def _cfg(key: str, default: str = "") -> str:
    value = _file_config().get(key)
    return str(value).strip() if value is not None else default


def manager_bind() -> str:
    """Listen address. Non-loopback binds REQUIRE a token (see validate())."""
    return (os.environ.get("OPENSHELL_MANAGER_BIND", "").strip()
            or _cfg("bind", "127.0.0.1"))


def manager_port() -> int:
    env = os.environ.get("OPENSHELL_MANAGER_PORT", "").strip()
    if env:
        return int(env)
    try:
        return int(_cfg("port", "18800"))
    except ValueError:
        return 18800


class TokenFileError(RuntimeError):
    """tokenFile 已配置但读取失败（EACCES/EIO 等）——fail-closed 信号。

    区别于"文件不存在"（= 未配置 token，按无鉴权放行维持现状）：文件在
    而读不到 = 鉴权材料不可得，require_token 必须拒绝请求（503）而不是
    吞掉异常当无 token 静默放行（B3-2 审计修复）。"""


# B3-2：文件解析结果 5s 缓存（env 分支不缓存——直读内存无 IO 且优先级
# 需实时生效）。require_token 是 async 依赖，无缓存时每请求一次阻塞磁盘
# 读。TokenFileError 不落缓存：权限恢复后下一个请求即自动恢复。
_TOKEN_CACHE_TTL_SECONDS = 5.0
_token_cache: "tuple[str, float] | None" = None  # (value, monotonic expiry)


def _token_from_file(token_file: str) -> str:
    path = Path(token_file)
    if not path.is_absolute():
        path = SERVICE_ROOT / token_file
    if not path.exists():
        return ""  # 文件不存在 = 未配置 token（放行，维持现状）
    try:
        return path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise TokenFileError(
            f"token file {path} exists but is unreadable: {exc}") from exc


def manager_token() -> str:
    """Bearer token for /api/* routes.

    Priority: $OPENSHELL_MANAGER_TOKEN > config ``tokenFile`` (relative to
    the service root) > config ``token``. Empty = auth disabled (loopback
    binds only). File-sourced results are cached for
    ``_TOKEN_CACHE_TTL_SECONDS`` (async dependency runs this per request;
    the disk read must not block the event loop on every call).
    """
    env = os.environ.get("OPENSHELL_MANAGER_TOKEN", "").strip()
    if env:
        return env
    global _token_cache
    now = time.monotonic()
    if _token_cache is not None and _token_cache[1] > now:
        return _token_cache[0]
    token_file = _cfg("tokenFile")
    token = _token_from_file(token_file) if token_file else ""
    if not token:
        token = _cfg("token")
    _token_cache = (token, now + _TOKEN_CACHE_TTL_SECONDS)
    return token


def gateway_endpoint() -> str:
    """The OpenShell Gateway gRPC endpoint (manager-side addressing)."""
    return (os.environ.get("OPENSHELL_GATEWAY_ENDPOINT", "").strip()
            or _cfg("gatewayEndpoint", DEFAULT_GATEWAY_ENDPOINT)
            or DEFAULT_GATEWAY_ENDPOINT)


def openshell_lib_path() -> Path:
    """Directory holding the vendored ``openshell`` Python SDK.

    Resolution: $OPENSHELL_LIB_PATH > config ``libPath`` (relative to the
    service root) > this service's own ``libs/OpenShell/python``. The python
    vendor subtree has been git-tracked since 2026-09-05, so a fresh clone
    always carries it; the pre-rename engine-checkout fallback paths
    (four-direction-pentest-engine / docs) are dead in the ADR-208 world and
    were removed (LESSONS #2).
    """
    env = os.environ.get("OPENSHELL_LIB_PATH", "").strip()
    if env:
        candidate = Path(env)
        if not (candidate / "openshell").is_dir():
            raise RuntimeError(
                f"OPENSHELL_LIB_PATH={env} has no openshell/ package inside")
        return candidate
    configured = _cfg("libPath")
    candidates = []
    if configured:
        path = Path(configured)
        candidates.append(path if path.is_absolute() else SERVICE_ROOT / path)
    candidates.append(SERVICE_ROOT / "libs" / "OpenShell" / "python")
    for candidate in candidates:
        if (candidate / "openshell").is_dir():
            return candidate
    raise RuntimeError(
        "openshell SDK not found: set OPENSHELL_LIB_PATH or config libPath "
        "to the vendored libs/OpenShell/python directory")


def max_upload_bytes() -> int:
    """Policy cap for streamed file uploads, in bytes. 0 = unlimited.

    Purely a mis-upload guard (e.g. someone points curl -F at a 50GB
    archive by accident): memory is NOT the reason — the upload path
    streams, so manager memory stays constant regardless of file size.

    Priority: $OPENSHELL_MANAGER_MAX_UPLOAD_BYTES > config ``maxUploadBytes``
    > DEFAULT_MAX_UPLOAD_BYTES (2 GiB, B3-3 — the old default of 0/unlimited
    left the guard disarmed).
    """
    env = os.environ.get("OPENSHELL_MANAGER_MAX_UPLOAD_BYTES", "").strip()
    if env:
        return int(env)
    try:
        return int(_cfg("maxUploadBytes", str(DEFAULT_MAX_UPLOAD_BYTES)))
    except ValueError:
        return DEFAULT_MAX_UPLOAD_BYTES


def validate() -> None:
    """Fail loud on unsafe combinations (bind discipline)."""
    bind = manager_bind()
    if not manager_token() and bind not in ("127.0.0.1", "localhost", "::1"):
        raise RuntimeError(
            f"refusing to bind {bind} without OPENSHELL_MANAGER_TOKEN: the "
            "service can execute commands inside sandboxes and must never be "
            "openly reachable")
