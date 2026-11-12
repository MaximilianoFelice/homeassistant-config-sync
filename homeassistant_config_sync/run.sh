#!/bin/sh
# App startup: initializes /data (bare repo, deploy key, API token), shows
# the deploy key and starts the API, the scheduler and stdin.
set -eu

options=/data/options.json

if ! hcs init --config "$options" --state-dir /data; then
  echo "The configuration is not valid. Check the app's Configuration tab (for example, 'repository' must be git@host:owner/repo.git)."
  exit 1
fi

echo "Deploy key for this app (add it in GitHub → Settings → Deploy keys, with Allow write access):"
hcs deploy-key --state-dir /data

exec hcs serve --config "$options" --state-dir /data --listen 0.0.0.0:8099 --stdin --sync-on-start --discovery
