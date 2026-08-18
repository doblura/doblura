// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func intstrFromString(s string) intstr.IntOrString { return intstr.FromString(s) }

// envSelector is identity only, never the version.
//
// A Deployment's spec.selector is immutable. Put anything that changes between
// releases in here and the first upgrade fails with "field is immutable" and the
// Deployment has to be deleted by hand.
func envSelector(env *doblurav1alpha1.OdooEnvironment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "odoo",
		"app.kubernetes.io/instance": env.Name,
	}
}

// envTierSelector is the identity of a NON-web tier.
//
// It deliberately does NOT extend envSelector with an extra label, for two
// reasons that pull the same way. A Deployment's selector is immutable, so
// adding a label to the web tier's selector would break every existing install
// with "field is immutable". And the Service selects on envSelector: any pod
// carrying those labels receives HTTP traffic, so a cron pod that merely ADDED a
// label to them would end up behind the Ingress, serving requests it was
// created to stop serving.
//
// So the cron tier is a different app.kubernetes.io/name. The two selectors are
// disjoint, neither Deployment can steal the other's pods, and the Service
// cannot reach the cron tier by construction rather than by care.
func envTierSelector(env *doblurav1alpha1.OdooEnvironment, tier string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "odoo-" + tier,
		"app.kubernetes.io/instance": env.Name,
	}
}

// envTierLabels is envLabels for a non-web tier.
func envTierLabels(env *doblurav1alpha1.OdooEnvironment, tier string) map[string]string {
	l := envTierSelector(env, tier)
	l["app.kubernetes.io/component"] = tier
	l["app.kubernetes.io/managed-by"] = "doblura"
	// The label the NetworkPolicy and the operator's own listings select on: it
	// is the one thing every pod of the environment shares, whatever its tier.
	l["doblura.dev/environment"] = env.Name
	l["doblura.dev/tier"] = tier
	if env.Spec.ForTenant != "" {
		l["doblura.dev/tenant"] = env.Spec.ForTenant
	}
	return l
}

func envLabels(env *doblurav1alpha1.OdooEnvironment, component string) map[string]string {
	l := envSelector(env)
	l["doblura.dev/tier"] = "web"
	l["app.kubernetes.io/component"] = component
	l["app.kubernetes.io/managed-by"] = "doblura"
	l["doblura.dev/environment"] = env.Name
	if env.Spec.ForTenant != "" {
		l["doblura.dev/tenant"] = env.Spec.ForTenant
	}
	return l
}

// envDBName is the database this environment runs on.
func envDBName(env *doblurav1alpha1.OdooEnvironment) string {
	return "env_" + strings.ReplaceAll(env.Name, "-", "_")
}

// envOdooConf composes the configuration every phase and the serving pod share.
func envOdooConf(env *doblurav1alpha1.OdooEnvironment) string {
	paths := env.Spec.Addons.AddonsPathFor(doblurav1alpha1.AddonRepoMountBase)

	var b strings.Builder
	b.WriteString("[options]\n")
	if len(paths) > 0 {
		b.WriteString("addons_path = " + strings.Join(paths, ",") + "\n")
	}
	b.WriteString("data_dir = " + doblurav1alpha1.DataDirPath + "\n")
	b.WriteString(fmt.Sprintf("db_host = %s\n", env.Spec.Database.Host))
	b.WriteString(fmt.Sprintf("db_port = %d\n", orDefaultInt32(env.Spec.Database.Port, 5432)))
	b.WriteString(fmt.Sprintf("db_user = %s\n", env.Spec.Database.User))
	b.WriteString(fmt.Sprintf("db_name = %s\n", envDBName(env)))
	// list_db off and no database manager: this environment may be public, and
	// the manager is a way to create and drop databases from a web form.
	b.WriteString("list_db = False\n")
	b.WriteString("proxy_mode = True\n")
	// workers and max_cron_threads used to be hardcoded at 2 and 1, which meant
	// every replica of every environment ran a cron thread. Now they follow the
	// workload split, and the value that matters is the ZERO: with a cron tier
	// present the web tier must not run crons, or they run in both places.
	w := env.Spec.Workload
	workers := int32(2)
	if w != nil && w.Web != nil && w.Web.Workers != nil {
		workers = *w.Web.Workers
	}
	b.WriteString(fmt.Sprintf("workers = %d\n", workers))
	b.WriteString(fmt.Sprintf("max_cron_threads = %d\n", w.CronThreadsForWeb()))
	return b.String()
}

// envCronConf is the configuration of the cron tier.
//
// The inverse of the web tier: no HTTP workers, all the cron threads. workers = 0
// puts Odoo in threaded mode, where max_cron_threads spawns cron threads directly
// in the one process — which is what a cron-only pod wants, and what makes the
// pod's memory footprint predictable.
//
// HTTP stays ENABLED even though nothing routes to this tier. Turning it off with
// http_enable = False would be tidier, but it would also remove the only probe
// that proves anything: /web/health opens a cursor against the database, so a
// cron pod that has lost its connection fails its liveness probe and is
// restarted. Without it the probe would be "is the process running", which stays
// true for a cron worker that has been doing nothing for six hours. The tier is
// unreachable because no Service selects it, not because it stopped listening.
func envCronConf(env *doblurav1alpha1.OdooEnvironment) string {
	base := envOdooConf(env)
	threads := env.Spec.Workload.CronThreads()

	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(base, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "workers "):
			b.WriteString("workers = 0\n")
		case strings.HasPrefix(line, "max_cron_threads "):
			b.WriteString(fmt.Sprintf("max_cron_threads = %d\n", threads))
		default:
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// envVolumes and envMounts are shared by every pod of the environment, so the
// phase Jobs and the serving Deployment see the same paths.
func envVolumes(env *doblurav1alpha1.OdooEnvironment) ([]corev1.Volume, []corev1.VolumeMount) {
	vols := []corev1.Volume{
		{Name: "tmp", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: qty("1Gi")}}},
		// The filestore. An emptyDir only when the environment is genuinely
		// throwaway — the API refuses the combination that would otherwise lose
		// attachments, and this is the half that honours the choice.
		filestoreVolume(env),
		{Name: "stage", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "odoo-conf", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: env.Name + "-odoo-conf"}}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "data", MountPath: doblurav1alpha1.DataDirPath},
		{Name: "stage", MountPath: doblurav1alpha1.StagePath},
		{Name: "odoo-conf", MountPath: "/etc/doblura", ReadOnly: true},
	}

	addonVols, addonMounts, _ := addonsPlumbing(&env.Spec.Addons)
	vols = append(vols, addonVols...)
	mounts = append(mounts, addonMounts...)
	return vols, mounts
}

func envEnv(env *doblurav1alpha1.OdooEnvironment) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: env.Spec.Database.Host},
		{Name: "PGPORT", Value: fmt.Sprint(orDefaultInt32(env.Spec.Database.Port, 5432))},
		{Name: "PGUSER", Value: env.Spec.Database.User},
		{Name: "PGDATABASE", Value: envDBName(env)},
		{Name: "PGPASSWORD", ValueFrom: secretRef(env.Spec.Database.PasswordSecret, "password")},
		// HOME so anything that writes a dotfile has somewhere to put it. The
		// pod runs as 65532, which usually has no home directory in the image.
		{Name: "HOME", Value: "/tmp"},
	}
}

func envSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func envPodSecurityContext(env *doblurav1alpha1.OdooEnvironment) *corev1.PodSecurityContext {
	// Odoo's uid, not Kubernetes'. See OdooEnvironmentSpec.PodUser.
	uid, gid, fsg := env.Spec.PodUser()
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr(true),
		RunAsUser:      ptr(uid),
		RunAsGroup:     ptr(gid),
		FSGroup:        ptr(fsg),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// envJobPod builds the pod for one phase step.
func envJobPod(env *doblurav1alpha1.OdooEnvironment, step envPhaseStep) corev1.PodTemplateSpec {
	vols, mounts := envVolumes(env)
	_, _, inits := addonsPlumbing(&env.Spec.Addons)

	// The harden step reads the generated admin passwords from their Secret.
	// Mounted as a volume rather than injected as env vars: the logins are
	// user-supplied, so they cannot be turned into valid env var names reliably.
	if step.name == "harden" {
		vols = append(vols, corev1.Volume{
			Name: "credentials",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: env.Name + "-credentials",
					Optional:   ptr(true),
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: "credentials", MountPath: "/etc/doblura-credentials", ReadOnly: true,
		})
	}

	// The restore step also needs the snapshot mounted.
	if step.name == "restore" && env.Spec.Data.Snapshot != nil {
		snapVols, snapMounts, snapInits := snapshotPlumbing(env.Spec.Data.Snapshot)
		vols = append(vols, snapVols...)
		vols = append(vols, customExtraVolumes(&env.Spec.Data.Snapshot.From)...)
		mounts = append(mounts, snapMounts...)
		inits = append(inits, snapInits...)
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: envLabels(env, step.name)},
		Spec: corev1.PodSpec{
			RestartPolicy:   corev1.RestartPolicyNever,
			SecurityContext: envPodSecurityContext(env),
			InitContainers:  inits,
			Containers: []corev1.Container{{
				Name:            step.name,
				Image:           env.Spec.Image,
				Command:         []string{"/bin/sh", "-euc"},
				Args:            []string{step.script(env)},
				Env:             envEnv(env),
				VolumeMounts:    mounts,
				SecurityContext: envSecurityContext(),
				Resources:       sizeToResources(env.Spec.Size),
			}},
			Volumes: vols,
		},
	}
}

// envServingPod builds the long-running Odoo.
func envServingPod(env *doblurav1alpha1.OdooEnvironment) corev1.PodTemplateSpec {
	vols, mounts := envVolumes(env)
	_, _, inits := addonsPlumbing(&env.Spec.Addons)

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: envLabels(env, "odoo")},
		Spec: corev1.PodSpec{
			SecurityContext: envPodSecurityContext(env),
			InitContainers:  inits,
			Containers: []corev1.Container{{
				Name:  "odoo",
				Image: env.Spec.Image,
				Args:  []string{"-c", "/etc/doblura/odoo.conf"},
				Env:   envEnv(env),
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8069},
					{Name: "websocket", ContainerPort: 8072},
				},
				// Probes by port NAME, so they survive a configuration change.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/web/health", Port: intstrFromString("http")}},
					InitialDelaySeconds: 20, PeriodSeconds: 10,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/web/health", Port: intstrFromString("http")}},
					InitialDelaySeconds: 60, PeriodSeconds: 30,
				},
				VolumeMounts:    mounts,
				SecurityContext: envSecurityContext(),
				Resources:       sizeToResources(env.Spec.Size),
			}},
			Volumes: vols,
		},
	}
}

// envCronPod builds the tier that runs the scheduled jobs.
//
// The same image, the same volumes, the same database — and a different
// configuration file. What makes it a cron worker is one line of odoo.conf, not a
// different program, which is why this shares everything with the serving pod
// rather than reimplementing it.
//
// It mounts the filestore like the web tier does, and that is the constraint that
// makes this tier interesting rather than trivial: report generation and any job
// that touches an attachment WRITES to the filestore. Two pods writing one
// filestore means it has to be genuinely shared, which is why the API refuses a
// cron tier over a ReadWriteOnce volume.
func envCronPod(env *doblurav1alpha1.OdooEnvironment) corev1.PodTemplateSpec {
	vols, mounts := envVolumes(env)
	_, _, inits := addonsPlumbing(&env.Spec.Addons)

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: envTierLabels(env, "cron")},
		Spec: corev1.PodSpec{
			SecurityContext: envPodSecurityContext(env),
			InitContainers:  inits,
			Containers: []corev1.Container{{
				Name:  "odoo-cron",
				Image: env.Spec.Image,
				Args:  []string{"-c", envCronConfPath},
				Env:   envEnv(env),
				// Named, but no Service selects them. The name is what the
				// probes below refer to.
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8069}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/web/health", Port: intstrFromString("http")}},
					InitialDelaySeconds: 20, PeriodSeconds: 10,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/web/health", Port: intstrFromString("http")}},
					// Longer than the web tier's. A cron worker in the middle of
					// a long job is busy, not dead, and restarting it there is
					// how a nightly job never finishes.
					InitialDelaySeconds: 60, PeriodSeconds: 60, FailureThreshold: 5,
				},
				VolumeMounts:    mounts,
				SecurityContext: envSecurityContext(),
				Resources:       sizeToResources(env.Spec.Size),
			}},
			Volumes: vols,
		},
	}
}

// ─────────────────────── phase scripts ───────────────────────

const (
	envConf         = "/etc/doblura/odoo.conf"
	envCronConfPath = "/etc/doblura/odoo-cron.conf"
)

// envInitScript creates the database from nothing, with or without demo data.
func envInitScript(env *doblurav1alpha1.OdooEnvironment) string {
	demo := "--without-demo=all"
	if env.Spec.Data.Type == doblurav1alpha1.DataDemo {
		demo = "--without-demo=False"
	}
	return fmt.Sprintf(`echo ">> creating %[1]s (%[2]s)"
dropdb --if-exists "%[1]s"
createdb "%[1]s"
odoo -c %[3]s -d "%[1]s" -i base %[4]s --stop-after-init --log-level=warn
echo ">> database ready"`, envDBName(env), env.Spec.Data.Type, envConf, demo)
}

// envRestoreScript restores the anonymized snapshot.
func envRestoreScript(env *doblurav1alpha1.OdooEnvironment) string {
	db := envDBName(env)
	snap := env.Spec.Data.Snapshot
	if snap == nil {
		return `echo ">> no snapshot declared" >&2; exit 1`
	}

	// Same staging as the rehearsal, for the same reason: click-odoo-restoredb
	// MOVES the filestore out of the source folder, and the snapshot is mounted
	// ReadOnly so one environment cannot damage the dump the others depend on.
	source := doblurav1alpha1.SnapshotMountPath
	stage := ""
	if snap.Format == "" || snap.Format == doblurav1alpha1.FormatOdooBackup {
		source = doblurav1alpha1.StagePath + "/dump"
		stage = fmt.Sprintf(`mkdir -p %s
cp -a %s %s
`, doblurav1alpha1.StagePath, doblurav1alpha1.SnapshotMountPath, source)
	}

	return fmt.Sprintf(`echo ">> restoring into %s"
echo ">> pg client: $(pg_restore --version 2>/dev/null || echo unknown)"
echo ">> pg server: $(psql -d postgres -tAc 'show server_version' 2>/dev/null || echo unreachable)"
dropdb --if-exists "%s"
%s%s
echo ">> restore finished"`, db, db, stage, snap.RestoreCommand(db, envConf, source))
}

// envMigrateScript runs the update. A step, not a gate.
func envMigrateScript(env *doblurav1alpha1.OdooEnvironment) string {
	// Preflight, because the alternative message is "/bin/sh: click-odoo-update:
	// not found", which tells somebody nothing about the requirement they missed.
	// The README states it — the image must ship click-odoo-contrib — and a tool
	// that states a requirement should also check it.
	pre := `if ! command -v click-odoo-update >/dev/null 2>&1; then
  echo "!! this image does not ship click-odoo-contrib, and migrating needs it." >&2
  echo "!! Doblura does not provide a base image: the requirement is that yours" >&2
  echo "!! has click-odoo-contrib installed (pip install click-odoo-contrib)." >&2
  echo "!! Image: ` + env.Spec.Image + `" >&2
  exit 1
fi
`
	db := envDBName(env)
	var cmd string
	switch env.Spec.Migration.Engine {
	case doblurav1alpha1.EngineOdooUpdateAll:
		cmd = fmt.Sprintf(`odoo -c %s -d "%s" -u all --stop-after-init`, envConf, db)
	case doblurav1alpha1.EngineMarabunta:
		cmd = fmt.Sprintf(`MARABUNTA_DATABASE="%s" MARABUNTA_MODE=full marabunta`, db)
	default:
		cmd = fmt.Sprintf(`click-odoo-update -c %s -d "%s"`, envConf, db)
	}
	if extra := env.Spec.Migration.ExtraArgs; len(extra) > 0 {
		cmd += " " + strings.Join(extra, " ")
	}
	guard := ""
	if strings.HasPrefix(cmd, "click-odoo-update") {
		guard = pre
	}
	return fmt.Sprintf(`echo ">> migrating %s to the declared version"
%s%s
echo ">> migration finished"`, db, guard, cmd)
}

// envHardenScript is the phase the whole design turns on.
//
// It runs BEFORE the Ingress exists. The dump OdooSnapshot produces carries the
// same known password for every user, which is right for a rehearsal and hands
// over the administrator account on a public URL. Serving the freshly restored
// database for even a second means serving that password to the internet.
func envHardenScript(env *doblurav1alpha1.OdooEnvironment) string {
	db := envDBName(env)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("echo \">> hardening %s\"\n", db))

	sec := env.Spec.Security
	if sec.RandomizeUserPasswords == nil || *sec.RandomizeUserPasswords {
		admins := sec.AdminUsers
		if len(admins) == 0 {
			admins = []string{"admin"}
		}
		// Everyone gets an unusable password; the declared admins get the one
		// the operator generated and stored in a Secret. Randomising everybody
		// would leave an environment nobody can validate, which is the same
		// problem by another route.
		b.WriteString(`echo ">> randomising passwords; admins keep the generated one"` + "\n")
		b.WriteString("psql -v ON_ERROR_STOP=1 <<SQL\nBEGIN;\n")
		b.WriteString("UPDATE res_users SET password = md5(random()::text || id::text);\n")
		b.WriteString("COMMIT;\nSQL\n")
		b.WriteString("for u in " + strings.Join(admins, " ") + "; do\n")
		b.WriteString("  pw=$(cat \"/etc/doblura-credentials/$u\")\n")
		b.WriteString("  psql -v ON_ERROR_STOP=1 -c \"UPDATE res_users SET password = '$pw' WHERE login = '$u'\"\n")
		b.WriteString("done\n")
	}

	if env.Spec.FilestoreInDatabase() {
		// Odoo core, not an addon: ir_attachment._storage() reads this parameter and
		// returns 'file' when it is absent. Setting it to 'db' sends new attachment
		// bytes to ir_attachment.db_datas instead of to disk.
		//
		// Set here rather than in odoo.conf because it is a DATABASE parameter, not
		// a server option — it travels with the dump, which is the point: a copy of
		// this database carries its own storage decision.
		b.WriteString(`echo ">> attachments to the database (ir_attachment.location = db)"` + "\n")
		b.WriteString("psql -v ON_ERROR_STOP=1 <<'SQL'\nBEGIN;\n")
		b.WriteString("INSERT INTO ir_config_parameter (key, value, create_uid, write_uid, create_date, write_date)\n")
		b.WriteString("VALUES ('ir_attachment.location', 'db', 1, 1, now(), now())\n")
		b.WriteString("ON CONFLICT (key) DO UPDATE SET value = 'db', write_date = now();\n")
		b.WriteString("COMMIT;\nSQL\n")

		// Existing attachments stay on disk until something moves them, and on an
		// ephemeral filestore that disk is about to disappear. force_storage() is
		// Odoo's own migration and it is a per-record ORM write, so it is slow on a
		// large database.
		//
		// Guarded on the actual condition rather than on a proxy for it: a freshly
		// initialised database has no attachments on disk, so there is nothing to
		// move and no reason to require click-odoo-contrib. Testing the data type
		// instead would have been a guess that is wrong for a restored snapshot
		// whose attachments were already in the database.
		b.WriteString(`n=$(psql -tAX -c "SELECT count(*) FROM ir_attachment WHERE store_fname IS NOT NULL" 2>/dev/null | tr -dc '0-9')` + "\n")
		b.WriteString(`if [ "${n:-0}" -gt 0 ]; then` + "\n")
		b.WriteString(`  if ! command -v click-odoo >/dev/null 2>&1; then` + "\n")
		b.WriteString(`    echo "!! $n attachments are on disk and this image has no click-odoo-contrib," >&2` + "\n")
		b.WriteString(`    echo "!! so they cannot be moved into the database. They will 404 when the" >&2` + "\n")
		b.WriteString(`    echo "!! filestore goes. Install click-odoo-contrib, or use a PVC filestore." >&2` + "\n")
		b.WriteString(`    exit 1` + "\n")
		b.WriteString(`  fi` + "\n")
		b.WriteString(`  echo ">> moving $n attachments into the database (per-record, and slow)"` + "\n")
		b.WriteString("  cat > /tmp/doblura-force-storage.py <<'PYSCRIPT'\n")
		b.WriteString("env['ir.attachment'].sudo().force_storage()\n")
		b.WriteString("env.cr.commit()\n")
		b.WriteString("PYSCRIPT\n")
		b.WriteString("  click-odoo -c " + envConf + " /tmp/doblura-force-storage.py\n")
		b.WriteString(`else` + "\n")
		b.WriteString(`  echo ">> no attachments on disk; nothing to move"` + "\n")
		b.WriteString(`fi` + "\n")
	}

	if sec.StripExternalCredentials == nil || *sec.StripExternalCredentials {
		// The gap neutralization leaves open. `odoo neutralize` cuts mail, crons,
		// payment providers and carriers; it does not touch the API tokens and
		// webhook URLs your own modules keep in ir_config_parameter. That is how
		// a test environment ends up writing into a supplier's ERP.
		b.WriteString(`echo ">> stripping external credentials from ir_config_parameter"` + "\n")
		b.WriteString("psql -v ON_ERROR_STOP=1 <<'SQL'\nBEGIN;\n")
		b.WriteString("DELETE FROM ir_config_parameter WHERE key ~* " +
			"'(token|secret|api[_.]?key|password|webhook|client[_.]?id|private[_.]?key)';\n")
		b.WriteString("COMMIT;\nSQL\n")
	}

	// Belt and braces on top of whatever the snapshot already neutralized: this
	// environment may be reachable from the internet.
	b.WriteString(`echo ">> re-asserting neutralization"` + "\n")
	b.WriteString(fmt.Sprintf("odoo -c %s neutralize -d \"%s\" || true\n", envConf, db))
	b.WriteString("psql -v ON_ERROR_STOP=1 <<'SQL'\nBEGIN;\n")
	b.WriteString("UPDATE ir_mail_server SET active = false;\n")
	b.WriteString("UPDATE ir_cron SET active = false;\n")
	b.WriteString("COMMIT;\nSQL\n")
	b.WriteString(`echo ">> hardened"`)
	return b.String()
}

// envDropScript removes the database when the environment is destroyed.
func envDropScript(env *doblurav1alpha1.OdooEnvironment) string {
	return fmt.Sprintf(`echo ">> dropping %[1]s"
dropdb --if-exists "%[1]s"
echo ">> dropped"`, envDBName(env))
}

// filestoreVolume returns the volume Odoo's filestore lives on.
//
// The unbounded emptyDir this used to be unconditionally is still the default, and
// it is correct for an environment that exists for eight hours. What changed is
// that it is now a choice rather than the only option, and the sizeLimit is set:
// an unbounded emptyDir is charged to the node's ephemeral storage and evicts the
// pod when the node fills, which is a failure that arrives as "my environment
// disappeared" with nothing in the environment's own events.
func filestoreVolume(env *doblurav1alpha1.OdooEnvironment) corev1.Volume {
	v := corev1.Volume{Name: "data"}
	fs := (*doblurav1alpha1.FilestoreSpec)(nil)
	if env.Spec.Storage != nil {
		fs = env.Spec.Storage.Filestore
	}
	if fs != nil && fs.Mode == doblurav1alpha1.FilestorePVC {
		name := fs.ClaimName
		if name == "" {
			// A claim this environment owns, named after it so the association is
			// visible in `kubectl get pvc` without cross-referencing anything.
			name = env.Name + "-filestore"
		}
		v.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name},
		}
		return v
	}
	v.VolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: qty("8Gi")},
	}
	return v
}

// FilestoreClaim is the PVC to create when the environment owns its filestore.
//
// Returns nil when the user named an existing claim: that volume's lifecycle is
// somebody else's, and creating or deleting it here would be the operator taking
// ownership of storage it was only asked to mount.
func FilestoreClaim(env *doblurav1alpha1.OdooEnvironment) *corev1.PersistentVolumeClaim {
	if env.Spec.Storage == nil || env.Spec.Storage.Filestore == nil {
		return nil
	}
	fs := env.Spec.Storage.Filestore
	if fs.Mode != doblurav1alpha1.FilestorePVC || fs.ClaimName != "" || fs.Size == "" {
		return nil
	}
	mode := corev1.ReadWriteOnce
	if fs.AccessModeReadWriteMany {
		mode = corev1.ReadWriteMany
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name + "-filestore", Namespace: env.Namespace,
			Labels: envLabels(env, "filestore"),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{mode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *qty(fs.Size)},
			},
		},
	}
	if fs.StorageClass != "" {
		pvc.Spec.StorageClassName = &fs.StorageClass
	}
	return pvc
}
