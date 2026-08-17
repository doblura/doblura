#!/usr/bin/env bash
# Validate the greenmask config Doblura generates, using greenmask itself.
#
# Why this exists: every Go test asserted on substrings, and a config where every
# transformer name was missing its closing quote passed all of them. It was
# invalid YAML from the first commit and greenmask would have refused to load it.
# A parser catches that; only the real tool catches a wrong SCHEMA — putting
# subset_conds at the dump level instead of on the table entry, for instance.
#
# It connects to no database, so it stops at the connection error. Everything
# before that is what we care about: config decoding.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=${GREENMASK_IMAGE:-greenmask/greenmask:latest}
OUT=$(mktemp -d)/greenmask.yaml

cat > /tmp/doblura-gmgen_test.go <<'GO'
package controller

import (
	"os"
	"testing"

	dobluv1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func TestZZGenerateGreenmaskConfig(t *testing.T) {
	yes := true
	for name, spec := range map[string]*dobluv1alpha1.OdooSnapshotSpec{
		"plain":            {},
		"truncate":         {Truncate: dobluv1alpha1.TruncateSpec{Preset: &yes}},
		"subset":           {Subset: &dobluv1alpha1.SubsetSpec{Companies: []string{"Acme ES", "O'Brien Ltd"}, AcknowledgeSharedMasterData: dobluv1alpha1.AckSharedMasterData}},
		"truncate+subset":  {Truncate: dobluv1alpha1.TruncateSpec{Preset: &yes}, Subset: &dobluv1alpha1.SubsetSpec{Companies: []string{"Acme"}, AcknowledgeSharedMasterData: dobluv1alpha1.AckSharedMasterData}},
	} {
		if err := os.WriteFile(os.Getenv("DOBLURA_GM_OUT")+"."+name+".yaml",
			[]byte(greenmaskConfig(spec, "work")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
GO
cp /tmp/doblura-gmgen_test.go internal/controller/zz_gmgen_test.go
DOBLURA_GM_OUT="$OUT" go test ./internal/controller/ -run TestZZGenerateGreenmaskConfig >/dev/null
rm -f internal/controller/zz_gmgen_test.go

fails=0
for f in "$OUT".*.yaml; do
  name=$(basename "$f" | sed 's/greenmask.yaml.//; s/.yaml$//')
  out=$(docker run --rm -v "$f:/cfg.yaml:ro" "$IMAGE" --config /cfg.yaml validate 2>&1 || true)
  if echo "$out" | grep -qiE 'invalid keys|decoding failed|cannot unmarshal|yaml:'; then
    printf '  FAIL  %-16s %s\n' "$name" "$(echo "$out" | grep -oiE '(invalid keys|decoding failed|yaml:).*' | head -1)"
    fails=$((fails+1))
  else
    printf '  ok    %-16s config accepted by greenmask\n' "$name"
  fi
done
[ $fails -eq 0 ] && echo "  greenmask accepts every generated config" || { echo "  $fails config(s) rejected"; exit 1; }
