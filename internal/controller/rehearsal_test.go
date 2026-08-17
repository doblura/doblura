// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The invariant that matters most in the whole project: unless somebody says
// otherwise, we neutralize. A failure here sends real emails.
func TestRestoreNeutralizesByDefault(t *testing.T) {
	cases := []struct {
		name       string
		format     doblurav1alpha1.SnapshotFormat
		neutralize *bool
		wantNeut   bool
	}{
		{"OdooBackup unspecified", doblurav1alpha1.FormatOdooBackup, nil, true},
		{"OdooBackup true", doblurav1alpha1.FormatOdooBackup, ptr(true), true},
		{"OdooBackup false", doblurav1alpha1.FormatOdooBackup, ptr(false), false},
		{"PgDump unspecified", doblurav1alpha1.FormatPgDump, nil, true},
		{"PgDump false", doblurav1alpha1.FormatPgDump, ptr(false), false},
		{"PgPlain unspecified", doblurav1alpha1.FormatPgPlain, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &doblurav1alpha1.SnapshotSpec{Format: tc.format, Neutralize: tc.neutralize}
			cmd := spec.RestoreCommand("db1", "/etc/doblura/odoo.conf", "/snapshot")
			// OdooBackup neutralizes through the flag; the Postgres formats through
			// Odoo's native command after restoring.
			got := strings.Contains(cmd, "--neutralize") || strings.Contains(cmd, " neutralize -d")
			if got != tc.wantNeut {
				t.Fatalf("neutralization = %v, expected %v\n  %s", got, tc.wantNeut, cmd)
			}
		})
	}
}

// All four providers end in the same place: the dump at /snapshot. That is the
// internal contract, and it is what lets Custom be first class.
func TestEveryProviderEndsAtSnapshotMountPath(t *testing.T) {
	cases := map[string]doblurav1alpha1.SnapshotProvider{
		"Volume": {
			Type:   doblurav1alpha1.ProviderVolume,
			Volume: &doblurav1alpha1.VolumeProvider{ClaimName: "d", SubPath: "prod-anon"},
		},
		"ObjectStore": {
			Type:        doblurav1alpha1.ProviderObjectStore,
			ObjectStore: &doblurav1alpha1.ObjectStoreProvider{Bucket: "b", Key: "k", Region: "eu-west-1"},
		},
		"HTTP": {
			Type: doblurav1alpha1.ProviderHTTP,
			HTTP: &doblurav1alpha1.HTTPProvider{URL: "https://x/dump"},
		},
		"Custom": {
			Type:   doblurav1alpha1.ProviderCustom,
			Custom: &doblurav1alpha1.CustomProvider{Image: "mio:1", Command: []string{"/traer.sh"}},
		},
	}

	for name, prov := range cases {
		t.Run(name, func(t *testing.T) {
			vols, mounts, inits := snapshotPlumbing(&doblurav1alpha1.SnapshotSpec{From: prov})

			var found bool
			for _, m := range mounts {
				if m.MountPath == doblurav1alpha1.SnapshotMountPath {
					found = true
					if !m.ReadOnly {
						t.Error("/snapshot must be ReadOnly for the main container")
					}
				}
			}
			if !found {
				t.Fatalf("no mount at %s", doblurav1alpha1.SnapshotMountPath)
			}
			if len(vols) == 0 {
				t.Error("a snapshot volume is required")
			}

			// Volume needs no fetcher; the other three do.
			wantInit := name != "Volume"
			if (len(inits) > 0) != wantInit {
				t.Errorf("initContainers=%d, expected fetcher=%v", len(inits), wantInit)
			}
			// And the fetcher writes where the others read.
			for _, ic := range inits {
				for _, m := range ic.VolumeMounts {
					if m.MountPath == doblurav1alpha1.SnapshotMountPath && m.ReadOnly {
						t.Error("the fetcher needs /snapshot writable")
					}
				}
			}
		})
	}
}

// With no credentials Secret, S3 uses the environment's: that is what you want
// with IRSA or Workload Identity, where there are no keys to rotate.
func TestObjectStoreWithoutSecretUsesEnvAuth(t *testing.T) {
	env := objectStoreEnv(&doblurav1alpha1.ObjectStoreProvider{Bucket: "b", Key: "k"})
	var envAuth bool
	for _, e := range env {
		if e.Name == "RCLONE_CONFIG_S_ENV_AUTH" && e.Value == "true" {
			envAuth = true
		}
		if strings.Contains(e.Name, "ACCESS_KEY") {
			t.Error("without CredentialsSecret it must not declare keys")
		}
	}
	if !envAuth {
		t.Error("it must enable env_auth for IRSA/Workload Identity")
	}
}

// A key ending in a slash is a prefix: copy the tree, not one object.
func TestObjectStoreTellsPrefixFromObject(t *testing.T) {
	obj := objectStoreScript(&doblurav1alpha1.ObjectStoreProvider{Bucket: "b", Key: "d/dump.zip"})
	if !strings.Contains(obj, "copyto") {
		t.Error("a single object is fetched with copyto")
	}
	pre := objectStoreScript(&doblurav1alpha1.ObjectStoreProvider{Bucket: "b", Key: "dumps/"})
	if !strings.Contains(pre, "rclone --config /dev/null copy ") {
		t.Error("a prefix is fetched with copy")
	}
}

// Custom is first class: its extra PVCs and Secrets must genuinely reach the
// pod.
func TestCustomProviderIsFirstClass(t *testing.T) {
	prov := doblurav1alpha1.SnapshotProvider{
		Type: doblurav1alpha1.ProviderCustom,
		Custom: &doblurav1alpha1.CustomProvider{
			Image:             "internal.registry/fetch-backup:3",
			Command:           []string{"/bin/fetch"},
			Env:               map[string]string{"APPLIANCE": "nfs-01"},
			EnvFromSecrets:    []string{"appliance-creds"},
			ExtraVolumeClaims: []string{"nfs-backups"},
		},
	}
	_, _, inits := snapshotPlumbing(&doblurav1alpha1.SnapshotSpec{From: prov})
	if len(inits) != 1 {
		t.Fatalf("expected 1 fetcher, found %d", len(inits))
	}
	ic := inits[0]
	if ic.Image != "internal.registry/fetch-backup:3" {
		t.Errorf("image = %q", ic.Image)
	}
	if len(ic.EnvFrom) != 1 {
		t.Error("EnvFromSecrets must reach the container")
	}
	var mounted bool
	for _, m := range ic.VolumeMounts {
		if m.MountPath == "/mnt/nfs-backups" {
			mounted, _ = true, m
			if !m.ReadOnly {
				t.Error("extra PVCs are mounted ReadOnly")
			}
		}
	}
	if !mounted {
		t.Error("the extra PVC mount is missing")
	}
	vols := customExtraVolumes(&prov)
	if len(vols) != 1 || vols[0].PersistentVolumeClaim == nil {
		t.Error("the extra PVC must be declared as a pod volume")
	}
}

// curl without --fail saves the error page as if it were the dump, and the
// failure only shows up much later, at restore time.
func TestHTTPUsesFailSoItDoesNotSaveA404(t *testing.T) {
	s := httpScript(&doblurav1alpha1.HTTPProvider{URL: "https://x/dump"})
	if !strings.Contains(s, "--fail") {
		t.Error("curl must pass --fail")
	}
}

func TestMigrateScriptPerEngine(t *testing.T) {
	cases := map[doblurav1alpha1.MigrationEngine]string{
		doblurav1alpha1.EngineClickOdooUpdate: "click-odoo-update",
		doblurav1alpha1.EngineOdooUpdateAll:   "-u all",
		doblurav1alpha1.EngineMarabunta:       "marabunta",
		"":                                    "click-odoo-update", // the default
	}
	for engine, want := range cases {
		reh := &doblurav1alpha1.OdooRehearsal{
			Spec: doblurav1alpha1.OdooRehearsalSpec{
				Migration: doblurav1alpha1.MigrationSpec{Engine: engine},
			},
		}
		st := &doblurav1alpha1.OdooRehearsalStatus{DatabaseName: "db1"}
		if got := migrateScript(reh, st); !strings.Contains(got, want) {
			t.Errorf("engine %q: expected %q in the script, got:\n%s", engine, want, got)
		}
	}
}

// The budget is not a timeout: a migration that finishes cleanly but overruns
// the budget must FAIL the rehearsal.
func TestExceededBudgetFailsTheRehearsal(t *testing.T) {
	r := &OdooRehearsalReconciler{}
	reh := &doblurav1alpha1.OdooRehearsal{
		Spec: doblurav1alpha1.OdooRehearsalSpec{
			Budget: &doblurav1alpha1.Budget{
				MaxUpgradeDuration: &metav1.Duration{Duration: mustDur("30m")},
			},
		},
	}
	st := &doblurav1alpha1.OdooRehearsalStatus{Phase: doblurav1alpha1.PhaseMigrating}

	job := jobRunFor("2h")
	r.advance(reh, st, job)

	if st.Phase != doblurav1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, expected Failed", st.Phase)
	}
	if st.UpgradeDuration == nil {
		t.Fatal("the duration must be recorded even on failure: it is the number people come for")
	}
	if c := findCond(st, doblurav1alpha1.ConditionMigrated); c == nil || c.Status != metav1.ConditionTrue {
		t.Error("Migrated must be True: the migration DID finish; what failed is that it does not fit")
	}
	if c := findCond(st, doblurav1alpha1.ConditionWithinBudget); c == nil || c.Status != metav1.ConditionFalse {
		t.Error("WithinBudget must be False and separate from Migrated")
	}
}

func TestWithinBudgetAdvances(t *testing.T) {
	r := &OdooRehearsalReconciler{}
	reh := &doblurav1alpha1.OdooRehearsal{
		Spec: doblurav1alpha1.OdooRehearsalSpec{
			Budget: &doblurav1alpha1.Budget{
				MaxUpgradeDuration: &metav1.Duration{Duration: mustDur("2h")},
			},
		},
	}
	st := &doblurav1alpha1.OdooRehearsalStatus{Phase: doblurav1alpha1.PhaseMigrating}
	r.advance(reh, st, jobRunFor("30m"))

	if st.Phase != doblurav1alpha1.PhaseAsserting {
		t.Fatalf("phase = %q, expected Asserting", st.Phase)
	}
}

func TestUnknownSizeFallsBackToMedium(t *testing.T) {
	got := sizeToResources("inventado")
	want := sizeToResources(doblurav1alpha1.SizeMedium)
	if got.Requests.Memory().Cmp(*want.Requests.Memory()) != 0 {
		t.Error("an unknown size must fall back to medium, not to empty")
	}
}

func TestAssertScriptGeneratesPythonPerModel(t *testing.T) {
	reh := &doblurav1alpha1.OdooRehearsal{
		Spec: doblurav1alpha1.OdooRehearsalSpec{
			Assertions: doblurav1alpha1.Assertions{
				ModelCounts: []doblurav1alpha1.ModelCountAssertion{
					{Model: "account.move", MinCount: 100},
					{Model: "res.partner"},
				},
			},
		},
	}
	st := &doblurav1alpha1.OdooRehearsalStatus{DatabaseName: "db1"}
	out := assertScript(reh, st)
	for _, want := range []string{"account.move", "res.partner", "rollback"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q is missing from the generated script", want)
		}
	}
	// click-odoo validates SCRIPT as an existing file and rejects "-". Found by
	// the first real end-to-end run, which is exactly what it is for.
	if strings.Contains(out, `-d "db1" -`) {
		t.Error("click-odoo must not be given a dash as SCRIPT: it rejects it outright")
	}
	if !strings.Contains(out, "/tmp/doblura-assert.py") {
		t.Error("the assertion script must be written to a file and passed by path")
	}
}

// The central invariant of the addons design: no copying into a persistent
// volume. Repos go to an emptyDir; the PVC is mounted ReadOnly.
func TestAddonsNeverCopyIntoAPersistentVolume(t *testing.T) {
	spec := &doblurav1alpha1.AddonsSpec{
		Repos:  []doblurav1alpha1.AddonRepo{{Name: "r1", URL: "https://x/y", Ref: "17.0"}},
		Volume: &doblurav1alpha1.AddonVolume{ClaimName: "agg"},
	}
	vols, mounts, inits := addonsPlumbing(spec)

	for _, v := range vols {
		if v.Name == "addons-repos" && v.EmptyDir == nil {
			t.Error("cloned repos must go to an emptyDir, not a PVC")
		}
		if v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("the addons PVC %q must be mounted ReadOnly", v.Name)
		}
	}
	for _, m := range mounts {
		if !m.ReadOnly && m.Name != "tmp" {
			t.Errorf("the main container mount %q must be ReadOnly", m.Name)
		}
	}
	if len(inits) != 1 {
		t.Fatalf("expected 1 clone init container, found %d", len(inits))
	}
	// And the script must not copy anything anywhere.
	for _, a := range inits[0].Args {
		if strings.Contains(a, "cp -r") || strings.Contains(a, "rsync") {
			t.Error("the init container must not copy addons, only clone into its emptyDir")
		}
	}
}

// The token must never end up in the logs.
func TestCloneScriptObfuscatesCredentials(t *testing.T) {
	s := cloneScript(doblurav1alpha1.AddonRepo{
		Name: "priv", URL: "https://github.com/acme/private", Ref: "17.0",
		Auth:  &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthToken, SecretRef: "gh"},
		Depth: 1,
	})
	if !strings.Contains(s, `s#://[^@]*@#://***@#g`) {
		t.Error("git output must be filtered so the token is never printed")
	}
	if !strings.Contains(s, "credential.helper") {
		t.Error("credentials must go through credential.helper, not embedded in the URL")
	}
	if strings.Contains(s, "$URL") && strings.Contains(s, "${GIT_PASSWORD}@") {
		t.Error("the token must not end up in the URL: it would land in .git/config")
	}
	if !strings.Contains(s, "GIT_SSH_COMMAND") {
		t.Error("it must support an SSH key")
	}
}

// Each auth type injects only its own keys. Marking everything optional and
// letting the script guess is what produces "authentication failed" with no
// clues.
func TestAuthEnvPerType(t *testing.T) {
	cases := []struct {
		name     string
		auth     *doblurav1alpha1.GitAuth
		url      string
		wantVars map[string]string // name -> expected literal value ("" = comes from a Secret)
	}{
		{
			name: "no auth, public repo",
			auth: nil,
		},
		{
			name:     "a github token derives x-access-token",
			auth:     &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthToken, SecretRef: "s"},
			url:      "https://github.com/OCA/x",
			wantVars: map[string]string{"GIT_USER": "x-access-token", "GIT_PASSWORD": ""},
		},
		{
			name:     "a gitlab token derives oauth2",
			auth:     &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthToken, SecretRef: "s"},
			url:      "https://gitlab.com/acme/x",
			wantVars: map[string]string{"GIT_USER": "oauth2", "GIT_PASSWORD": ""},
		},
		{
			name:     "an explicit username beats the heuristic",
			auth:     &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthToken, SecretRef: "s", Username: "mio"},
			url:      "https://git.interno.example/x",
			wantVars: map[string]string{"GIT_USER": "mio", "GIT_PASSWORD": ""},
		},
		{
			name:     "basic auth takes both from the Secret",
			auth:     &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthBasicAuth, SecretRef: "s"},
			url:      "https://gitlab.com/acme/x",
			wantVars: map[string]string{"GIT_USER": "", "GIT_PASSWORD": ""},
		},
		{
			name:     "ssh key",
			auth:     &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthSSHKey, SecretRef: "s"},
			url:      "git@github.com:acme/x",
			wantVars: map[string]string{"GIT_SSH_KEY": "", "GIT_KNOWN_HOSTS": ""},
		},
		{
			name:     "github app uses the already-minted token",
			auth:     &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthGitHubApp, SecretRef: "s"},
			url:      "https://github.com/acme/x",
			wantVars: map[string]string{"GIT_USER": "x-access-token", "GIT_PASSWORD": ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := authEnv(doblurav1alpha1.AddonRepo{Name: "r", URL: tc.url, Auth: tc.auth})
			if len(env) != len(tc.wantVars) {
				t.Fatalf("expected %d variables, found %d: %+v", len(tc.wantVars), len(env), env)
			}
			for _, e := range env {
				want, ok := tc.wantVars[e.Name]
				if !ok {
					t.Errorf("unexpected variable %q", e.Name)
					continue
				}
				if want != "" && e.Value != want {
					t.Errorf("%s = %q, expected %q", e.Name, e.Value, want)
				}
				if want == "" && e.ValueFrom == nil {
					t.Errorf("%s should come from a Secret, not in plaintext", e.Name)
				}
			}
		})
	}
}

// GitHub App is the only mechanism that needs the operator to make a network
// call before the Job.
func TestOnlyGitHubAppNeedsMinting(t *testing.T) {
	for _, ty := range []doblurav1alpha1.GitAuthType{
		doblurav1alpha1.AuthToken, doblurav1alpha1.AuthBasicAuth, doblurav1alpha1.AuthSSHKey,
	} {
		a := &doblurav1alpha1.GitAuth{Type: ty}
		if a.NeedsTokenMinting() {
			t.Errorf("%s should not need minting", ty)
		}
	}
	app := &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthGitHubApp}
	if !app.NeedsTokenMinting() {
		t.Error("GitHubApp does need minting")
	}
	var nilAuth *doblurav1alpha1.GitAuth
	if nilAuth.NeedsTokenMinting() {
		t.Error("a nil auth must not panic and must not need minting")
	}
}

// A GitHub App private key arrives as PKCS#1 or PKCS#8 depending on where you
// got it from. Accepting only one is a classic cause of "invalid key".
func TestParseRSAKeyAcceptsBothFormats(t *testing.T) {
	k, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	for name, data := range map[string][]byte{"PKCS#1": pkcs1, "PKCS#8": pkcs8} {
		if _, err := parseRSAKey(data); err != nil {
			t.Errorf("%s should parse: %v", name, err)
		}
	}
	if _, err := parseRSAKey([]byte("no soy pem")); err == nil {
		t.Error("garbage should error out")
	}
}

// The JWT must have three parts and an iat in the past: GitHub rejects JWTs from
// the future and node clocks drift.
func TestSignAppJWTShape(t *testing.T) {
	k, _ := rsa.GenerateKey(crand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
	tok, err := signAppJWT("123456", pemKey)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT has 3 parts, found %d", len(parts))
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ Iat, Exp int64 }
	if err := json.Unmarshal(claims, &c); err != nil {
		t.Fatal(err)
	}
	if c.Iat >= time.Now().Unix() {
		t.Error("iat must be in the past to tolerate clock drift")
	}
	if c.Exp-c.Iat > 600 {
		t.Error("GitHub rejects JWTs older than 10 minutes")
	}
}

// The addons path genuinely ends up in the odoo.conf the three phases use.
func TestOdooConfCarriesTheAddonsPath(t *testing.T) {
	reh := &doblurav1alpha1.OdooRehearsal{
		Spec: doblurav1alpha1.OdooRehearsalSpec{
			Database: doblurav1alpha1.DatabaseSpec{Host: "pg", User: "odoo"},
			Addons: doblurav1alpha1.AddonsSpec{
				Baked: []string{"/opt/odoo/custom"},
				Repos: []doblurav1alpha1.AddonRepo{{Name: "oca", URL: "u"}},
			},
		},
	}
	conf := odooConf(reh, "db1")
	if !strings.Contains(conf, "addons_path = "+doblurav1alpha1.AddonRepoMountBase+"/oca,/opt/odoo/custom") {
		t.Errorf("addons path composed incorrectly:\n%s", conf)
	}
	if strings.Contains(conf, "db_password") || strings.Contains(conf, "PGPASSWORD") {
		t.Error("the password must NOT go into the ConfigMap")
	}
	if !strings.Contains(conf, "db_port = 5432") {
		t.Error("the port must fall back to the default")
	}
}

// Advancing a phase must be reported, because Reconcile has to requeue when it
// happens: nothing else will wake the controller. GenerationChangedPredicate
// filters our own status writes and the Job that triggered us is already in its
// final state. The first real end-to-end run stalled in Asserting for exactly
// this reason.
func TestAdvanceReportsThePreviousPhase(t *testing.T) {
	r := &OdooRehearsalReconciler{}
	reh := &doblurav1alpha1.OdooRehearsal{}

	st := &doblurav1alpha1.OdooRehearsalStatus{Phase: doblurav1alpha1.PhaseRestoring}
	if was := r.advance(reh, st, jobRunFor("10s")); was != doblurav1alpha1.PhaseRestoring {
		t.Errorf("advance must return the previous phase, got %q", was)
	}
	if st.Phase != doblurav1alpha1.PhaseMigrating {
		t.Errorf("Restoring must advance to Migrating, got %q", st.Phase)
	}

	// And the whole chain has to be reachable, phase by phase.
	chain := []doblurav1alpha1.RehearsalPhase{
		doblurav1alpha1.PhaseRestoring, doblurav1alpha1.PhaseMigrating,
		doblurav1alpha1.PhaseAsserting, doblurav1alpha1.PhaseSucceeded,
	}
	st = &doblurav1alpha1.OdooRehearsalStatus{Phase: chain[0]}
	for i := 0; i < len(chain)-1; i++ {
		if st.Phase != chain[i] {
			t.Fatalf("step %d: phase is %q, expected %q", i, st.Phase, chain[i])
		}
		r.advance(reh, st, jobRunFor("10s"))
	}
	if st.Phase != doblurav1alpha1.PhaseSucceeded {
		t.Errorf("the chain must reach Succeeded, stopped at %q", st.Phase)
	}
	if !terminal(st.Phase) {
		t.Error("Succeeded must be terminal")
	}
	if terminal(doblurav1alpha1.PhaseAsserting) {
		t.Error("Asserting must not be terminal")
	}
}
