// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func intstrFromInt(i int32) intstr.IntOrString { return intstr.FromInt32(i) }

// snapshotPodTemplate builds the pipeline as a chain of init containers.
//
// The order IS the design:
//
//	1-copy         bring production into the work database
//	2-neutralize   cut the outbound paths BEFORE touching the data
//	3-anonymize    mask and (optionally) truncate
//	4-dump         write the result out
//	(main) upload  leave it at its destination
//
// Neutralizing at step 2 rather than step 3 is deliberate: if the pod dies
// halfway through, the work database must not sit for even a second with
// production mail servers configured and reachable.
func snapshotPodTemplate(snap *doblurav1alpha1.OdooSnapshot) corev1.PodTemplateSpec {
	work := workDBName(snap)
	res := sizeToResources(snap.Spec.Size)

	env := []corev1.EnvVar{
		{Name: "SRC_HOST", Value: snap.Spec.Source.Host},
		{Name: "SRC_DB", Value: snap.Spec.Source.Name},
		{Name: "SRC_USER", Value: snap.Spec.Source.User},
		{Name: "SRC_PASSWORD", ValueFrom: secretRef(snap.Spec.Source.PasswordSecret, "password")},
		{Name: "PGHOST", Value: snap.Spec.Work.Host},
		{Name: "PGUSER", Value: snap.Spec.Work.User},
		{Name: "PGDATABASE", Value: work},
		{Name: "PGPASSWORD", ValueFrom: secretRef(snap.Spec.Work.PasswordSecret, "password")},
	}

	vols := []corev1.Volume{
		{Name: "tmp", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "pipeline", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: snap.Name + "-pipeline"}}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "scratch", MountPath: "/scratch"},
		{Name: "pipeline", MountPath: "/etc/doblura", ReadOnly: true},
	}

	sec := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	// Which image runs each step.
	//
	// The masking step runs the CUSTOM image when there is one, because with
	// engine Custom the image IS the masking configuration. Everything around it
	// keeps the snapshot's own image: copying, neutralizing and dumping are
	// doblura's steps and they need doblura's contract, not somebody's masking
	// container.
	imageFor := func(name string) string {
		if name == "3-anonymize" &&
			snap.Spec.Mask.Engine == doblurav1alpha1.EngineCustom &&
			snap.Spec.Mask.CustomImage != "" {
			return snap.Spec.Mask.CustomImage
		}
		return snap.Spec.Image
	}

	step := func(name, script string) corev1.Container {
		return corev1.Container{
			Name: name, Image: imageFor(name),
			Command: []string{"/bin/sh", "-euc"}, Args: []string{script},
			Env: env, VolumeMounts: mounts, SecurityContext: sec, Resources: res,
		}
	}

	// THE DESTINATION, which was never mounted.
	//
	// The upload step ends with `cp -r /scratch/dump/. /snapshot/`, and no volume
	// was ever attached at /snapshot — so the copy landed on the container's own
	// read-only root filesystem and every snapshot to a Volume destination failed
	// at its last step, after copying production, neutralizing it, masking it and
	// dumping it. The four expensive steps worked and the result went nowhere.
	//
	// On the upload container only. An init step that could write the destination
	// could leave a partial dump there for something else to pick up as finished.
	uploadMounts := mounts
	if snap.Spec.To.Type == doblurav1alpha1.ProviderVolume && snap.Spec.To.Volume != nil {
		vols = append(vols, corev1.Volume{
			Name: "destination",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: snap.Spec.To.Volume.ClaimName,
				},
			},
		})
		uploadMounts = append(append([]corev1.VolumeMount{}, mounts...), corev1.VolumeMount{
			Name:      "destination",
			MountPath: doblurav1alpha1.SnapshotMountPath,
			SubPath:   snap.Spec.To.Volume.SubPath,
		})
	}

	dump := step("4-dump", dumpScript(snap, work))
	dump.TerminationMessagePath = maskReportPath
	// FallbackToLogsOnError: when the dump fails there is no report to write, and
	// the last lines of the log say what greenmask refused — which beats an empty
	// message on the one path that matters.
	dump.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError

	inits := []corev1.Container{
		step("1-copy", copyScript(work)),
		step("2-neutralize", neutralizeScript(work, "/etc/doblura/odoo.conf")),
		step("3-anonymize", anonymizeStep(snap, work)),
		dump,
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			// The NetworkPolicy selects on this label.
			Labels: map[string]string{"doblura.dev/snapshot": snap.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr(true),
				RunAsUser:    ptr(int64(65532)),
				// So the destination volume is writable by that user. Without it
				// the claim belongs to root and the upload fails with a permission
				// error one layer below the one it looks like.
				FSGroup:        ptr(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: inits,
			Containers: []corev1.Container{func() corev1.Container {
				c := step("5-upload", uploadScript(snap))
				c.VolumeMounts = uploadMounts
				return c
			}()},
			Volumes: vols,
		},
	}
}

func copyScript(work string) string {
	// --no-owner --no-acl: the work database has a different owner than
	// production, and without these the restore spews hundreds of permission
	// errors.
	return `echo ">> copying source into the work database"
dropdb --if-exists "` + work + `"
createdb "` + work + `"
PGPASSWORD="$SRC_PASSWORD" pg_dump -h "$SRC_HOST" -U "$SRC_USER" -d "$SRC_DB" \
  --format=custom --no-owner --no-acl -f /scratch/origen.dump
pg_restore -d "` + work + `" --no-owner --no-acl /scratch/origen.dump
rm -f /scratch/origen.dump
echo ">> copy ready"`
}

func anonymizeStep(snap *doblurav1alpha1.OdooSnapshot, work string) string {
	switch snap.Spec.Mask.Engine {
	case doblurav1alpha1.EngineSQL:
		return `echo ">> masking with SQL"
sh /etc/doblura/mask.sh
echo ">> masked"`
	case doblurav1alpha1.EngineCustom:
		// The image is what does the work here, so the step says which one ran.
		// It said "delegated to the custom image" while every container in this
		// pod ran the SNAPSHOT's image — mask.customImage was declared, described
		// as "the image used when Engine is Custom", and read by nobody. Somebody
		// choosing Custom got their own masking silently not applied, and a dump
		// that looks anonymised and is not is the worst artefact this pipeline can
		// produce.
		return `echo ">> masking delegated to this image, which is the custom one"`
	default:
		// With greenmask the masking happens DURING the dump, so all that is left
		// here is what greenmask does not do: resetting passwords.
		s := `echo ">> greenmask masks during the dump"` + "\n"
		if snap.Spec.Mask.ResetPasswords == nil || *snap.Spec.Mask.ResetPasswords {
			s += `psql -v ON_ERROR_STOP=1 -c "UPDATE res_users SET password = '` +
				doblurav1alpha1.PasswordResetValue + `';"` + "\n"
		}
		return s
	}
}

// greenmaskPrune drops masking rules for tables this database does not have.
//
// greenmask treats a rule naming a missing table as FATAL — five of them, and it
// refuses to dump at all. doblura's default rule set covers crm_lead,
// hr_employee, mail_message and the rest, and plenty of real installations have
// never installed CRM or HR. Without this, a snapshot of such a database could
// never succeed, and the message would be about a table nobody wrote down.
//
// It happens HERE and not when the config is generated, because only the pod can
// ask: the manager holds no database credential, and that is deliberate.
//
// It PRINTS what it dropped. A filter that quietly removes masking rules is a
// filter that quietly stops anonymizing a column, and the whole point of this
// pipeline is that somebody can say what was masked.
// maskReportPath is where the prune step leaves what it decided.
//
// Inside /tmp, which this pod mounts writable: the root filesystem is read-only
// and the default termination path (/dev/termination-log) is on it — the same
// trap the addons clone hit.
//
// What travels this way is a DATA-PROTECTION FACT, not a log line: which columns
// this run did not mask. It was printed to a Job's log, where it lives as long as
// the pod does and is visible to whoever thinks to look. On the object it can be
// read months later by somebody asking what was in that dump.
const maskReportPath = "/tmp/masking-report"

const greenmaskPrune = `python3 - <<'PRUNE'
import yaml, subprocess, sys

cfg = "/etc/doblura/greenmask.yaml"
with open(cfg) as fh:
    doc = yaml.safe_load(fh)

have = set(subprocess.run(
    ["psql", "-tAX", "-c",
     "SELECT tablename FROM pg_tables WHERE schemaname = 'public'"],
    capture_output=True, text=True, check=True).stdout.split())

# Columns too, and for the same reason. Odoo's schema differs by version and by
# which modules are installed: res_partner.email_normalized exists in some
# majors and not others, and greenmask treats a rule naming an absent column as
# fatal exactly as it does an absent table.
cols = {}
for line in subprocess.run(
        ["psql", "-tAX", "-F", "\t", "-c",
         "SELECT table_name, column_name FROM information_schema.columns "
         "WHERE table_schema = 'public'"],
        capture_output=True, text=True, check=True).stdout.splitlines():
    if "\t" in line:
        t, c = line.split("\t", 1)
        cols.setdefault(t, set()).add(c)

def wanted(tr, table):
    """The columns one transformer touches: 'column', or 'columns[].name'."""
    p = tr.get("params") or {}
    if "column" in p:
        return [p["column"]]
    return [c.get("name") for c in (p.get("columns") or []) if isinstance(c, dict)]

dropped_cols = []

def prune_columns(rule):
    table = rule.get("name")
    kept = []
    for tr in rule.get("transformers") or []:
        missing = [c for c in wanted(tr, table) if c not in cols.get(table, set())]
        if missing:
            dropped_cols.extend(table + "." + c for c in missing)
            continue
        kept.append(tr)
    rule["transformers"] = kept
    return rule

rules = doc.get("dump", {}).get("transformation") or []
keep = [prune_columns(r) for r in rules if r.get("name") in have]
keep = [r for r in keep if r.get("transformers")]
gone = sorted({r.get("name") for r in rules if r.get("name") not in have})
if gone:
    print(">> not in this database, so not masked: " + ", ".join(gone))
if dropped_cols:
    print(">> columns not in this database, so not masked: " + ", ".join(sorted(dropped_cols)))

# And back to the object, not only to this log. The size is added after the dump,
# because that is when there is one.
import json
with open("/tmp/prune.json", "w") as fh:
    json.dump({"tables": sorted(gone), "columns": sorted(set(dropped_cols)),
               "masked": sum(len(r.get("transformers") or []) for r in keep)}, fh)
if not keep and rules:
    print("!! none of the tables this snapshot masks exist in the source database.",
          file=sys.stderr)
    print("!! that is not an anonymized dump, it is a dump. Refusing.", file=sys.stderr)
    sys.exit(1)
doc["dump"]["transformation"] = keep
with open("/tmp/greenmask.yaml", "w") as fh:
    yaml.safe_dump(doc, fh, sort_keys=False)
PRUNE
`

func dumpScript(snap *doblurav1alpha1.OdooSnapshot, work string) string {
	if snap.Spec.Mask.Engine == doblurav1alpha1.EngineGreenmask || snap.Spec.Mask.Engine == "" {
		// The collection step is not `|| true`. It was, and it hid the only
		// failure that matters here: if the dump is not where the next step looks,
		// the snapshot reports success and produces nothing — the exact shape of
		// silence this project keeps removing. greenmask writes one directory per
		// dump under its storage path; the newest one is this run's.
		return `echo ">> dumping with greenmask (masks on the fly)"
mkdir -p ` + greenmaskOut + `
` + greenmaskPrune + `
greenmask --config /tmp/greenmask.yaml dump
d=$(ls -1dt ` + greenmaskOut + `/*/ 2>/dev/null | head -1)
if [ -z "$d" ]; then echo "greenmask reported success and produced no dump" >&2; exit 1; fi
mv "$d" /scratch/dump
sz=$(du -sb /scratch/dump | cut -f1)
python3 -c "import json; d=json.load(open('/tmp/prune.json')); d['size_bytes']=int('$sz'); json.dump(d, open('` + maskReportPath + `','w'))"
echo ">> dumped, $sz bytes"`
	}
	// With SQL or Custom the database is already clean: a plain pg_dump, with the
	// volume tables left out through --exclude-table-data.
	s := `echo ">> dumping"
pg_dump --format=directory --no-owner --no-acl \`
	for _, t := range snap.Spec.TablesToTruncate() {
		s += "\n  --exclude-table-data=public." + t + ` \`
	}
	s += "\n  -f /scratch/dump \"" + work + "\"\necho \">> volcado\""
	return s
}

func uploadScript(snap *doblurav1alpha1.OdooSnapshot) string {
	tail := `
echo ">> dropping the work database"
dropdb --if-exists "` + workDBName(snap) + `"`

	switch snap.Spec.To.Type {
	case doblurav1alpha1.ProviderVolume:
		return `echo ">> copying the dump to the destination volume"
cp -r /scratch/dump/. "` + doblurav1alpha1.SnapshotMountPath + `/"` + tail
	case doblurav1alpha1.ProviderObjectStore:
		return `echo ">> uploading the dump to the object store"
rclone --config /dev/null copy /scratch/dump "S:` +
			snap.Spec.To.ObjectStore.Bucket + "/" + snap.Spec.To.ObjectStore.Key + `"` + tail
	default:
		return `echo ">> destination delegated to the custom image"` + tail
	}
}
