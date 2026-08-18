// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

// ─────────────────────────── Addons ───────────────────────────
//
// The antipattern this type exists to avoid
// ─────────────────────────────────────────
//
// The usual way to get addons into an Odoo container is to bake them into the
// image and then, on startup, copy them into a volume so they "persist". That
// breaks in three separate ways:
//
//  1. The copy runs on EVERY startup. With a few hundred modules that is
//     minutes before Odoo listens, and the probes kill the pod before it
//     finishes.
//  2. If the volume already has content, the copy either overwrites it or
//     merges with it, and you end up running a mixture of two versions that
//     exists in no commit.
//  3. With more than one replica, two pods copy into the same volume at once.
//
// Doblura NEVER copies addons into a volume. All three sources are read-only:
//
//   - Baked:  already in the image. They are read WHERE THEY ARE. Zero copies.
//   - Repos:  an init container clones them into an ephemeral emptyDir, which
//             is born empty in every pod. There is no state to mix.
//   - Volume: a PVC populated by another process (a CronJob running
//             git-aggregator). Mounted ReadOnly.
//
// And the operator COMPOSES the addons path from all three, in an explicit
// order. Nobody maintains that list by hand, which is the other classic source
// of "it loads on my machine".

// AddonsPrecedence decides who wins when the same module appears in more than
// one source. In Odoo the first entry on the addons path wins, which is why
// this has to be explicit: the alternative is loading different code than you
// think, and that is exactly the class of failure Doblura exists to catch.
// +kubebuilder:validation:Enum=ExternalFirst;BakedFirst
type AddonsPrecedence string

const (
	// PrecedenceExternalFirst puts repos and volumes ahead of the image. It is
	// the default: you bake a known-good set into the image and override it
	// selectively from outside to test a PR or a hotfix without rebuilding.
	PrecedenceExternalFirst AddonsPrecedence = "ExternalFirst"

	// PrecedenceBakedFirst puts the image first. Useful when the image is the
	// source of truth and external sources only add new modules.
	PrecedenceBakedFirst AddonsPrecedence = "BakedFirst"
)

// OdooEdition distinguishes Community from Enterprise.
// +kubebuilder:validation:Enum=Community;Enterprise
type OdooEdition string

const (
	EditionCommunity  OdooEdition = "Community"
	EditionEnterprise OdooEdition = "Enterprise"
)

// AddonsSpec says where the addons come from.
//
// Everything can be combined: some baked, some cloned, some on a volume. The
// operator composes the resulting addons path.
//
// +kubebuilder:validation:XValidation:rule="self.edition != 'Enterprise' || size(self.baked) > 0 || size(self.repos) > 0 || has(self.volume)",message="edition Enterprise needs at least one addons source: the enterprise modules do not ship in the community image"
type AddonsSpec struct {
	// Edition is Community or Enterprise. It does not change the operator's
	// behaviour, but it does change validation and what gets recorded in the
	// status: knowing which edition a migration was rehearsed against matters
	// when something breaks six months later.
	// +kubebuilder:default=Community
	// +optional
	Edition OdooEdition `json:"edition,omitempty"`

	// Baked lists addon paths that ALREADY ship inside the image. They are read
	// where they are: not copied, not moved, not mounted.
	//
	// Example: ["/opt/odoo/addons-custom", "/opt/odoo/addons-oca"]
	// +optional
	Baked []string `json:"baked,omitempty"`

	// Repos are cloned by an init container into an ephemeral emptyDir. Every
	// pod starts with the volume empty, so no mixture of versions is possible.
	// +optional
	Repos []AddonRepo `json:"repos,omitempty"`

	// Volume is a PVC already populated by another process. It is mounted
	// ReadOnly, so a rehearsal cannot corrupt what others share.
	// +optional
	Volume *AddonVolume `json:"volume,omitempty"`

	// +kubebuilder:default=ExternalFirst
	// +optional
	Precedence AddonsPrecedence `json:"precedence,omitempty"`
}

// AddonRepo is a git repository of addons, public or private.
type AddonRepo struct {
	// Name identifies the repo and names its directory inside the emptyDir. It
	// must be unique within the same AddonsSpec.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// URL of the repository. HTTPS or SSH.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Ref is a branch, a tag or a commit.
	//
	// For a rehearsal, USE A COMMIT. A branch makes the rehearsal
	// non-reproducible: repeating it tomorrow clones different code and the
	// result stops meaning anything.
	// +kubebuilder:default=main
	// +optional
	Ref string `json:"ref,omitempty"`

	// Paths are the subdirectories of the repo that contain addons. Empty means
	// the root, which is the norm for OCA repositories.
	//
	// A repo with addons in per-version subfolders is declared as:
	// paths: ["17.0"]
	// +optional
	Paths []string `json:"paths,omitempty"`

	// Auth authenticates private repos: PAT, username/password, deploy key or
	// GitHub App. Empty means a public repo.
	//
	// The Odoo Enterprise repository is private, so the EE edition necessarily
	// goes through here.
	// +optional
	Auth *GitAuth `json:"auth,omitempty"`

	// Depth is the clone depth. 1 by default: a rehearsal does not need history
	// and cloning all of OCA costs minutes.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Depth int32 `json:"depth,omitempty"`
}

// AddonVolume is a PVC with addons already in place.
type AddonVolume struct {
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// Paths are the paths inside the volume that contain addons.
	// +kubebuilder:default={"/addons"}
	// +optional
	Paths []string `json:"paths,omitempty"`
}

// AddonsPathFor composes the final addons path.
//
// It lives in the API rather than the controller on purpose: it is the part of
// the contract users need to reason about without reading the reconciler, and
// it tests on its own.
//
// mountBase is where the init container leaves the cloned repos.
func (a *AddonsSpec) AddonsPathFor(mountBase string) []string {
	external := make([]string, 0, len(a.Repos)+2)
	for _, r := range a.Repos {
		if len(r.Paths) == 0 {
			external = append(external, mountBase+"/"+r.Name)
			continue
		}
		for _, p := range r.Paths {
			external = append(external, mountBase+"/"+r.Name+"/"+p)
		}
	}
	if a.Volume != nil {
		paths := a.Volume.Paths
		if len(paths) == 0 {
			paths = []string{"/addons"}
		}
		for _, p := range paths {
			external = append(external, AddonVolumeMountPath+p)
		}
	}

	baked := append([]string(nil), a.Baked...)

	if a.Precedence == PrecedenceBakedFirst {
		return append(baked, external...)
	}
	return append(external, baked...)
}

// ─────────────── What the image already provides ───────────────
//
// Doblura assumed the official odoo image everywhere: `odoo` on the PATH, the
// server's own addons found without help, uid 100. That assumption is wrong for
// the scaffolding most Odoo shops actually build on, and wrong in ways that do
// not announce themselves.
//
// Measured against ghcr.io/tecnativa/doodba:18.0 rather than remembered:
//
//	                     official odoo:18        doodba:18.0
//	 runs as             uid 100 (odoo)          root; there is no odoo user
//	 uid 100 is          odoo                    messagebus
//	 entrypoint/cmd      entrypoint.sh / odoo    none / python3
//	 addons              found by the server     /opt/odoo/auto/addons
//	 config              -c, or /etc/odoo        $OPENERP_SERVER, /opt/odoo/auto
//	 click-odoo-contrib  absent                  present, on the PATH
//	 /var/lib/odoo       exists                  does not exist
//
// Two of those are not cosmetic. Doodba's cmd is `python3`, so the arguments
// doblura passes — `-c /etc/doblura/odoo.conf` — would hand the configuration
// file to the Python interpreter as a program to execute. And Doodba's own
// entrypoint writes /opt/odoo/auto/odoo.conf at startup, into a root-owned 755
// directory, which cannot work under runAsNonRoot.
//
// The second one turns out not to matter, because doblura generates the
// configuration itself and mounts it read-only: the entrypoint is not needed and
// is deliberately bypassed. The first one matters a great deal, and is why the
// flavour sets the command rather than only the paths.
//
// The flavour is DECLARED and then VERIFIED. It is not sniffed: probing an image
// to guess its layout is how a wrong guess becomes a confusing runtime failure
// instead of an admission error. The preflight in every phase Job checks what the
// flavour promised and fails naming exactly what was missing.

// ImageFlavor says how an Odoo image is put together.
// +kubebuilder:validation:Enum=Official;Doodba;Custom
type ImageFlavor string

const (
	// FlavorOfficial is the odoo image published by Odoo SA.
	FlavorOfficial ImageFlavor = "Official"
	// FlavorDoodba is Tecnativa's scaffolding, and images built from it.
	FlavorDoodba ImageFlavor = "Doodba"
	// FlavorCustom promises nothing. Declare addons.baked yourself; the
	// preflight still checks that what you declared exists.
	FlavorCustom ImageFlavor = "Custom"
)

// DoodbaAddonsPath is where Doodba's build step assembles every addon it has.
const DoodbaAddonsPath = "/opt/odoo/auto/addons"

// FlavorBakedPaths are the addon directories the image ships, by flavour.
//
// Prepended to whatever the user declared in addons.baked rather than replacing
// it: an image can be Doodba AND carry something else, and silently dropping a
// declared path would be the worse failure of the two.
func FlavorBakedPaths(f ImageFlavor) []string {
	if f == FlavorDoodba {
		return []string{DoodbaAddonsPath}
	}
	return nil
}

// FlavorCommand is the entrypoint override, or nil to use the image's own.
//
// Doodba needs one. Its cmd is `python3`, so appending doblura's arguments would
// run `python3 -c /etc/doblura/odoo.conf`, handing an ini file to the Python
// interpreter — which fails with a SyntaxError about a config file, in a pod
// whose logs give no hint that the image was never going to accept those
// arguments.
func FlavorCommand(f ImageFlavor) []string {
	if f == FlavorDoodba {
		return []string{"odoo"}
	}
	return nil
}

// AddonVolumeMountPath is where the addons PVC is mounted.
const AddonVolumeMountPath = "/mnt/addons-volume"

// DataDirPath is Odoo's data directory inside every pod Doblura creates. Backed
// by an emptyDir: a rehearsal's filestore is scratch, exactly like its database.
//
// It has to be set explicitly. Odoo defaults data_dir to $HOME/.local/share/Odoo,
// and the pod runs as UID 65532 which has no home directory in most images, so
// HOME resolves to "/" and the path lands on a read-only root filesystem. The
// restore then dies on the filestore with a FileNotFoundError naming a path
// nobody configured.
const DataDirPath = "/var/lib/odoo"

// StagePath is where a mutating restore stages its writable copy of the dump.
// See restoreScript in internal/controller for why that copy exists.
const StagePath = "/stage"

// AddonRepoMountBase is where the init container leaves the cloned repos.
// It is an emptyDir: born empty in every pod, and that is the whole point.
const AddonRepoMountBase = "/mnt/addons-repos"
