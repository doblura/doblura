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
	step := func(name, script string) corev1.Container {
		return corev1.Container{
			Name: name, Image: snap.Spec.Image,
			Command: []string{"/bin/sh", "-euc"}, Args: []string{script},
			Env: env, VolumeMounts: mounts, SecurityContext: sec, Resources: res,
		}
	}

	inits := []corev1.Container{
		step("1-copy", copyScript(work)),
		step("2-neutralize", neutralizeScript(work, "/etc/doblura/odoo.conf")),
		step("3-anonymize", anonymizeStep(snap, work)),
		step("4-dump", dumpScript(snap, work)),
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			// The NetworkPolicy selects on this label.
			Labels: map[string]string{"doblura.dev/snapshot": snap.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				RunAsUser:      ptr(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: inits,
			Containers:     []corev1.Container{step("5-upload", uploadScript(snap))},
			Volumes:        vols,
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
		return `echo ">> masking delegated to the custom image"`
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

func dumpScript(snap *doblurav1alpha1.OdooSnapshot, work string) string {
	if snap.Spec.Mask.Engine == doblurav1alpha1.EngineGreenmask || snap.Spec.Mask.Engine == "" {
		return `echo ">> dumping with greenmask (masks on the fly)"
greenmask --config /etc/doblura/greenmask.yaml dump
cp -r "$(greenmask --config /etc/doblura/greenmask.yaml list-dumps --format=json | head -1)" /scratch/dump || true
echo ">> dumped"`
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
