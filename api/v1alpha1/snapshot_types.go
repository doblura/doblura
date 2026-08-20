// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

// ─────────────────────────── Snapshots ───────────────────────────
//
// The design principle: generic first, provider second.
//
// Every snapshot provider, however unusual, reduces to the same internal
// contract:
//
//	"leave a dump at /snapshot and exit 0"
//
// That is all. One container, one path, one exit code. From there:
//
//   - Volume       fetches nothing: it mounts a PVC at /snapshot.
//   - ObjectStore  a container that downloads from anything S3-compatible.
//                  ONE implementation covers AWS, MinIO, Ceph, R2, Wasabi,
//                  Backblaze and whatever comes next: it is the same protocol.
//   - HTTP         a container that runs curl.
//   - Custom       you supply the container.
//
// Custom is not a second-class escape hatch: it is THE SAME mechanism the
// built-ins use. If we ever add a native GCS or Azure provider it will be a
// preset that fills in this contract, not a parallel code path. That is the
// test of a sound abstraction: the built-ins are expressible through the
// extension point.
//
// Which is why the provider layer is deliberately thin. If your backup lives
// on an NFS appliance, in Bacula, or behind a proprietary API from 2004, Doblura
// does not need to know: you write twenty lines of shell in a Custom provider
// and it works exactly like the rest.

// SnapshotMountPath is the contract: the dump must be here when the fetch
// container finishes.
const SnapshotMountPath = "/snapshot"

// SnapshotProviderType is where the bytes come from.
// +kubebuilder:validation:Enum=Volume;ObjectStore;HTTP;Custom
type SnapshotProviderType string

const (
	// ProviderVolume mounts a PVC that already holds the dump. The recommended
	// path: another process produces the dump (a CronJob that anonymizes) and
	// Doblura only reads it.
	ProviderVolume SnapshotProviderType = "Volume"

	// ProviderObjectStore downloads from S3-compatible storage. Deliberately
	// generic: there is no type per cloud, because it is the same protocol.
	ProviderObjectStore SnapshotProviderType = "ObjectStore"

	// ProviderHTTP downloads from a URL. Useful for internally published dumps.
	ProviderHTTP SnapshotProviderType = "HTTP"

	// ProviderCustom runs your container. Contract: leave the dump at /snapshot
	// and exit 0.
	ProviderCustom SnapshotProviderType = "Custom"
)

// SnapshotFormat is how the dump is packaged.
//
// This axis is independent of the provider: you can have an Odoo dump on a PVC
// or in S3, and where it came from tells you nothing about how to restore it.
// Keeping them separate avoids a provider-times-format explosion.
// +kubebuilder:validation:Enum=OdooBackup;PgDump;PgPlain
type SnapshotFormat string

const (
	// FormatOdooBackup is the output of click-odoo-backupdb, folder or zip. It
	// contains the database AND the filestore, which is exactly what you want:
	// restoring a database without the filestore from the same moment leaves
	// orphaned attachments, and that breaks migrations in confusing ways.
	FormatOdooBackup SnapshotFormat = "OdooBackup"

	// FormatPgDump is pg_dump in custom or directory format. It restores the
	// database only: if your attachments do not live in the database, you need
	// the filestore separately.
	FormatPgDump SnapshotFormat = "PgDump"

	// FormatPgPlain is plain SQL, restored through psql.
	FormatPgPlain SnapshotFormat = "PgPlain"
)

// SnapshotSpec says where the data comes from and how to restore it.
//
// +kubebuilder:validation:XValidation:rule="(!has(self.neutralize) || self.neutralize) || self.unsafeAcknowledgement == 'i-accept-this-can-send-real-emails-and-charge-real-cards'",message="disabling neutralize requires unsafeAcknowledgement set to its literal value, because an un-neutralized production dump sends real emails and charges real cards"
type SnapshotSpec struct {
	// From is the provider.
	From SnapshotProvider `json:"from"`

	// Format is how the dump is packaged.
	// +kubebuilder:default=OdooBackup
	// +optional
	Format SnapshotFormat `json:"format,omitempty"`

	// Neutralize disables scheduled actions, outgoing mail servers, payment
	// providers and delivery carriers after restoring.
	//
	// It defaults to true, and for good reason: a production dump restored with
	// network access and no neutralization sends real invoices to real
	// customers. It is the most expensive failure in this domain and
	// documentation does not prevent it, so the default covers it and disabling
	// it takes effort.
	//
	// Note: neutralizing is NOT anonymizing. This cuts the outbound paths; the
	// personal data is still there.
	// +kubebuilder:default=true
	// +optional
	Neutralize *bool `json:"neutralize,omitempty"`

	// UnsafeAcknowledgement must be exactly
	// "i-accept-this-can-send-real-emails-and-charge-real-cards"
	// for neutralize: false to be accepted.
	// +optional
	UnsafeAcknowledgement string `json:"unsafeAcknowledgement,omitempty"`
}

// SnapshotProvider is a discriminated union: Type says which field to read.
//
// Discriminated rather than "whichever field is set", on purpose. With
// inference, a typo in a field name becomes "no provider given" instead of
// "that field does not exist", and the API server error stops being helpful.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'Volume' || has(self.volume)",message="type Volume requires the volume field"
// +kubebuilder:validation:XValidation:rule="self.type != 'ObjectStore' || has(self.objectStore)",message="type ObjectStore requires the objectStore field"
// +kubebuilder:validation:XValidation:rule="self.type != 'HTTP' || has(self.http)",message="type HTTP requires the http field"
// +kubebuilder:validation:XValidation:rule="self.type != 'Custom' || has(self.custom)",message="type Custom requires the custom field"
type SnapshotProvider struct {
	Type SnapshotProviderType `json:"type"`

	// +optional
	Volume *VolumeProvider `json:"volume,omitempty"`
	// +optional
	ObjectStore *ObjectStoreProvider `json:"objectStore,omitempty"`
	// +optional
	HTTP *HTTPProvider `json:"http,omitempty"`
	// +optional
	Custom *CustomProvider `json:"custom,omitempty"`
}

// VolumeProvider mounts a PVC that already holds the dump.
type VolumeProvider struct {
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// SubPath is the path inside the volume where the dump lives.
	// +kubebuilder:default="prod-anon"
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// ObjectStoreProvider downloads from any S3-compatible store.
//
// Deliberately generic. AWS, MinIO, Ceph RGW, Cloudflare R2, Wasabi, Backblaze
// B2 and DigitalOcean Spaces all speak the same protocol: a type per cloud
// would duplicate the same code five times and still fall short on the sixth.
type ObjectStoreProvider struct {
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Key is the object, or the prefix when the dump is several files.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Endpoint for anything that is not AWS. Empty means AWS.
	// Examples: https://minio.svc:9000, https://<id>.r2.cloudflarestorage.com
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// +kubebuilder:default="us-east-1"
	// +optional
	Region string `json:"region,omitempty"`

	// CredentialsSecret holds the accessKeyID and secretAccessKey keys. Leaving
	// it empty means credentials from the environment, which is what you want
	// with IRSA on EKS or Workload Identity, where there are no keys to store.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// ForcePathStyle is required by MinIO and almost everything self-hosted,
	// where the bucket goes in the path rather than the subdomain.
	// +optional
	ForcePathStyle *bool `json:"forcePathStyle,omitempty"`
}

// HTTPProvider downloads the dump from a URL.
type HTTPProvider struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// AuthSecret holds "username" and "password" for basic auth, or "bearer"
	// for a token.
	// +optional
	AuthSecret string `json:"authSecret,omitempty"`
}

// CustomProvider runs your container to fetch the dump.
//
// The contract, in full:
//
//  1. Leave the dump at /snapshot (the SnapshotMountPath constant).
//  2. Exit 0 on success.
//
// Nothing else. Doblura does not inspect how you got it. A backup on NFS, in
// Bacula, behind a proprietary API, a pg_dump against a live replica: if it
// honours the contract it works exactly like the built-in providers.
type CustomProvider struct {
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// +optional
	Command []string `json:"command,omitempty"`
	// +optional
	Args []string `json:"args,omitempty"`

	// Env holds literal variables. For secrets, use EnvFromSecrets.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// EnvFromSecrets projects whole Secrets as environment variables.
	// +optional
	EnvFromSecrets []string `json:"envFromSecrets,omitempty"`

	// ExtraVolumeClaims mounts additional PVCs, read-only, under
	// /mnt/<claimName>. For when the source is another volume in the cluster.
	// +optional
	ExtraVolumeClaims []string `json:"extraVolumeClaims,omitempty"`
}

// NeedsFetchContainer says whether a container is required to bring the dump in.
//
// Volume is the only provider that does not need one: the dump is already on a
// PVC and mounting it is enough. The rest pull bytes from outside the cluster.
func (p *SnapshotProvider) NeedsFetchContainer() bool {
	return p.Type != ProviderVolume
}

// RestoreCommand returns the restore command for the declared format.
//
// source is where the dump actually is when the command runs. It is not always
// SnapshotMountPath: OdooBackup restores from a writable staged copy, because
// click-odoo-restoredb moves the filestore out of the source folder and the
// snapshot is mounted ReadOnly.
//
// It lives in the API, next to the type that determines it, rather than buried
// in the reconciler: it is part of the observable contract and it tests on its
// own.
func (s *SnapshotSpec) RestoreCommand(dbName, confPath, source string) string {
	neutralize := " --neutralize"
	if s.Neutralize != nil && !*s.Neutralize {
		// Only reachable when the CEL rule accepted the acknowledgement.
		neutralize = ""
	}

	switch s.Format {
	case FormatPgDump:
		// pg_restore cannot neutralize: we restore and then neutralize with
		// Odoo's native command, available since v16.
		//
		// The SUBCOMMAND COMES FIRST. It did not, and Odoo takes its first
		// argument as the command — so `odoo -c conf neutralize -d db` fell back
		// to `server`, died on a stray positional, and took this whole && chain
		// with it. These two formats have never completed a restore with
		// neutralization on. Only FormatOdooBackup, which neutralizes through
		// click-odoo-restoredb, ever worked, and it is the default and the one the
		// e2e exercises.
		cmd := "createdb \"" + dbName + "\" && pg_restore -d \"" + dbName + "\" --no-owner --no-acl " + source
		if neutralize != "" {
			cmd += " && odoo neutralize -c " + confPath + " -d \"" + dbName + "\""
		}
		return cmd

	case FormatPgPlain:
		cmd := "createdb \"" + dbName + "\" && psql -q -d \"" + dbName + "\" -f " + source
		if neutralize != "" {
			cmd += " && odoo neutralize -c " + confPath + " -d \"" + dbName + "\""
		}
		return cmd

	default: // FormatOdooBackup
		// click-odoo-restoredb does neutralize, and it also restores the
		// filestore alongside the database, which is the correct behaviour.
		//
		// --copy is not optional: Odoo needs to know whether the database was
		// moved or copied, and a rehearsal is unambiguously a copy. Getting this
		// wrong makes Odoo treat the scratch database as the same instance,
		// which affects cron ownership and the database uuid.
		//
		// --force so a retry against a leftover scratch database does not fail
		// on "already exists".
		return "click-odoo-restoredb -c " + confPath + " --copy --force" + neutralize +
			" \"" + dbName + "\" " + source
	}
}
