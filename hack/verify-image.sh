#!/usr/bin/env sh
# Doblura image conformance check.
#
# Run this INSIDE a candidate image. It reports, capability by capability, what
# Doblura can and cannot drive with it. Exit code 0 means the core is present;
# individual capabilities may still be missing, and that is fine as long as you
# do not declare a spec that needs them.
#
#   docker run --rm -v "$PWD/hack/verify-image.sh:/v.sh:ro" --entrypoint sh IMAGE /v.sh
#
# Why a capability report and not a pass/fail: the contract is not monolithic.
# An image with click-odoo but no psql can run OdooBackup rehearsals perfectly
# and cannot run PgDump ones. Telling you which is far more useful than refusing
# to start.
set -u
ok=0; missing=0

have() { command -v "$1" >/dev/null 2>&1; }
line() { printf '  %-4s %-22s %s\n' "$1" "$2" "$3"; }

cap() { # name, description, commands...
  name=$1; desc=$2; shift 2
  lacking=""
  for c in "$@"; do have "$c" || lacking="$lacking $c"; done
  if [ -z "$lacking" ]; then
    line "OK" "$name" "$desc"; ok=$((ok+1))
  else
    line "--" "$name" "needs:$lacking"; missing=$((missing+1))
  fi
}

echo "Doblura image conformance"
echo "  python : $(python3 --version 2>&1)"
printf '  odoo   : '
if have odoo; then odoo --version 2>&1 | head -1; else echo "odoo NOT on PATH"; fi
printf '  import : '
# odoo.release rather than plain odoo: on Debian-packaged images the top-level
# odoo directory resolves as a namespace package, so `import odoo` succeeds
# without exposing anything useful. click-odoo needs the real module.
python3 -c 'import odoo.release as r; print("odoo", r.version, "importable")' 2>/dev/null \
  || echo "odoo NOT importable by plain python3 (click-odoo sets its own path)"
echo

echo "CORE (required for every rehearsal)"
cap core "odoo + click-odoo" odoo click-odoo
echo
echo "SNAPSHOT FORMATS (spec.snapshot.format)"
cap OdooBackup "restore and back up" click-odoo-restoredb click-odoo-backupdb
cap PgDump     "pg_restore path"     pg_restore createdb
cap PgPlain    "psql path"           psql createdb
echo
echo "MIGRATION ENGINES (spec.migration.engine)"
cap ClickOdooUpdate "checksum-based update" click-odoo-update
cap OdooUpdateAll   "odoo -u all"           odoo
cap Marabunta       "versioned migrations"   marabunta
echo
echo "OdooSnapshot PIPELINE (producing anonymized dumps)"
cap snapshot-copy "copy production into the work db" pg_dump pg_restore createdb dropdb
cap snapshot-sql  "SQL masking engine"               psql
cap greenmask     "Greenmask masking engine"         greenmask
echo
echo "HYGIENE"
if [ -w /tmp ]; then line "OK" "writable /tmp" "required with readOnlyRootFilesystem"
else line "--" "writable /tmp" "mount an emptyDir at /tmp"; missing=$((missing+1)); fi
if [ "$(id -u)" -ne 0 ]; then line "OK" "non-root default" "runs as $(id -u)"
else line "WARN" "non-root default" "image defaults to root; Doblura forces 65532"; fi
echo
echo "  $ok capabilities present, $missing missing"

# Only the core is fatal: everything else is a matter of what you declare.
if have odoo && have click-odoo; then
  echo "  CORE OK - this image can run Doblura rehearsals"
  exit 0
fi
echo "  CORE MISSING - Doblura cannot drive this image"
exit 1
