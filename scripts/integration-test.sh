#!/usr/bin/env bash
# Tests for the config_sync integration (custom_components/), with the Home
# Assistant test harness pinned in requirements_test.txt. The virtualenv lives
# in .venv (ignored by git) and is created on first use.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -x .venv/bin/python ]] || ! cmp -s requirements_test.txt .venv/.requirements; then
  rm -rf .venv
  uv venv --quiet --python "$(mise which python)" .venv
  uv pip install --quiet --python .venv/bin/python -r requirements_test.txt
  cp requirements_test.txt .venv/.requirements
fi
# Custom integrations load translations/en.json; strings.json is the source.
cp custom_components/config_sync/strings.json custom_components/config_sync/translations/en.json
.venv/bin/python -m pytest -q "$@"
