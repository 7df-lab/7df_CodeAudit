#!/usr/bin/env bash
# Launch the OpenShell manager microservice (default 127.0.0.1:18800).
#
#   OPENSHELL_MANAGER_TOKEN=... ./run.sh
#
# Env: OPENSHELL_MANAGER_BIND / _PORT / _TOKEN,
#      OPENSHELL_GATEWAY_ENDPOINT, OPENSHELL_LIB_PATH (see config.py).
set -euo pipefail
cd "$(dirname "$0")"
exec python3 -m openshell_manager
