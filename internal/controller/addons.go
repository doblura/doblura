// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// addonsPlumbing returns the volumes, mounts and init containers needed for the
// composed addons path to actually exist inside the pod.
//
// Project invariant: NONE of this copies addons into a persistent volume. Repos
// go into an emptyDir that is born empty in every pod; baked addons are read
// where they are; the PVC is mounted ReadOnly. See the long comment in
// api/v1alpha1/addons_types.go.
func addonsPlumbing(a *doblurav1alpha1.AddonsSpec) (
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
	inits []corev1.Container,
) {
	if len(a.Repos) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: "addons-repos",
			// emptyDir, not a PVC. Ephemeral on purpose: no state to mix
			// between restarts or between pods.
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: qty("4Gi")},
			},
		})
		// The main container sees it read-only: if the rehearsal's code can
		// modify itself, the rehearsal is worthless.
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "addons-repos",
			MountPath: doblurav1alpha1.AddonRepoMountBase,
			ReadOnly:  true,
		})

		for _, r := range a.Repos {
			inits = append(inits, cloneContainer(r))
		}
	}

	if a.Volume != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "addons-volume",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: a.Volume.ClaimName,
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "addons-volume",
			MountPath: doblurav1alpha1.AddonVolumeMountPath,
			ReadOnly:  true,
		})
	}

	return volumes, mounts, inits
}

// cloneContainer builds the init container that clones one repo.
func cloneContainer(r doblurav1alpha1.AddonRepo) corev1.Container {
	env := authEnv(r)

	return corev1.Container{
		Name:    "clone-" + r.Name,
		Image:   gitImage(),
		Command: []string{"/bin/sh", "-euc"},
		Args:    []string{cloneScript(r)},
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			// Writable here: this is the init container, it is its job.
			{Name: "addons-repos", MountPath: doblurav1alpha1.AddonRepoMountBase},
			{Name: "tmp", MountPath: "/tmp"},
		},
		TerminationMessagePath: cloneRevisionPath,
		// FallbackToLogsOnError and not the default: when the clone fails there
		// is no revision to write, and the last lines of the log say which repo
		// and why — which beats an empty message on the one path that matters.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
}

// cloneRevisionPath is inside /tmp, which the clone container already mounts
// writable — the root filesystem is read-only, and the default termination path
// (/dev/termination-log) is on it.
const cloneRevisionPath = "/tmp/revision"

// authEnv translates the declared GitAuth into environment variables.
//
// Each type injects only its own keys: no marking everything optional and
// letting the script guess. If the Secret lacks the key the type requires, the
// pod fails to start with a message naming exactly what is missing, instead of
// an "authentication failed" twenty seconds later.
func authEnv(r doblurav1alpha1.AddonRepo) []corev1.EnvVar {
	a := r.Auth
	if a == nil {
		return nil
	}

	ref := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: a.SecretRef},
			Key:                  key,
		}}
	}

	switch a.Type {
	case doblurav1alpha1.AuthToken:
		user := a.Username
		if user == "" {
			user = doblurav1alpha1.DefaultTokenUserFor(r.URL)
		}
		return []corev1.EnvVar{
			{Name: "GIT_USER", Value: user},
			{Name: "GIT_PASSWORD", ValueFrom: ref("token")},
		}

	case doblurav1alpha1.AuthBasicAuth:
		return []corev1.EnvVar{
			{Name: "GIT_USER", ValueFrom: ref("username")},
			{Name: "GIT_PASSWORD", ValueFrom: ref("password")},
		}

	case doblurav1alpha1.AuthSSHKey:
		return []corev1.EnvVar{
			{Name: "GIT_SSH_KEY", ValueFrom: ref("ssh-privatekey")},
			{Name: "GIT_KNOWN_HOSTS", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: a.SecretRef},
					Key:                  "known_hosts",
					// Genuinely optional: without known_hosts we fall back to
					// accept-new, which is acceptable inside a cluster.
					Optional: ptr(true),
				},
			}},
		}

	case doblurav1alpha1.AuthGitHubApp:
		// The operator already minted the installation token and left it in an
		// ephemeral Secret it owns. From here it is just a token.
		return []corev1.EnvVar{
			{Name: "GIT_USER", Value: doblurav1alpha1.GitHubTokenUser},
			{Name: "GIT_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: MintedTokenSecretName(r.Name)},
					Key:                  "token",
				},
			}},
		}
	}
	return nil
}

func cloneScript(r doblurav1alpha1.AddonRepo) string {
	dest := doblurav1alpha1.AddonRepoMountBase + "/" + r.Name
	depth := ""
	if r.Depth > 0 {
		depth = fmt.Sprintf("--depth %d", r.Depth)
	}

	var b strings.Builder
	// pipefail, because every git command here is piped through the sed that
	// obfuscates credentials — and without it the exit status is SED's, which is
	// always zero. A failed fetch went unnoticed under `set -e` and the script
	// carried on to check out a FETCH_HEAD that did not exist, reporting the
	// confusing "--detach does not take a path argument" instead of the network
	// error that actually happened.
	b.WriteString("set -o pipefail\n")
	b.WriteString("export HOME=/tmp\n")
	b.WriteString(`URL="` + r.URL + `"` + "\n")

	// Authentication. Credentials are injected at run time and appear neither in
	// the manifest nor in the logs: all git output goes through a sed that
	// obfuscates the URL's userinfo.
	b.WriteString(`if [ -n "${GIT_PASSWORD:-}" ]; then` + "\n")
	// credential.helper over stdin rather than putting the secret in the URL:
	// that way it never lands in .git/config, which outlives the init
	// container.
	b.WriteString(`  git config --global credential.helper '!f() { echo "username=${GIT_USER}"; echo "password=${GIT_PASSWORD}"; }; f'` + "\n")
	b.WriteString(`elif [ -n "${GIT_SSH_KEY:-}" ]; then` + "\n")
	b.WriteString(`  mkdir -p /tmp/.ssh && printf '%s\n' "$GIT_SSH_KEY" > /tmp/.ssh/id && chmod 600 /tmp/.ssh/id` + "\n")
	b.WriteString(`  if [ -n "${GIT_KNOWN_HOSTS:-}" ]; then` + "\n")
	b.WriteString(`    printf '%s\n' "$GIT_KNOWN_HOSTS" > /tmp/.ssh/known_hosts` + "\n")
	b.WriteString(`    export GIT_SSH_COMMAND="ssh -i /tmp/.ssh/id -o UserKnownHostsFile=/tmp/.ssh/known_hosts"` + "\n")
	b.WriteString(`  else` + "\n")
	b.WriteString(`    export GIT_SSH_COMMAND="ssh -i /tmp/.ssh/id -o StrictHostKeyChecking=accept-new"` + "\n")
	b.WriteString(`  fi` + "\n")
	b.WriteString("fi\n")

	// init + fetch, not clone.
	//
	// `git clone --branch` cannot take a commit, so this used to fall back to a
	// FULL clone and then check the commit out. That made the one thing the API
	// documentation tells you to do — pin a rehearsal to a commit — by far the
	// most expensive. Measured on OCA/server-tools: 149 MB and 22 seconds for the
	// full clone against 4.4 MB for a shallow fetch of the same commit, per repo,
	// on every environment. A customer with eight repos paid that eight times.
	//
	// `git fetch --depth` takes a branch, a tag OR a bare commit against any
	// server that allows reachable-SHA1-in-want, which GitHub, GitLab and Gitea
	// all do. So one path serves all three kinds of ref and the fallback is gone
	// — and with it the case that was fast in testing and slow in production,
	// because a branch name is what you type while trying things out.
	b.WriteString(fmt.Sprintf(`mkdir -p "%s" && git -C "%s" init -q`+"\n", dest, dest))
	b.WriteString(fmt.Sprintf(`git -C "%s" remote add origin "$URL"`+"\n", dest))
	b.WriteString(fmt.Sprintf(`git -C "%s" fetch -q %s origin "%s" 2>&1 | sed -E "s#://[^@]*@#://***@#g" || {`+"\n", dest, depth, r.Ref))
	// A server that refuses bare SHAs is the one case left. Say so precisely
	// rather than retrying blindly: the fix is a branch or a tag, and the person
	// reading this needs to know that is why.
	b.WriteString(fmt.Sprintf(`  echo ">> could not fetch %s from %s." >&2`+"\n", r.Ref, r.Name))
	b.WriteString(`  echo ">> If that is a commit, this server may not serve commits directly;" >&2` + "\n")
	b.WriteString(`  echo ">> use a branch or tag, or raise depth to reach it by history." >&2` + "\n")
	b.WriteString("  exit 1\n")
	b.WriteString("}\n")
	// checkout FETCH_HEAD, without --detach: git reads `--detach FETCH_HEAD` as a
	// branch plus a path, and says so in a message about paths.
	b.WriteString(fmt.Sprintf(`git -C "%s" checkout -q FETCH_HEAD`+"\n", dest))
	// Record the exact revision: in a rehearsal that is the difference between a
	// reproducible result and an anecdote.
	b.WriteString(fmt.Sprintf(`echo ">> %s at $(git -C "%s" rev-parse HEAD)"`+"\n", r.Name, dest))

	// And write it where it OUTLIVES the pod.
	//
	// The line above goes to the Job's log, which is gone the moment the Job is
	// cleaned up — so "which commit was this environment actually running" was
	// answerable only for as long as nobody tidied. Kubernetes keeps a container's
	// termination message in the pod status, so the operator can read it and put
	// it in the environment's own status, where it belongs.
	//
	// The message is name=sha, one per line, because an init container has one
	// message and this is the smallest thing that survives being parsed.
	b.WriteString(fmt.Sprintf(`printf '%%s=%%s\n' "%s" "$(git -C "%s" rev-parse HEAD)" > %s`+"\n",
		r.Name, dest, cloneRevisionPath))
	return b.String()
}

// odooConf generates the configuration the three phases will use.
//
// Generated here rather than requiring the image to ship an odoo.conf, so the
// contract with the user's image stays minimal. And it goes into a visible
// ConfigMap: `kubectl get cm` tells you the EXACT addons path the rehearsal ran
// with, which is the first question when a module does not load.
func odooConf(reh *doblurav1alpha1.OdooRehearsal, dbName string) string {
	paths := reh.Spec.Addons.AddonsPathFor(doblurav1alpha1.AddonRepoMountBase)

	var b strings.Builder
	b.WriteString("[options]\n")
	if len(paths) > 0 {
		b.WriteString("addons_path = " + strings.Join(paths, ",") + "\n")
	}
	// data_dir must be explicit and writable.
	//
	// Odoo defaults it to $HOME/.local/share/Odoo, and the pod runs as UID 65532
	// which has no home directory in most images, so HOME resolves to "/" and the
	// path lands on a read-only root filesystem. The restore then dies on the
	// filestore with a FileNotFoundError that names a path nobody configured.
	b.WriteString("data_dir = " + doblurav1alpha1.DataDirPath + "\n")
	b.WriteString(fmt.Sprintf("db_host = %s\n", reh.Spec.Database.ConnectHost()))
	b.WriteString(fmt.Sprintf("db_port = %d\n", reh.Spec.Database.ConnectPort()))
	b.WriteString(fmt.Sprintf("db_user = %s\n", reh.Spec.Database.User))
	b.WriteString(fmt.Sprintf("db_name = %s\n", dbName))
	b.WriteString("list_db = False\n")
	// The password does NOT go here: it travels via PGPASSWORD from a Secret.
	return b.String()
}
