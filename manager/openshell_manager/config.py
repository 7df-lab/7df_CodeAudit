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
      "gatewayEndpoint": "gateway.internal:8080",
      "libPath":         "libs/OpenShell/python"     # relative to service root
    }

The service is a thin transport over the OpenShell Gateway SDK: it adds no
domain logic and never stores credentials (secrets pass through to the
gateway, which keeps them in its encrypted provider store).
"""
from __future__ import annotations

import json
import os
from pathlib import Path

SERVICE_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_GATEWAY_ENDPOINT = "gateway.internal:8080"

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


def _token_from_file(token_file: str) -> str:
    path = Path(token_file)
    if not path.is_absolute():
        path = SERVICE_ROOT / token_file
    try:
        return path.read_text(encoding="utf-8").strip()
    except Exception:  # noqa: BLE001 - missing file == no token
        return ""


def manager_token() -> str:
    """Bearer token for /api/* routes.

    Priority: $OPENSHELL_MANAGER_TOKEN > config ``tokenFile`` (relative to
    the service root) > config ``token``. Empty = auth disabled (loopback
    binds only).
    """
    env = os.environ.get("OPENSHELL_MANAGER_TOKEN", "").strip()
    if env:
        return env
    token_file = _cfg("tokenFile")
    if token_file:
        token = _token_from_file(token_file)
        if token:
            return token
    return _cfg("token")


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

    Priority: $OPENSHELL_MANAGER_MAX_UPLOAD_BYTES > config ``maxUploadBytes``.
    """
    env = os.environ.get("OPENSHELL_MANAGER_MAX_UPLOAD_BYTES", "").strip()
    if env:
        return int(env)
    try:
        return int(_cfg("maxUploadBytes", "0"))
    except ValueError:
        return 0


def validate() -> None:
    """Fail loud on unsafe combinations (bind discipline)."""
    bind = manager_bind()
    if not manager_token() and bind not in ("127.0.0.1", "localhost", "::1"):
        raise RuntimeError(
            f"refusing to bind {bind} without OPENSHELL_MANAGER_TOKEN: the "
            "service can execute commands inside sandboxes and must never be "
            "openly reachable")
