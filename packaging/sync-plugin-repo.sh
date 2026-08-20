#!/bin/sh
# Copy the bar widget into the standalone plugin repo, or report drift.
#
# The widget is developed here, under share/quickshell/castr-indicator/, where
# internal/ui's tests read it. The marketplace needs a repo whose ROOT holds
# manifest.json, so a second repo exists holding a copy. Two copies of anything
# drift; this is the one command that keeps them honest.
#
#   sync-plugin-repo.sh          report whether they differ
#   sync-plugin-repo.sh --write  copy this repo's version over
set -e
cd "$(dirname "$0")/.."
SRC=share/quickshell/castr-indicator
DST=${CASTR_PLUGIN_REPO:-../castr-indicator}

[ -d "$DST" ] || { echo "no plugin repo at $DST (set CASTR_PLUGIN_REPO)"; exit 2; }

status=0
if ! diff -q "$SRC/Widget.qml" "$DST/Widget.qml" >/dev/null 2>&1; then
  echo "Widget.qml differs"
  status=1
fi
# The manifests differ on purpose: the plugin repo carries a marketplace
# description. Only the fields that must match are compared.
for field in schemaVersion id version author kinds; do
  a=$(python3 -c "import json,sys;print(json.load(open('$SRC/manifest.json')).get('$field'))")
  b=$(python3 -c "import json,sys;print(json.load(open('$DST/manifest.json')).get('$field'))")
  [ "$a" = "$b" ] || { echo "manifest.$field differs: $a vs $b"; status=1; }
done

if [ "$status" -eq 0 ]; then
  echo "in sync"
  exit 0
fi
if [ "$1" = "--write" ]; then
  cp "$SRC/Widget.qml" "$DST/Widget.qml"
  python3 - "$SRC/manifest.json" "$DST/manifest.json" <<'PY'
import json, sys
src = json.load(open(sys.argv[1])); dst = json.load(open(sys.argv[2]))
for f in ("schemaVersion", "id", "version", "author", "kinds", "entryPoints"):
    dst[f] = src[f]
json.dump(dst, open(sys.argv[2], "w"), indent=2); open(sys.argv[2], "a").write("\n")
PY
  echo "copied into $DST — commit it there"
  exit 0
fi
echo "run with --write to copy"
exit 1
