// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The build.
//
// It runs in the CUSTOMER'S cluster, which is decision 6's resolution of its own
// collision with decision 2: building means seeing the source, and a control
// plane that sees only metadata cannot see source. What crosses the boundary is
// the digest.
//
// Unprivileged, and that was measured rather than hoped for: buildah as uid 1000
// with allowPrivilegeEscalation false and every capability dropped except SETUID
// and SETGID, vfs storage, chroot isolation. That set runs under a restricted Pod
// Security Standard, which is where an operator belongs.
type OdooBuildReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// buildDigestPath is where the builder leaves the digest it pushed.
//
// Inside /tmp, which the build container mounts writable: the default
// /dev/termination-log lives on a read-only root filesystem here.
const buildDigestPath = "/tmp/pushed-digest"

// addonsInImage is where a build puts the repositories it copied in.
//
// One directory per repository, and NOT flattened together: two repositories with
// a module of the same name are a thing that happens — a customer's fix on top of
// an OCA module is exactly that — and flattening makes which one wins depend on
// copy order rather than on the addons path, where it is at least visible.
const addonsInImage = "/opt/doblura/addons"

// The RBAC markers are a FREE-FLOATING comment, with a blank line under them.
// They are package-scoped, and controller-gen does not pick them up from a
// declaration's doc comment — CONTRIBUTING.md says so, and this file had them
// attached to the reconciler type and generated no rule at all. Everything built,
// everything passed, and the manager would have been denied on its own kind.

// +kubebuilder:rbac:groups=doblura.dev,resources=odoobuilds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=doblura.dev,resources=odoobuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoobuilds/finalizers,verbs=update

func (r *OdooBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b doblurav1alpha1.OdooBuild
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := b.Status.DeepCopy()
	st.ObservedGeneration = b.Generation

	name := b.Name + "-build"
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &job)
	switch {
	case apierrors.IsNotFound(err):
		j := r.job(&b, name)
		if err := ctrl.SetControllerReference(&b, j, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		st.Phase = doblurav1alpha1.BuildCloning
		st.Message = "cloning and building"
	case err != nil:
		return ctrl.Result{}, err
	default:
		r.readBack(ctx, &b, st, name, &job)
	}

	if equality.Semantic.DeepEqual(&b.Status, st) {
		return ctrl.Result{}, nil
	}
	b.Status = *st
	return ctrl.Result{}, r.Status().Update(ctx, &b)
}

// readBack turns the Job and its pod into a status.
func (r *OdooBuildReconciler) readBack(
	ctx context.Context,
	b *doblurav1alpha1.OdooBuild,
	st *doblurav1alpha1.OdooBuildStatus,
	name string,
	job *batchv1.Job,
) {
	// The sources first: they are known as soon as the clones finish, and they
	// are the answer to "which code is in this image" — which somebody may need
	// even for a build that then failed to push.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(b.Namespace),
		client.MatchingLabels{"job-name": name}); err == nil {
		for i := range pods.Items {
			st.Sources = sourcesFrom(b, &pods.Items[i])
			if d := digestFrom(&pods.Items[i]); d != "" {
				st.Image = b.Spec.To.Image + "@" + d
			}
		}
	}

	switch {
	case job.Status.Succeeded > 0:
		st.Phase = doblurav1alpha1.BuildSucceeded
		if st.Image == "" {
			// Succeeded with nothing to show for it. Never reported as a plain
			// success: an image nobody can name is an image nobody can use.
			st.Phase = doblurav1alpha1.BuildFailed
			st.Message = "the build finished without reporting a digest; check the logs of Job " + name
			break
		}
		st.Message = "built and pushed"
		if st.BuiltAt == nil && job.Status.CompletionTime != nil {
			st.BuiltAt = job.Status.CompletionTime
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Built", Status: metav1.ConditionTrue, Reason: "JobCompleted",
			Message: st.Image, ObservedGeneration: b.Generation,
		})
	case job.Status.Failed > 0:
		st.Phase = doblurav1alpha1.BuildFailed
		st.Message = "the build failed; check the logs of Job " + name
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Built", Status: metav1.ConditionFalse, Reason: "JobFailed",
			Message: "Job " + name + " failed", ObservedGeneration: b.Generation,
		})
	default:
		st.Phase = doblurav1alpha1.BuildBuilding
		st.Message = "building"
	}
}

// sourcesFrom reads what each clone resolved to.
func sourcesFrom(b *doblurav1alpha1.OdooBuild, pod *corev1.Pod) []doblurav1alpha1.BuiltSource {
	byName := commitsIn(pod)
	out := make([]doblurav1alpha1.BuiltSource, 0, len(b.Spec.Repos))
	for _, repo := range b.Spec.Repos {
		out = append(out, doblurav1alpha1.BuiltSource{
			Name: repo.Name, URL: repo.URL, Ref: repo.Ref, Commit: byName[repo.Name],
		})
	}
	return out
}

// commitsIn reads what each clone container recorded.
//
// The clone writes "<name>=<sha>", which is the format addons.go has used since
// it existed. The first version of this took the last whitespace-separated field
// and threw away anything longer than 40 characters — so it threw away
// "mis-builder=f988ae69…" every time, and every build recorded an empty commit
// while reporting success. The field exists precisely so that "which code is in
// this image" has an answer; an empty one is the question unanswered.
//
// FallbackToLogsOnError means the message may be log lines instead, so each line
// is examined rather than assumed.
func commitsIn(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated == nil {
			continue
		}
		for _, line := range strings.Split(cs.State.Terminated.Message, "\n") {
			name, sha, found := strings.Cut(strings.TrimSpace(line), "=")
			if !found || name == "" || len(sha) != 40 {
				continue
			}
			out[name] = sha
		}
	}
	return out
}

// digestFrom reads the digest the builder pushed.
func digestFrom(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "build" || cs.State.Terminated == nil {
			continue
		}
		for _, line := range strings.Fields(cs.State.Terminated.Message) {
			if strings.HasPrefix(line, "sha256:") {
				return line
			}
		}
	}
	return ""
}

// tagFor is the tag a build pushes to when none was declared.
//
// Derived from the base image and the refs asked for, so two builds of the same
// declaration land on the same tag and a build of a different declaration cannot
// silently overwrite it. Short, because it ends up in `kubectl get` and in a pod
// spec somebody reads.
func tagFor(b *doblurav1alpha1.OdooBuild) string {
	if b.Spec.To.Tag != "" {
		return b.Spec.To.Tag
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", b.Spec.From)
	for _, r := range b.Spec.Repos {
		fmt.Fprintf(h, "%s %s %s\n", r.Name, r.URL, r.Ref)
	}
	return "doblura-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// buildScript generates the Dockerfile and runs buildah.
//
// The Dockerfile is generated rather than taken from the customer's repository,
// and that is the point: the base image guarantees click-odoo-contrib, greenmask
// and the Postgres client, and a Dockerfile somebody else owns is a place where
// that guarantee is quietly broken — discovered on the day a restore is needed.
func buildScript(b *doblurav1alpha1.OdooBuild) string {
	ref := b.Spec.To.Image + ":" + tagFor(b)

	var paths []string
	var copies strings.Builder
	for _, r := range b.Spec.Repos {
		dest := addonsInImage + "/" + r.Name
		paths = append(paths, dest)
		copies.WriteString("COPY --chown=root:root " + r.Name + " " + dest + "\n")
	}

	tls := "--tls-verify=true"
	if b.Spec.To.Insecure != nil && *b.Spec.To.Insecure {
		tls = "--tls-verify=false"
	}

	var s strings.Builder
	s.WriteString("set -euo pipefail\n")
	s.WriteString("cd " + doblurav1alpha1.AddonRepoMountBase + "\n")
	// The clones carry their own .git, which is history, credentials in a config
	// somebody forgot, and megabytes per repository. None of it belongs in an
	// image that gets pulled onto every node.
	s.WriteString(`echo ">> dropping .git from the sources"` + "\n")
	s.WriteString("find . -maxdepth 2 -name .git -type d -prune -exec rm -rf {} +\n")
	s.WriteString("cat > Dockerfile <<'EOF'\n")
	s.WriteString("FROM " + b.Spec.From + "\n")
	s.WriteString("USER root\n")
	s.WriteString(copies.String())
	// A label rather than a convention: the image says where its addons are, so
	// the image study can report it and nobody has to remember.
	s.WriteString("LABEL dev.doblura.addons-path=\"" + strings.Join(paths, ",") + "\"\n")
	s.WriteString("USER odoo\n")
	s.WriteString("EOF\n")
	s.WriteString(`echo ">> building ` + ref + `"` + "\n")
	s.WriteString("buildah --storage-driver=vfs bud --isolation=chroot " +
		"--format=docker -t " + ref + " .\n")
	s.WriteString(`echo ">> pushing"` + "\n")
	s.WriteString("buildah --storage-driver=vfs push " + tls +
		" --digestfile /tmp/digest " + ref + " docker://" + ref + "\n")
	// The digest, and only the digest, on the termination message: it is what the
	// object records and what anything downstream pulls by.
	s.WriteString("cp /tmp/digest " + buildDigestPath + "\n")
	s.WriteString(`echo ">> pushed $(cat /tmp/digest)"` + "\n")
	return s.String()
}

func (r *OdooBuildReconciler) job(b *doblurav1alpha1.OdooBuild, name string) *batchv1.Job {
	vols := []corev1.Volume{
		{Name: "addons-repos", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		// buildah's own storage. vfs is space-hungry and needs no privileges,
		// which is the trade this design accepts on purpose.
		{Name: "containers", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "push", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: b.Spec.To.PushSecret,
				Items: []corev1.KeyToPath{
					{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
				},
			}}},
	}

	inits := make([]corev1.Container, 0, len(b.Spec.Repos))
	for _, repo := range b.Spec.Repos {
		inits = append(inits, cloneContainer(repo))
	}

	build := corev1.Container{
		Name:    "build",
		Image:   b.Spec.BuilderImage,
		Command: []string{"/bin/sh", "-euc"},
		Args:    []string{buildScript(b)},
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/tmp"},
			// buildah reads the push credential from here.
			{Name: "REGISTRY_AUTH_FILE", Value: "/etc/doblura-push/config.json"},
			{Name: "STORAGE_DRIVER", Value: "vfs"},
		},
		SecurityContext: &corev1.SecurityContext{
			// TRUE, and this is the line to argue with.
			//
			// Building an image without root means unpacking layers whose files
			// belong to other UIDs, which needs a RANGE of UIDs mapped into the
			// build's user namespace. Only newuidmap can write that map, newuidmap
			// is setuid, and allowPrivilegeEscalation: false is exactly the flag
			// that stops a setuid binary doing anything. Measured: with it false,
			// "newuidmap: write to uid_map failed: Operation not permitted",
			// followed by a fallback to a single mapping and then "potentially
			// insufficient UIDs or GIDs available in user namespace" on the first
			// real layer.
			//
			// What it does NOT give away: the capability BOUNDING SET is still
			// SETUID and SETGID only, so a setuid-root binary here reaches euid 0
			// with those two capabilities and no others — no CAP_SYS_ADMIN, no
			// mounts, no devices. Combined with non-root, no host network, no host
			// paths and a read-only registry credential, that is the smallest
			// posture in which a real image can be built at all.
			//
			// An earlier version of this had it false and a probe that "proved" it
			// worked. The probe built FROM scratch, which unpacks nothing.
			AllowPrivilegeEscalation: ptr(true),
			// SETUID and SETGID and nothing else.
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"SETUID", "SETGID"},
			},
			// Unconfined seccomp, ON THIS CONTAINER ONLY, and it is the one
			// uncomfortable line in this file.
			//
			// Building an image without root still needs unshare(CLONE_NEWUSER),
			// and the RuntimeDefault profile blocks that syscall: measured, as
			// "Error during unshare(CLONE_NEWUSER): Operation not permitted" on a
			// pod that was otherwise identical.
			//
			// What it means for whoever installs this: a build cannot run in a
			// namespace enforcing the RESTRICTED Pod Security Standard, because
			// restricted forbids Unconfined. It runs under BASELINE. Everything
			// else stays: no privilege escalation, no root, every capability
			// dropped but two, no host mounts, no host network.
			//
			// The alternative is a custom seccomp profile that allows unshare and
			// nothing else, which is better and is a file the cluster
			// administrator has to install on every node — so it is offered as a
			// choice rather than assumed. See the README.
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "addons-repos", MountPath: doblurav1alpha1.AddonRepoMountBase},
			{Name: "tmp", MountPath: "/tmp"},
			{Name: "containers", MountPath: "/home/build/.local/share/containers"},
			{Name: "push", MountPath: "/etc/doblura-push", ReadOnly: true},
		},
		Resources:                sizeToResources(b.Spec.Size),
		TerminationMessagePath:   buildDigestPath,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.Namespace},
		Spec: batchv1.JobSpec{
			// No retries. A build that failed will fail the same way, and a second
			// attempt is a second clone of somebody's source.
			BackoffLimit: ptr(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"doblura.dev/build": b.Name},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true),
						// 1000 is `build` in the buildah image, and the clone
						// containers write to a shared emptyDir as the same user.
						RunAsUser:  ptr(int64(1000)),
						RunAsGroup: ptr(int64(1000)),
						FSGroup:    ptr(int64(1000)),
						// RuntimeDefault for the pod: the clone containers get it,
						// and only the build container opts out — with a comment
						// on it saying why.
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: inits,
					Containers:     []corev1.Container{build},
					Volumes:        vols,
				},
			},
		},
	}
}

func (r *OdooBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooBuild{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Without this the Job finishes and the object goes on saying "building":
		// the predicate above means a status change is not something else.
		Owns(&batchv1.Job{}).
		Named("odoobuild").
		Complete(r)
}
