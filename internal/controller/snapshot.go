// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// snapshotPlumbing translates the declared provider into pod plumbing.
//
// Every path ends in the same place: a volume mounted at /snapshot with the dump
// inside. Volume gets there by mounting a PVC; everything else through an init
// container that downloads the bytes into an emptyDir.
//
// That uniformity is the point: the rest of the operator does not know which
// provider the dump came from, and does not need to.
func snapshotPlumbing(s *doblurav1alpha1.SnapshotSpec) (
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
	inits []corev1.Container,
) {
	p := &s.From

	if !p.NeedsFetchContainer() {
		// Volume: the dump is already there. ReadOnly so one rehearsal cannot
		// corrupt the dump the next ones depend on.
		v := p.Volume
		volumes = append(volumes, corev1.Volume{
			Name: "snapshot",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: v.ClaimName,
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "snapshot",
			MountPath: doblurav1alpha1.SnapshotMountPath,
			SubPath:   v.SubPath,
			ReadOnly:  true,
		})
		return volumes, mounts, nil
	}

	// Everything else: an emptyDir the fetcher can write to, ReadOnly for all
	// other containers.
	volumes = append(volumes, corev1.Volume{
		Name: "snapshot",
		VolumeSource: corev1.VolumeSource{
			// No sizeLimit: a production dump can be tens of gigabytes, and a
			// low limit here produces an eviction failure that explains
			// nothing. What bounds this is the node's disk.
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      "snapshot",
		MountPath: doblurav1alpha1.SnapshotMountPath,
		ReadOnly:  true,
	})
	inits = append(inits, fetchContainer(p))
	return volumes, mounts, inits
}

// fetchContainer builds the container that brings the dump in.
//
// The three built-ins are presets of exactly what Custom does: an image, a
// command, and the commitment to leave the dump at /snapshot.
func fetchContainer(p *doblurav1alpha1.SnapshotProvider) corev1.Container {
	c := corev1.Container{
		Name: "fetch-snapshot",
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			// Writable here: that is its job. For every other container,
			// /snapshot is ReadOnly.
			{Name: "snapshot", MountPath: doblurav1alpha1.SnapshotMountPath},
			{Name: "tmp", MountPath: "/tmp"},
		},
	}

	switch p.Type {
	case doblurav1alpha1.ProviderObjectStore:
		o := p.ObjectStore
		// rclone rather than a vendor CLI: one binary, every S3-compatible
		// store. The alternative was one image per cloud.
		c.Image = objectStoreImage()
		c.Command = []string{"/bin/sh", "-euc"}
		c.Args = []string{objectStoreScript(o)}
		c.Env = objectStoreEnv(o)

	case doblurav1alpha1.ProviderHTTP:
		h := p.HTTP
		c.Image = httpImage()
		c.Command = []string{"/bin/sh", "-euc"}
		c.Args = []string{httpScript(h)}
		if h.AuthSecret != "" {
			c.Env = []corev1.EnvVar{
				{Name: "HTTP_USER", ValueFrom: optionalSecretRef(h.AuthSecret, "username")},
				{Name: "HTTP_PASSWORD", ValueFrom: optionalSecretRef(h.AuthSecret, "password")},
				{Name: "HTTP_BEARER", ValueFrom: optionalSecretRef(h.AuthSecret, "bearer")},
			}
		}

	case doblurav1alpha1.ProviderCustom:
		cu := p.Custom
		c.Image = cu.Image
		c.Command = cu.Command
		c.Args = cu.Args
		for k, v := range cu.Env {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: v})
		}
		for _, sec := range cu.EnvFromSecrets {
			c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: sec},
				},
			})
		}
		for _, claim := range cu.ExtraVolumeClaims {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      "extra-" + claim,
				MountPath: "/mnt/" + claim,
				ReadOnly:  true,
			})
		}
	}
	return c
}

// customExtraVolumes are the additional PVCs a CustomProvider asks for.
// They are returned separately because they belong to the pod, not the
// container.
func customExtraVolumes(p *doblurav1alpha1.SnapshotProvider) []corev1.Volume {
	if p.Type != doblurav1alpha1.ProviderCustom || p.Custom == nil {
		return nil
	}
	var out []corev1.Volume
	for _, claim := range p.Custom.ExtraVolumeClaims {
		out = append(out, corev1.Volume{
			Name: "extra-" + claim,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim,
					ReadOnly:  true,
				},
			},
		})
	}
	return out
}

func objectStoreEnv(o *doblurav1alpha1.ObjectStoreProvider) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "RCLONE_CONFIG_S_TYPE", Value: "s3"},
		{Name: "RCLONE_CONFIG_S_PROVIDER", Value: "Other"},
		{Name: "RCLONE_CONFIG_S_REGION", Value: o.Region},
	}
	if o.Endpoint != "" {
		env = append(env, corev1.EnvVar{Name: "RCLONE_CONFIG_S_ENDPOINT", Value: o.Endpoint})
	}
	if o.ForcePathStyle != nil && *o.ForcePathStyle {
		env = append(env, corev1.EnvVar{Name: "RCLONE_CONFIG_S_FORCE_PATH_STYLE", Value: "true"})
	}
	if o.CredentialsSecret != "" {
		env = append(env,
			corev1.EnvVar{Name: "RCLONE_CONFIG_S_ACCESS_KEY_ID",
				ValueFrom: secretRef(o.CredentialsSecret, "accessKeyID")},
			corev1.EnvVar{Name: "RCLONE_CONFIG_S_SECRET_ACCESS_KEY",
				ValueFrom: secretRef(o.CredentialsSecret, "secretAccessKey")},
		)
	} else {
		// No Secret: credentials from the environment. That is what you want
		// with IRSA on EKS or Workload Identity, where there are no keys to
		// rotate.
		env = append(env, corev1.EnvVar{Name: "RCLONE_CONFIG_S_ENV_AUTH", Value: "true"})
	}
	return env
}

func objectStoreScript(o *doblurav1alpha1.ObjectStoreProvider) string {
	// copyto for a single object, copy for a prefix. Decided by whether the key
	// ends in a slash, which is everyone's convention for prefixes.
	verb := "copyto"
	if strings.HasSuffix(o.Key, "/") {
		verb = "copy"
	}
	return fmt.Sprintf(`echo ">> downloading s3://%[1]s/%[2]s"
rclone --config /dev/null %[3]s "S:%[1]s/%[2]s" "%[4]s" --progress
ls -la "%[4]s"`, o.Bucket, strings.TrimSuffix(o.Key, "/"), verb, doblurav1alpha1.SnapshotMountPath)
}

func httpScript(h *doblurav1alpha1.HTTPProvider) string {
	dest := doblurav1alpha1.SnapshotMountPath + "/dump"
	return fmt.Sprintf(`AUTH=""
if [ -n "${HTTP_BEARER:-}" ]; then
  AUTH="-H Authorization:Bearer\ ${HTTP_BEARER}"
elif [ -n "${HTTP_USER:-}" ]; then
  AUTH="-u ${HTTP_USER}:${HTTP_PASSWORD}"
fi
echo ">> downloading the dump over HTTP"
# --fail so a 404 is an error rather than a file containing an error page,
# which is a far more confusing failure to diagnose.
curl --fail --location --show-error --silent $AUTH -o "%s" "%s"
ls -la "%s"`, dest, h.URL, doblurav1alpha1.SnapshotMountPath)
}

func secretRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
	}}
}

func optionalSecretRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
		Optional:             ptr(true),
	}}
}
