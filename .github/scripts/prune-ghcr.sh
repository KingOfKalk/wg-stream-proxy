#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Retention for the ghcr.io container package (container.yml, job Prune).
#
# Keeps:
#   - every version with a tag other than sha-* (edge, X.Y.Z, X.Y),
#   - the KEEP_SHA newest versions that only carry sha-* tags,
#   - every digest referenced by a kept index (the per-platform manifests;
#     ghcr.io lists them as separate untagged versions),
#   - untagged versions younger than MIN_AGE_HOURS (a publish in flight).
# Deletes everything else. Any lookup error aborts before the first
# deletion, and so does a package without an `edge` version.
#
# Why not actions/delete-package-versions: for containers it matches
# ignore-versions against the digest, not the tags, and its
# delete-only-untagged-versions also deletes the untagged child manifests
# of tagged multi-arch images.
#
# Usage (needs gh, jq, docker logged in to ghcr.io, GH_TOKEN with
# packages: write):
#   IMAGE=ghcr.io/<owner>/<name> .github/scripts/prune-ghcr.sh
# Options (environment): KEEP_SHA (default 10), MIN_AGE_HOURS (default 24),
# DRY_RUN=1 (print the plan, delete nothing).
set -euo pipefail

image="${IMAGE:?usage: IMAGE=ghcr.io/<owner>/<name> prune-ghcr.sh}"
keep_sha="${KEEP_SHA:-10}"
min_age_hours="${MIN_AGE_HOURS:-24}"
dry_run="${DRY_RUN:-0}"

path="${image#ghcr.io/}"
owner="${path%%/*}"
package="${path#*/}"
api="/users/${owner}/packages/container/${package}/versions"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --paginate prints one JSON array per page.
gh api --paginate "${api}?per_page=100" | jq -s 'add // []' > "$tmp/versions.json"

if ! jq -e 'any(.[]; (.metadata.container.tags // []) | index("edge"))' \
  "$tmp/versions.json" > /dev/null; then
  echo "no version tagged edge in ${image}; refusing to prune" >&2
  exit 1
fi

# Kept tagged versions (by digest): non-sha tags, plus the newest sha-only ones.
jq -r --argjson keep "$keep_sha" '
  def tags: .metadata.container.tags // [];
  def sha_only: (tags | length > 0) and all(tags[]; startswith("sha-"));
  ([.[] | select(tags | length > 0) | select(sha_only | not)]
   + ([.[] | select(sha_only)] | sort_by(.created_at) | reverse | .[:$keep]))
  | .[].name' "$tmp/versions.json" > "$tmp/kept.txt"

# Everything a kept index references is protected too.
cp "$tmp/kept.txt" "$tmp/protected.txt"
while read -r digest; do
  [ -n "$digest" ] || continue
  docker buildx imagetools inspect "${image}@${digest}" --raw > "$tmp/manifest.json"
  jq -e '.schemaVersion' "$tmp/manifest.json" > /dev/null
  jq -r '.manifests[]?.digest' "$tmp/manifest.json" >> "$tmp/protected.txt"
done < "$tmp/kept.txt"

# Plan: id, digest, reason.
jq -r --argjson min_age "$min_age_hours" --rawfile protected "$tmp/protected.txt" '
  def tags: .metadata.container.tags // [];
  ($protected | split("\n") | map(select(length > 0))) as $p
  | (now - $min_age * 3600) as $cutoff
  | .[]
  | select(.name as $n | $p | index($n) | not)
  | select((tags | length > 0) or ((.updated_at | fromdateiso8601) < $cutoff))
  | [.id, .name, (if (tags | length) > 0 then "old " + (tags | join(",")) else "untagged" end)]
  | @tsv' "$tmp/versions.json" > "$tmp/plan.tsv"

kept="$(wc -l < "$tmp/kept.txt")"
protected="$(sort -u "$tmp/protected.txt" | wc -l)"
delete="$(wc -l < "$tmp/plan.tsv")"
total="$(jq 'length' "$tmp/versions.json")"

{
  echo "## Prune"
  echo
  echo "- ${image}: ${total} versions, ${kept} kept tagged, ${protected} protected digests, ${delete} to delete$([ "$dry_run" = 1 ] && echo ' (dry run)')"
  if [ "$delete" -gt 0 ]; then
    echo
    echo "| Version id | Digest | Reason |"
    echo "| --- | --- | --- |"
    while IFS=$'\t' read -r id digest reason; do
      echo "| ${id} | \`${digest}\` | ${reason} |"
    done < "$tmp/plan.tsv"
  fi
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"

[ "$dry_run" = 1 ] && exit 0

while IFS=$'\t' read -r id digest reason; do
  echo "deleting ${id} ${digest} (${reason})"
  gh api -X DELETE "${api}/${id}"
done < "$tmp/plan.tsv"
