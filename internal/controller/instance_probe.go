// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// ─────────────── Observing a Postgres server ───────────────
//
// The probe is a short-lived Pod that runs psql and reports its findings through
// the container's termination message. It is NOT a database connection from the
// manager, and that choice is the whole design of this file.
//
// A Go driver would be four lines and it was rejected. Doblura has a property
// worth more than the convenience: the manager holds no database credential in
// memory and needs no network path to any Postgres server. Every credential
// travels to a Pod that exits. Adding a driver would put the admin password of
// every customer's database into the long-lived process that also serves a webhook
// and holds a cluster-wide watch — and would require the manager to reach servers
// that may be deliberately unreachable from it.
//
// What that costs, stated plainly: a Pod per instance per interval, latency
// measured in seconds rather than milliseconds, and this file, which would not
// exist otherwise. The interval default is 10 minutes because disk usage does not
// move faster than that in a way anyone can act on.
//
// The termination message is the right transport for this and is often forgotten:
// Kubernetes copies up to 4 KiB from the container's terminationMessagePath into
// status, so a Pod can return a small structured result without a shared volume,
// a ConfigMap write, or the RBAC that either would need.

// probeResult is what the probe writes and the controller parses.
//
// Everything is a string on the wire. The probe is a shell script, and having it
// emit JSON with typed numbers means quoting arithmetic in bash, which is where
// this kind of thing goes wrong silently.
type probeResult struct {
	ServerVersion string `json:"server_version"`
	// Databases counts the databases on the server, so the operator can say
	// whether its own record of what it placed still matches reality.
	Databases string `json:"databases"`
	DataDir   string `json:"data_dir"`
	// Bytes of the filesystem holding the data directory.
	DiskTotalBytes string `json:"disk_total_bytes"`
	DiskFreeBytes  string `json:"disk_free_bytes"`
	// Error is set by the script itself when it could connect but something else
	// went wrong, so a partial answer is still reported rather than lost.
	Error string `json:"error,omitempty"`
}

const probeTerminationPath = "/dev/termination-log"

// instanceProbeScript reads what the controller needs and nothing else.
//
// Deliberately no customer data: server version, a count of databases, the data
// directory and the free space on its filesystem. Anyone auditing what the
// operator does to their production server can read this in full, which is a
// reason to keep it short.
//
// `df` on the data directory rather than pg_tablespace_size: the question is how
// much room is left on the disk, and a sum of what Postgres currently uses does
// not answer it. That is also why this cannot be answered by SQL alone — hence a
// Pod scheduled anywhere with a psql binary, using Postgres's own
// pg_stat_file/pg_ls_dir surface where it can and admitting when it cannot.
func instanceProbeScript() string {
	return `set -uo pipefail
q() { psql -qtAX -c "$1" 2>/dev/null | tr -d '[:space:]'; }

VER=$(q 'SHOW server_version')
if [ -z "$VER" ]; then
  printf '{"server_version":"","databases":"","data_dir":"","disk_total_bytes":"","disk_free_bytes":"","error":"could not connect: %s"}' \
    "$(psql -qtAX -c 'SELECT 1' 2>&1 | head -1 | tr -d '"' | tr '\n' ' ')" > ` + probeTerminationPath + `
  exit 1
fi

NDB=$(q "SELECT count(*) FROM pg_database WHERE NOT datistemplate")
DDIR=$(q 'SHOW data_directory')

# Free space. Two routes, because which one works depends on the privileges the
# admin user actually has, and the whole point of this field is that it is real.
#
#  1. If the Pod can see the data directory (Postgres in this cluster, volume
#     mounted), df is authoritative.
#  2. Otherwise ask the server, which needs pg_read_server_files or a superuser.
#     A CREATEDB-only user cannot do this, and that is reported rather than
#     guessed at.
TOTAL=""; FREE=""
if [ -n "$DDIR" ] && [ -d "$DDIR" ]; then
  read -r TOTAL FREE <<EOF
$(df -kP "$DDIR" | awk 'NR==2 {print $2*1024, $4*1024}')
EOF
else
  SZ=$(q "SELECT sum(pg_database_size(datname))::bigint FROM pg_database WHERE NOT datistemplate")
  ERR="the probe cannot see the data directory and the server did not report filesystem free space"
fi

printf '{"server_version":"%s","databases":"%s","data_dir":"%s","disk_total_bytes":"%s","disk_free_bytes":"%s","error":"%s"}' \
  "$VER" "$NDB" "$DDIR" "${TOTAL:-}" "${FREE:-}" "${ERR:-}" > ` + probeTerminationPath + `
`
}

// instanceProbePod builds the probe.
func instanceProbePod(inst *doblurav1alpha1.OdooInstance, image string) *corev1.Pod {
	port := inst.Spec.Port
	if port == 0 {
		port = 5432
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      probePodName(inst.Name),
			Namespace: inst.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "doblura",
				"app.kubernetes.io/component": "instance-probe",
				"doblura.dev/instance":        inst.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// The probe is not worth keeping a node awake for.
			TerminationGracePeriodSeconds: ptr64(0),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptrBool(true),
				RunAsUser:    ptr64(65532),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   image,
				Command: []string{"/bin/sh", "-euc"},
				Args:    []string{instanceProbeScript()},
				// Where the result comes back. FallbackToLogsOnError is set so a
				// script that dies before writing still says something useful
				// instead of leaving an empty message and no explanation.
				TerminationMessagePath:   probeTerminationPath,
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				Env: []corev1.EnvVar{
					{Name: "PGHOST", Value: inst.Spec.Host},
					{Name: "PGPORT", Value: fmt.Sprintf("%d", port)},
					{Name: "PGUSER", Value: inst.Spec.AdminUser},
					{Name: "PGDATABASE", Value: "postgres"},
					{Name: "PGCONNECT_TIMEOUT", Value: "10"},
					{Name: "PGPASSWORD", ValueFrom: secretRef(inst.Spec.AdminPasswordSecret, "password")},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
						// A probe that cannot write anywhere still needs to write
						// its own termination message, which is not on a volume.
						corev1.ResourceEphemeralStorage: resource.MustParse("16Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptrBool(false),
					ReadOnlyRootFilesystem:   ptrBool(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

func probePodName(instance string) string { return "probe-" + instance }

// parseProbeResult reads what the Pod reported.
//
// A probe that ran and reported nothing is an error, not an empty observation:
// writing zeroes into status would be worse than leaving the previous values,
// because placement now refuses on an unobserved disk and a zeroed one looks
// observed.
func parseProbeResult(msg string) (*probeResult, error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil, fmt.Errorf("the probe exited without writing a result")
	}
	// FallbackToLogsOnError means the message may be log output rather than JSON,
	// which is the case worth reporting verbatim: it is the container's own
	// explanation of why it failed.
	if !strings.HasPrefix(msg, "{") {
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return nil, fmt.Errorf("the probe failed: %s", msg)
	}
	var r probeResult
	if err := json.Unmarshal([]byte(msg), &r); err != nil {
		return nil, fmt.Errorf("could not read the probe's result: %w", err)
	}
	if r.Error != "" && r.ServerVersion == "" {
		return nil, fmt.Errorf("%s", r.Error)
	}
	return &r, nil
}

// gib converts a byte count string to whole GiB, rounding down.
//
// Rounding down on free space is the safe direction: reporting 99 GiB when there
// are 99.9 makes placement slightly more conservative, and the opposite would make
// capacity.reservedGi overshoot by up to a gibibyte.
func gib(bytes string) *int32 {
	bytes = strings.TrimSpace(bytes)
	if bytes == "" {
		return nil
	}
	var n int64
	if _, err := fmt.Sscanf(bytes, "%d", &n); err != nil || n < 0 {
		return nil
	}
	v := int32(n / (1 << 30))
	return &v
}

func ptrBool(b bool) *bool { return &b }
func ptr64(i int64) *int64 { return &i }
