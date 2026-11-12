#!/usr/bin/env bash
# Cuts a release: sets the version everywhere it lives, checks the changelog
# has an entry for it, commits and creates the annotated tag vX.Y.Z. It does
# not push: pushing the tag is what starts the release workflow.
#
#   scripts/release.sh 0.0.2
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:?usage: scripts/release.sh X.Y.Z}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not a semver version: $version" >&2; exit 2; }
tag=v$version

[[ -z $(git status --porcelain) ]] || { echo "the working tree is not clean" >&2; exit 1; }
[[ $(git rev-parse --abbrev-ref HEAD) == main ]] || { echo "releases are cut from main" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && { echo "$tag already exists" >&2; exit 1; }
grep -q "^## $version\$" homeassistant_config_sync/CHANGELOG.md || {
  echo "homeassistant_config_sync/CHANGELOG.md has no '## $version' section" >&2; exit 1; }

sed -i -E "s/^version: .*/version: \"$version\"/" homeassistant_config_sync/config.yaml
manifest=custom_components/config_sync/manifest.json
if [[ -f $manifest ]]; then
  sed -i -E "s/\"version\": \"[^\"]*\"/\"version\": \"$version\"/" "$manifest"
fi

scripts/release-verify.sh "$tag"
mise run check >/dev/null

git add -A homeassistant_config_sync/config.yaml "$manifest" 2>/dev/null || git add homeassistant_config_sync/config.yaml
git diff --cached --quiet || git commit -q -m "release: $tag"
git tag -a "$tag" -m "Git Config Sync $tag"
echo "Tagged $tag. Push it to publish:"
echo "  git push origin main $tag"
