#!/usr/bin/env bash
# Rewrites the rolling section of test/hatchet-versions with the newest Hatchet release and the
# newest patch of the newest minor released at least 30 and 90 days ago. Minors are dated by the
# commit of their first patch. Needs curl, jq and an authenticated gh.
set -euo pipefail

file=${1:-test/hatchet-versions}
repo=hatchet-dev/hatchet

grep -qx '# rolling' "$file" || { echo "$file has no '# rolling' marker" >&2; exit 1; }

image=$repo/hatchet-lite
token=$(curl -fsS "https://ghcr.io/token?scope=repository:$image:pull" | jq -r .token)
tags="" last=""
while :; do
  page=$(curl -fsS -H "Authorization: Bearer $token" "https://ghcr.io/v2/$image/tags/list?n=1000&last=$last" | jq -r '.tags // [] | .[]')
  [[ -n "$page" ]] || break
  tags+="$page"$'\n'
  last=$(tail -n1 <<< "$page")
done

# "<first patch> <newest patch>" per minor, newest minor first.
minors=()
while IFS= read -r line; do
  minors+=("$line")
done < <(grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' <<< "$tags" | sort -V | awk -F. '
  { minor = $1 "." $2; if (!(minor in first)) { first[minor] = $0; order[++count] = minor }; newest[minor] = $0 }
  END { for (i = count; i >= 1; i--) print first[order[i]], newest[order[i]] }')
if [[ ${#minors[@]} -eq 0 ]]; then
  echo "no hatchet-lite releases found" >&2
  exit 1
fi

# Release time of a tag, or the current time when GitHub has no such tag, which skips its minor.
released() {
  gh api "repos/$repo/commits/$1" --jq '.commit.committer.date | fromdateiso8601' 2>/dev/null || date +%s
}

now=$(date +%s)
picks=() i=0 date=""
for age in 0 30 90; do
  cutoff=$((now - age * 86400))
  while ((i < ${#minors[@]})); do
    read -r first newest <<< "${minors[i]}"
    [[ -n "$date" ]] || date=$(released "$first")
    if ((date <= cutoff)); then
      picks=("$newest $age" "${picks[@]+"${picks[@]}"}")
      break
    fi
    i=$((i + 1)) date=""
  done
done

# Picks at or below the hand-maintained minimum are dropped.
minimum=$(awk '!/^#/ && NF {print $1; exit}' "$file")
{
  sed '/^# rolling$/q' "$file"
  printf '%s\n' "${picks[@]}" | while read -r version age; do
    if [[ -z "$minimum" || "$(printf '%s\n' "$minimum" "$version" | sort -V | tail -n1)" != "$minimum" ]]; then
      echo "$version $age"
    fi
  done | awk '!seen[$1]++ {
    note = $2 == 0 ? "newest release" : "newest patch of the newest minor released " $2 "+ days ago"
    printf "%-10s %s\n", $1, note
  }'
} > "$file.tmp"
mv "$file.tmp" "$file"
