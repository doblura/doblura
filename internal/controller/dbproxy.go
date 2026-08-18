// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

const (
	proxySecretMount = "/etc/doblura/dbproxy"
	proxyRunDir      = "/run/pgbouncer"
	proxyVolSecret   = "dbproxy-secret"
	proxyVolRun      = "dbproxy-run"
)

// dbProxyScript writes pgbouncer's configuration and then becomes pgbouncer.
//
// Generated in the pod rather than by the operator, and that is the point: the
// manager never reads the database password, so the credential goes from the
// Secret straight into a file in this container's own tmpfs and nowhere else. A
// ConfigMap would have put it in etcd in the clear and in `kubectl get -o yaml`.
//
// The password is read with `cat` into a redirect, never passed as an argument.
// An argument would be visible in /proc/<pid>/cmdline to every process in the
// pod, which includes Odoo — that would have handed back exactly what this whole
// mechanism exists to withhold.
//
// The `*` database entry is a wildcard, not laziness. Odoo does not connect only
// to its own database: the phase scripts run createdb and dropdb against
// `postgres` and `template1`, and bus.py opens `db_connect('postgres')` for the
// notification channel. Enumerating databases here would have worked until the
// first time the bus tried to start.
func dbProxyScript(db *doblurav1alpha1.DatabaseSpec) string {
	p := db.Proxy

	var extra strings.Builder
	if p.MaxClientConn != nil {
		extra.WriteString(fmt.Sprintf("printf 'max_client_conn = %d\\n'\n", *p.MaxClientConn))
	}
	if p.DefaultPoolSize != nil {
		extra.WriteString(fmt.Sprintf("printf 'default_pool_size = %d\\n'\n", *p.DefaultPoolSize))
	}

	return fmt.Sprintf(`set -eu
mkdir -p %[1]s

# auth_type = trust still requires the user to EXIST in auth_file: trust means
# "do not check this user's password", not "accept anyone". An empty auth_file
# gives "trust authentication failed", which reads like a misconfigured password
# and is not one.
printf '"%[2]s" "unused-under-trust"\n' > %[1]s/userlist.txt

{
  printf '[databases]\n'
  # No newline before the password: it is concatenated onto the connection
  # string, and cat provides no trailing newline of its own from a Secret file.
  printf '* = host=%[3]s port=%[4]d user=%[2]s password='
  cat %[5]s/password
  printf '\n\n[pgbouncer]\n'
  printf 'listen_addr = 127.0.0.1\n'
  # Its own tmpfs, not /tmp: the root filesystem is read-only, and pgbouncer
  # treats failing to create the unix socket as fatal even when every client
  # reaches it over TCP.
  printf 'unix_socket_dir = %[1]s\n'
  printf 'listen_port = %[6]d\n'
  printf 'auth_type = trust\n'
  printf 'auth_file = %[1]s/userlist.txt\n'
  printf 'pool_mode = %[7]s\n'
  # Odoo sends these on connect and pgbouncer rejects unknown startup parameters
  # by default, which surfaces as a connection failure with no obvious cause.
  printf 'ignore_startup_parameters = extra_float_digits,options,search_path\n'
  printf 'server_tls_sslmode = prefer\n'
%[8]s} > %[1]s/pgbouncer.ini

chmod 600 %[1]s/pgbouncer.ini
exec pgbouncer %[1]s/pgbouncer.ini
`,
		proxyRunDir, db.User, db.Host, orDefaultInt32(db.Port, 5432),
		proxySecretMount, doblurav1alpha1.ProxyListenPort, p.PoolModeString(), extra.String())
}

// dbProxySidecar is the pooler, as a native sidecar.
//
// A native sidecar (an init container with restartPolicy Always) rather than an
// ordinary container, because the same plumbing has to serve the phase Jobs. An
// ordinary sidecar in a Job never exits, so the Job never completes and the
// restore that finished ten minutes ago still shows as running. Kubernetes stops
// a native sidecar once the main containers are done, which is the behaviour a
// Job needs and is harmless in a Deployment.
func dbProxySidecar(db *doblurav1alpha1.DatabaseSpec) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	return corev1.Container{
		Name:          "dbproxy",
		Image:         db.Proxy.Image,
		RestartPolicy: &always,
		Command:       []string{"/bin/sh", "-c", dbProxyScript(db)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: proxyVolSecret, MountPath: proxySecretMount, ReadOnly: true},
			{Name: proxyVolRun, MountPath: proxyRunDir},
		},
		SecurityContext: envSecurityContext(),
		Resources:       proxyResources(),
	}
}

// dbProxyVolumes are declared on the pod but mounted ONLY into the sidecar.
//
// That asymmetry is the entire security property. A volume in .spec.volumes is
// visible in the pod manifest — the NAME of the Secret is not hidden and does
// not need to be — but a container that does not mount it cannot read it, because
// containers in a pod share a network namespace and not a filesystem.
func dbProxyVolumes(db *doblurav1alpha1.DatabaseSpec) []corev1.Volume {
	return []corev1.Volume{
		{Name: proxyVolSecret, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: db.PasswordSecret},
		}},
		// Small and in memory: the rendered pgbouncer.ini holds the password in
		// the clear, and it should never reach a node's disk.
		{Name: proxyVolRun, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory, SizeLimit: qty("8Mi"),
			},
		}},
	}
}

func proxyResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"cpu": *qty("10m"), "memory": *qty("32Mi")},
		Limits:   corev1.ResourceList{"memory": *qty("128Mi")},
	}
}
