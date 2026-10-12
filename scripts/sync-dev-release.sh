#!/usr/bin/env bash
set -euo pipefail

fail() { echo "$*" >&2; exit 1; }
[[ $# == 3 ]] || fail "Usage: sync-dev-release.sh FIXED_VERSION COMMIT_SHA DIST_DIR"
version=$1
commit=$2
repository=${GITHUB_REPOSITORY:-}
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail "Expected canonical vMAJOR.MINOR.PATCH"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || fail "Expected a full commit SHA"
[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "Expected GitHub OWNER/REPO"
case "${GITHUB_EVENT_NAME:-}" in
  push|workflow_dispatch) ;;
  *) echo "Skipping dev synchronization for this event"; exit 0 ;;
esac

dist="$(cd -- "$3" && pwd -P)"
names=(mi-node-linux-amd64 mi-node-linux-arm64 xbctl-linux-amd64 xbctl-linux-arm64 node-release.json)
files=()
for name in "${names[@]}"; do
  file="$dist/$name"
  [[ -f "$file" && ! -L "$file" && -s "$file" ]] || fail "Expected nonempty regular file: $name"
  files+=("$file")
done
manifest="$dist/node-release.json"
[[ $(wc -c < "$manifest") -le 1048576 ]] || fail "Manifest exceeds 1 MiB"
# Validate the exact manifest contract and retain immutable fixed-version URLs.
jq -e -s --arg version "$version" --arg repo "$repository" '
  length == 1 and (.[0] |
    type == "object" and
    keys == ["artifacts", "min_agent_protocol", "os", "version"] and
    .version == $version and .os == "linux" and .min_agent_protocol == 1 and
    (.artifacts | type == "array" and length == 4 and
      all(.[];
        type == "object" and
        keys == ["arch", "component", "https_url", "sha256", "size_bytes"] and
        (.arch == "amd64" or .arch == "arm64") and
        (.component == "mi-node" or .component == "xbctl") and
        (.sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
        (.size_bytes | type == "number" and . == floor and . > 0 and . <= 536870912) and
        .https_url == ("https://github.com/" + $repo + "/releases/download/" + $version + "/" + .component + "-linux-" + .arch)) and
      ([.[] | .component + "-linux-" + .arch] | unique | length == 4)))
' "$manifest" >/dev/null || fail "Invalid fixed-release manifest"
for name in "${names[@]:0:4}"; do
  read -r expected_size expected_hash < <(jq -r --arg name "$name" '
    .artifacts[] | select(.component + "-linux-" + .arch == $name) |
    "\(.size_bytes) \(.sha256)"
  ' "$manifest")
  actual_size=$(wc -c < "$dist/$name")
  actual_hash=$(sha256sum -- "$dist/$name")
  [[ "$actual_size" -eq "$expected_size" && "${actual_hash%% *}" == "$expected_hash" ]] || fail "Artifact metadata mismatch: $name"
done

# A queued old build must never replace the compatibility entry of the live latest.
latest=$(gh release view --repo "$repository" --json tagName --jq .tagName)
if [[ "$latest" != "$version" ]]; then
  echo "Skipping dev synchronization: $version is not latest ($latest)"
  exit 0
fi
fixed=$(gh release view "$version" --repo "$repository" --json tagName,isDraft,isPrerelease,assets)
expected_names=$(jq -cn --args '$ARGS.positional | sort' "${names[@]}")
jq -e --arg version "$version" --argjson names "$expected_names" '
  .tagName == $version and .isDraft == false and .isPrerelease == false and
  (.assets | type == "array" and (map(.name) | sort) == $names)
' <<< "$fixed" >/dev/null || fail "Source fixed release is not complete and published"
for name in "${names[@]}"; do
  size=$(wc -c < "$dist/$name")
  jq -e --arg name "$name" --argjson size "$size" '
    .assets | any(.name == $name and .size == $size)
  ' <<< "$fixed" >/dev/null || fail "Source fixed asset size mismatch: $name"
done

printf -v notes '%s\n\nSource fixed release: %s\nSource commit: %s\nhttps://github.com/%s/releases/tag/%s\n\n%s' \
  'Mutable compatibility download for legacy xbctl --version dev.' \
  "$version" "$commit" "$repository" "$version" \
  'Assets and manifest are copied unchanged from the fixed release. The historical dev git tag is retained.'
if gh release view dev --repo "$repository" >/dev/null 2>&1; then
  # Never pass --target here: the historical dev tag must retain its commit.
  gh release edit dev --repo "$repository" --prerelease --latest=false
else
  gh release create dev --repo "$repository" --target "$commit" \
    --title 'dev (mutable compatibility download)' --draft --prerelease --latest=false \
    --notes 'Mutable compatibility download; synchronization in progress.'
fi
# Only this explicit mutable alias may overwrite assets; never upload a dist glob.
gh release upload dev --repo "$repository" "${files[@]}" --clobber
gh release edit dev --repo "$repository" --draft=false --prerelease --latest=false \
  --title 'dev (mutable compatibility download)' --notes "$notes"
