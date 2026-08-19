#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Antoni Romera
#
# The screenshots in the README, retaken.
#
# A tool with a web console and no screenshot in its README is one people assume
# has no console, so these are part of the project rather than something taken
# once by hand and never matched to the code again.
#
# How it works, and why it is not just "point a browser at the console":
#
#   - The pages are fetched with curl, SIGNED IN. A headless browser cannot fill
#     in a login form without being scripted, and the alternative — running the
#     console with --dev-identity — puts a "Development mode. Nobody is being
#     authenticated" banner across the top of every shot.
#   - The saved HTML has its stylesheet link rewritten to an absolute URL, so the
#     browser renders a local file against the running the server assets.
#   - Two device pixels per CSS pixel, because a screenshot of an interface that
#     is soft on a retina display looks like a screenshot of a soft interface.
#
# Usage: CONSOLE=http://localhost:8092 USER=toni PASS=... ./hack/screenshots.sh
set -euo pipefail

CONSOLE=${CONSOLE:-http://localhost:8092}
USERNAME=${USER_NAME:-toni}
PASSWORD=${PASSWORD:?set PASSWORD to the console account password}
OUT=${OUT:-docs/screenshots}
CHROME=${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

[ -x "$CHROME" ] || { echo "no headless browser at $CHROME (set CHROME=)" >&2; exit 1; }
mkdir -p "$OUT"

jar="$WORK/cookies"
curl -sf -c "$jar" -b "$jar" -X POST "$CONSOLE/auth/login" \
  --data-urlencode "user=$USERNAME" --data-urlencode "password=$PASSWORD" -o /dev/null

shot() { # path, name, theme
  curl -s -c "$jar" -b "$jar" "$CONSOLE/theme?to=$3&back=/" -o /dev/null
  curl -sf -b "$jar" "$CONSOLE$1" \
    | sed "s|href=\"/assets/|href=\"$CONSOLE/assets/|g" > "$WORK/$2.html"
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars \
    --window-size=1400,880 --force-device-scale-factor=2 \
    --virtual-time-budget=4000 --allow-file-access-from-files \
    --screenshot="$OUT/$2.png" "file://$WORK/$2.html" >/dev/null 2>&1
  printf '  %s\n' "$OUT/$2.png"
}

shot /                     console-overview-light     light
shot /                     console-overview-dark      dark
shot /c/demo/acme          console-customer-light     light
shot /e/demo/url-staging   console-environment-light  light
shot /b/demo/rb-nightly    console-backup-light       light

# Leave the account as it was found: the theme is a cookie on this session, but
# somebody running this against a console they use would rather it did not choose
# for them.
curl -s -c "$jar" -b "$jar" "$CONSOLE/theme?to=auto&back=/" -o /dev/null
