#!/bin/sh
# App startup: initializes /data (bare repo, askpass program, API token) and
# starts the API, the web UI and the scheduler.
set -eu

options=/data/options.json

if ! hcs init --config "$options" --state-dir /data; then
  echo "The configuration is not valid. Check the app's Configuration tab ('repository' is the repo's HTTPS URL and 'token' an access token with read and write access to it)."
  exit 1
fi

exec hcs serve --config "$options" --state-dir /data --listen 0.0.0.0:8099 --sync-on-start --discovery
