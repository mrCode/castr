#!/bin/sh
# Checks that every SOURCE file the PKGBUILD installs exists in the repo.
# A missing one fails at makepkg time, on a user's machine, not here.
#
# Only the source half of each install line is checked: the other path is a
# destination under $pkgdir and does not exist until the package is built.
set -e
cd "$(dirname "$0")/.."
status=0

# build/ holds compiled binaries that exist only after build(); everything
# else must already be in the repo.
sources=$(grep -oE '^\s*install -Dm[0-9]+ [^ ]+' packaging/PKGBUILD \
          | awk '{print $3}' | grep -v '\$' | grep -v '^build/' | sort -u)
for f in $sources; do
  if [ ! -f "$f" ]; then
    echo "PKGBUILD installs $f, which does not exist"
    status=1
  fi
done

# Continuation lines: `install -Dm644 path \` puts the destination on the next
# line, so the source is still field 3 above. Anything with no source found at
# all is a sign this parser has drifted from the PKGBUILD.
count=$(printf '%s\n' "$sources" | grep -c . || true)
if [ "$count" -lt 4 ]; then
  echo "only found $count installed files; this checker has drifted from the PKGBUILD"
  status=1
fi

[ "$status" -eq 0 ] && echo "all $count installed files exist"
exit $status
