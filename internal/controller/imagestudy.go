// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Studying an image.
//
// A Job that runs the image with a shell and reports what it found. Not `docker
// inspect` and not a registry API call: the questions worth answering are about
// the running filesystem — which user exists, where the addons are, whether
// click-odoo is on the PATH — and none of them is in the manifest.
//
// The report comes back through the termination message, the same mechanism the
// probe and the addon revisions use. Four kilobytes is the limit, which is why
// the module list is a count and a sample rather than an enumeration.
//
// The study never blocks anything. It is evidence for a person deciding whether
// an image is what they think it is, and an image that will not start is exactly
// the finding somebody needs, not a reason to refuse them the entry.

const studyTerminationPath = "/tmp/study"

// studyScript asks the image about itself.
//
// Written to be robust on an image that is missing almost everything: every
// command is guarded, and a missing tool is a finding rather than a failure. It
// ends by printing one line of JSON, because the alternative — parsing prose —
// breaks the first time a version string contains a space.
func studyScript() string {
	return `
have() { command -v "$1" >/dev/null 2>&1; }
esc() { sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\n'; }

VER=""
if have odoo; then
  VER=$(odoo --version 2>/dev/null | head -1 | sed -E 's/^[^0-9]*([0-9]+\.[0-9]+).*/\1/')
fi
if [ -z "$VER" ] && have python3; then
  VER=$(python3 -c 'import odoo,sys; sys.stdout.write(odoo.release.version)' 2>/dev/null | sed -E 's/^([0-9]+\.[0-9]+).*/\1/')
fi

UID_N=$(id -u 2>/dev/null || echo "?")
GID_N=$(id -g 2>/dev/null || echo "?")
UNAME=$(id -un 2>/dev/null || echo "")
USER_S="$UID_N:$GID_N"
[ -n "$UNAME" ] && USER_S="$USER_S ($UNAME)"

# Where the addons live, from the image rather than from a guess.
PATHS=""
for d in /opt/odoo/auto/addons /mnt/extra-addons /usr/lib/python3/dist-packages/odoo/addons; do
  [ -d "$d" ] && PATHS="$PATHS$d,"
done
PATHS=${PATHS%,}

# Modules: count them, and sample the ones that are not Odoo's own. Odoo ships
# its addons inside the package; anything outside it came from this build.
COUNT=0
EXTRA=""
for d in $(echo "$PATHS" | tr ',' ' '); do
  n=$(find "$d" -maxdepth 2 -name __manifest__.py 2>/dev/null | wc -l | tr -d ' ')
  COUNT=$((COUNT + n))
  case "$d" in
    */dist-packages/*) ;;
    *) EXTRA="$EXTRA$(find "$d" -maxdepth 2 -name __manifest__.py 2>/dev/null | head -12 | sed -E 's#.*/([^/]+)/__manifest__.py#\1#' | tr '\n' ',')" ;;
  esac
done
EXTRA=${EXTRA%,}

CLICK=false
have click-odoo && CLICK=true

FLAVOR=Custom
if [ -d /opt/odoo/auto ] && [ -d /opt/odoo/common ]; then
  FLAVOR=Doodba
elif [ -d /usr/lib/python3/dist-packages/odoo ]; then
  FLAVOR=Official
fi

FINDINGS=""
add() { FINDINGS="$FINDINGS$1|"; }
have odoo || add "there is no 'odoo' on the PATH: this image cannot run the server as it is"
[ -z "$VER" ] && add "the image does not report an Odoo version"
[ "$UID_N" = "0" ] && add "it runs as root by default; Doblura will run it as the uid you configure, and that user must be able to write /var/lib/odoo"
[ -z "$UNAME" ] && add "uid $UID_N has no entry in /etc/passwd; Odoo calls getpwuid at startup and exits if the user does not exist"
$CLICK || add "click-odoo is not installed: snapshot restores and moving the filestore into the database both need it"
[ "$COUNT" = "0" ] && add "no addon manifests were found anywhere Doblura looks"
FINDINGS=${FINDINGS%|}

printf '{"odooVersion":"%s","user":"%s","addonsPaths":"%s","modules":%s,"extraModules":"%s","hasClickOdoo":%s,"flavor":"%s","findings":"%s"}\n' \
  "$VER" "$USER_S" "$PATHS" "$COUNT" "$EXTRA" "$CLICK" "$FLAVOR" "$FINDINGS" > ` + studyTerminationPath + `
cat ` + studyTerminationPath + `
`
}

// studyReport is the JSON the script writes. Strings throughout, because a shell
// producing typed JSON is a shell producing broken JSON.
type studyReport struct {
	OdooVersion  string `json:"odooVersion"`
	User         string `json:"user"`
	AddonsPaths  string `json:"addonsPaths"`
	Modules      int32  `json:"modules"`
	ExtraModules string `json:"extraModules"`
	HasClickOdoo bool   `json:"hasClickOdoo"`
	Flavor       string `json:"flavor"`
	Findings     string `json:"findings"`
}

func studyJobName(tenant, entry string) string {
	return "study-" + tenant + "-" + entry
}

// ensureImageStudy runs one study, and reads it back when it finishes.
//
// Runs at most once per catalogue entry per image reference: the Job's name is
// derived from the entry and the result records which image it was about, so
// repointing an entry produces a new study and does not leave a stale one
// looking current.
func (r *OdooTenantReconciler) ensureImageStudy(
	ctx context.Context,
	tenant *doblurav1alpha1.OdooTenant,
	st *doblurav1alpha1.OdooTenantStatus,
	entry doblurav1alpha1.ImageCatalogueEntry,
) error {
	for _, s := range st.ImageStudies {
		if s.Name == entry.Name && s.Image == entry.Image && s.StudiedAt != nil {
			return nil // already answered, about this exact image
		}
	}

	name := studyJobName(tenant.Name, entry.Name)
	key := client.ObjectKey{Namespace: tenant.Namespace, Name: name}

	var job batchv1.Job
	err := r.Get(ctx, key, &job)
	switch {
	case errors.IsNotFound(err):
		return r.createStudyJob(ctx, tenant, entry, name)
	case err != nil:
		return err
	}

	// A Job for a different image than the entry now names: delete it, so the
	// next pass studies what is actually there.
	if job.Annotations["doblura.dev/image"] != entry.Image {
		bg := metav1.DeletePropagationBackground
		return client.IgnoreNotFound(r.Delete(ctx, &job, &client.DeleteOptions{PropagationPolicy: &bg}))
	}

	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		// A Job that cannot pull its image never completes and never fails: it
		// waits, and the entry sat at "not looked at yet" for ever. That is the
		// state a typo in a registry path produces, so it is worth reporting as
		// a finding rather than leaving as silence.
		if reason, detail := r.pullProblem(ctx, tenant.Namespace, name); reason != "" {
			now := metav1.Now()
			upsertStudy(st, doblurav1alpha1.ImageStudy{
				Name: entry.Name, Image: entry.Image, StudiedAt: &now,
				Failed: fmt.Sprintf("the image could not be pulled (%s): %s", reason, detail),
			})
		}
		return nil
	}

	study := doblurav1alpha1.ImageStudy{Name: entry.Name, Image: entry.Image}
	now := metav1.Now()
	study.StudiedAt = &now

	msg := r.studyMessage(ctx, tenant.Namespace, name)
	if msg == "" {
		study.Failed = "the image produced no report; it may not have started at all"
	} else if err := applyStudy(&study, msg); err != nil {
		study.Failed = err.Error()
	}

	upsertStudy(st, study)
	return nil
}

func (r *OdooTenantReconciler) createStudyJob(
	ctx context.Context,
	tenant *doblurav1alpha1.OdooTenant,
	entry doblurav1alpha1.ImageCatalogueEntry,
	name string,
) error {
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: tenant.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "doblura",
				"doblura.dev/tenant":           tenant.Name,
				"doblura.dev/image-study":      entry.Name,
			},
			// The image the report is about, so a repointed entry is studied
			// again rather than showing a report about the previous build.
			Annotations: map[string]string{"doblura.dev/image": entry.Image},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr(int32(1)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// No security context beyond the defaults, and that is
					// deliberate: the study has to run the image AS IT IS to
					// report what it is, including the user it chose. Forcing a
					// uid here would make the most useful finding impossible.
					Containers: []corev1.Container{{
						Name:    "study",
						Image:   entry.Image,
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{studyScript()},
						// Read-only would stop the script writing its own report.
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						TerminationMessagePath: studyTerminationPath,
						// FallbackToLogsOnError, so an image with no shell at all
						// still tells us something rather than nothing.
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{"cpu": *qty("50m"), "memory": *qty("128Mi")},
							Limits:   corev1.ResourceList{"memory": *qty("512Mi")},
						},
					}},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(tenant, job, r.Scheme); err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Create(ctx, job))
}

// pullProblem reports a pod that is stuck waiting for an image.
//
// Only the pull failures, and not every waiting reason: a pod that is briefly
// ContainerCreating is not a problem, and reporting it as one would make the
// study flap between "no report" and "failed" on every pass.
func (r *OdooTenantReconciler) pullProblem(ctx context.Context, ns, jobName string) (reason, detail string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{"job-name": jobName}); err != nil {
		return "", ""
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			w := cs.State.Waiting
			if w == nil {
				continue
			}
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return w.Reason, firstLine(w.Message)
			}
		}
	}
	return "", ""
}

// studyMessage reads the report back off the pod.
func (r *OdooTenantReconciler) studyMessage(ctx context.Context, ns, jobName string) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{"job-name": jobName}); err != nil {
		return ""
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return cs.State.Terminated.Message
			}
		}
	}
	return ""
}

// applyStudy parses the report.
func applyStudy(into *doblurav1alpha1.ImageStudy, msg string) error {
	// The message may hold log lines when the fallback kicked in, so take the
	// last line that looks like an object.
	line := ""
	for _, l := range strings.Split(strings.TrimSpace(msg), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "{") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		return fmt.Errorf("the image did not produce a report: %s", firstLine(msg))
	}

	var rep studyReport
	if err := json.Unmarshal([]byte(line), &rep); err != nil {
		return fmt.Errorf("the report could not be read: %w", err)
	}

	into.OdooVersion = rep.OdooVersion
	into.User = rep.User
	into.Modules = rep.Modules
	into.HasClickOdoo = rep.HasClickOdoo
	into.Flavor = doblurav1alpha1.ImageFlavor(rep.Flavor)
	into.AddonsPaths = splitNonEmpty(rep.AddonsPaths, ",")
	into.ExtraModules = splitNonEmpty(rep.ExtraModules, ",")
	into.Findings = splitNonEmpty(rep.Findings, "|")
	return nil
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func upsertStudy(st *doblurav1alpha1.OdooTenantStatus, study doblurav1alpha1.ImageStudy) {
	for i := range st.ImageStudies {
		if st.ImageStudies[i].Name == study.Name {
			st.ImageStudies[i] = study
			return
		}
	}
	st.ImageStudies = append(st.ImageStudies, study)
}
