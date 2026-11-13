#!/usr/bin/env bash
# notarize.sh <file.dmg|file.zip> <keychain-profile>
# notarytool exits 0 even when Apple rejects the upload, so check the status ourselves.
set -euo pipefail
file=$1 profile=$2
out=$(xcrun notarytool submit "$file" --keychain-profile "$profile" --wait --output-format json)
status=$(printf '%s' "$out" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')
id=$(printf '%s' "$out" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))')
if [ "$status" != "Accepted" ]; then
  echo "Notarization $status for $file" >&2
  [ -n "$id" ] && xcrun notarytool log "$id" --keychain-profile "$profile" >&2 || true
  exit 1
fi
echo "Notarized $file ($id)"
