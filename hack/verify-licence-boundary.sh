#!/usr/bin/env bash
# The licence boundary, enforced instead of documented.
#
# This repository is AGPL-3.0 except for api/, which is Apache-2.0. That is not
# tidiness: api/ is the integration surface. Anyone writing Go against Doblura
# imports those types — including the proprietary enterprise edition, and
# including third parties who have no intention of being AGPL. If api/ were AGPL,
# every Go integration in the ecosystem would be forced to match, which is exactly
# the friction that kills an ecosystem.
#
# The boundary only holds while api/ depends on nothing under AGPL. One import of
# internal/ and the Apache promise is void — silently, because it still compiles
# and every test still passes. So it is checked.
set -uo pipefail
fails=0

MODULE=$(awk '/^module /{print $2}' go.mod)

say() { printf '  %-6s %s\n' "$1" "$2"; }

# ── 1. api/ must not import anything else from this module ──
deps=$(go list -deps ./api/... 2>&1)
if [ $? -ne 0 ]; then
  # An import of AGPL code from api/ usually creates a cycle, so `go list` fails
  # rather than reporting it. Swallowing that error made this check pass exactly
  # when it mattered most.
  say FAIL "go list could not resolve api/'s dependencies:"
  printf '           %s\n' "$(printf '%s' "$deps" | head -3)"
  fails=$((fails+1))
  deps=""
  resolved=no
fi
leaked=$(printf '%s' "$deps" \
  | grep "^${MODULE}/" \
  | grep -v "^${MODULE}/api" || true)
if [ -n "$leaked" ]; then
  say FAIL "api/ imports AGPL code from this module:"
  printf '           %s\n' $leaked
  say ""    "api/ is Apache-2.0; anything it imports is effectively Apache-2.0 too."
  fails=$((fails+1))
elif [ "${resolved:-yes}" = yes ]; then
  say ok "api/ imports nothing else from this module"
fi

# ── 2. Every file under api/ must carry the Apache header ──
missing=$(grep -rL 'SPDX-License-Identifier: Apache-2.0' --include='*.go' api/ 2>/dev/null \
  | grep -v 'zz_generated' || true)
if [ -n "$missing" ]; then
  say FAIL "files under api/ without the Apache-2.0 SPDX header:"
  printf '           %s\n' $missing
  fails=$((fails+1))
else
  say ok "every file under api/ declares Apache-2.0"
fi

# ── 3. And no file under api/ may claim AGPL ──
wrong=$(grep -rl 'AGPL' --include='*.go' api/ 2>/dev/null || true)
if [ -n "$wrong" ]; then
  say FAIL "files under api/ claiming AGPL:"
  printf '           %s\n' $wrong
  fails=$((fails+1))
else
  say ok "no file under api/ claims AGPL"
fi

# ── 4. Conversely, the operator itself must NOT be Apache ──
#
# Guards the mistake in the other direction: copying a file out of api/ into
# internal/ carries the Apache header with it, and quietly relicenses the part
# that is meant to be copyleft.
loose=$(grep -rl 'SPDX-License-Identifier: Apache-2.0' --include='*.go' internal/ cmd/ 2>/dev/null || true)
if [ -n "$loose" ]; then
  say FAIL "files outside api/ declaring Apache-2.0 (copied out of api/?):"
  printf '           %s\n' $loose
  fails=$((fails+1))
else
  say ok "internal/ and cmd/ are AGPL throughout"
fi

# ── 5. Both licence texts must be present ──
for f in LICENSE api/LICENSE; do
  [ -s "$f" ] || { say FAIL "$f is missing or empty"; fails=$((fails+1)); }
done
grep -q 'GNU AFFERO GENERAL PUBLIC LICENSE' LICENSE 2>/dev/null \
  || { say FAIL "LICENSE is not the AGPL text"; fails=$((fails+1)); }
grep -q 'Apache License' api/LICENSE 2>/dev/null \
  || { say FAIL "api/LICENSE is not the Apache text"; fails=$((fails+1)); }
[ $fails -eq 0 ] && say ok "both licence texts present and correct"

echo
[ $fails -eq 0 ] && echo "  licence boundary OK" || { echo "  $fails licence problem(s)"; exit 1; }
