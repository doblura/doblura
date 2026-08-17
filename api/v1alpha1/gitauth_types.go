// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

// ─────────────────────────── Git authentication ───────────────────────────
//
// "Private repos" is not one thing, it is four, and every forge pushes you
// towards a different one. Inferring which is in use from whichever keys happen
// to be in the Secret looks convenient and is a trap: when it fails the message
// is "authentication failed" and you lose an afternoon. So the type is
// explicit.
//
//	Token      GitHub PAT, GitLab project/group access token, Gitea token.
//	           The simplest and what almost everyone uses.
//	BasicAuth  username + password. GitLab deploy tokens and Bitbucket app
//	           passwords are this, not a bare token.
//	SSHKey     deploy key. The only option when the forge has no per-repository
//	           read tokens.
//	GitHubApp  a GitHub App installation token. The right path inside an
//	           organisation: per-repository permissions, auditable, revocable
//	           without touching anyone, and NOT tied to a person who will
//	           eventually leave.
//
// That last point matters more than it looks: a personal PAT in a pipeline is a
// time bomb. The day its owner leaves the company the token is revoked and the
// migration rehearsal stops working, right when nobody remembers why.

// GitAuthType is the authentication mechanism.
// +kubebuilder:validation:Enum=Token;BasicAuth;SSHKey;GitHubApp
type GitAuthType string

const (
	// AuthToken uses a token as the password over HTTPS.
	// Secret keys: "token" (required), "username" (optional).
	AuthToken GitAuthType = "Token"

	// AuthBasicAuth uses a username and a password.
	// Secret keys: "username", "password".
	AuthBasicAuth GitAuthType = "BasicAuth"

	// AuthSSHKey uses an SSH private key.
	// Secret keys: "ssh-privatekey", "known_hosts" (optional).
	AuthSSHKey GitAuthType = "SSHKey"

	// AuthGitHubApp mints an installation token from a GitHub App's
	// credentials. The operator performs the exchange and leaves the token in
	// an ephemeral Secret it owns, which is garbage-collected with the
	// rehearsal.
	// Secret keys: "appID", "installationID", "privateKey".
	AuthGitHubApp GitAuthType = "GitHubApp"
)

// Default usernames per forge when using AuthToken. Tokens travel as the
// password, but the username each forge expects differs, and getting it wrong
// yields a 403 that explains nothing.
const (
	// GitHubTokenUser is the username GitHub expects for a PAT or an
	// installation token.
	GitHubTokenUser = "x-access-token"
	// GitLabTokenUser is the username GitLab expects for an access token.
	GitLabTokenUser = "oauth2"
)

// GitAuth declares how to authenticate against a repository.
type GitAuth struct {
	// Type is the mechanism. Explicit on purpose: see the package comment on
	// why it is not inferred.
	Type GitAuthType `json:"type"`

	// SecretRef is the Secret holding the keys the Type requires. It must live
	// in the same namespace as the OdooRehearsal: reading Secrets from other
	// namespaces would turn Doblura into a privilege-escalation vector.
	// +kubebuilder:validation:MinLength=1
	SecretRef string `json:"secretRef"`

	// Username overrides the forge's default username. It only applies to
	// Token. Left empty, it is derived from the URL: x-access-token for
	// github.com, oauth2 for gitlab.com.
	// +optional
	Username string `json:"username,omitempty"`
}

// DefaultTokenUserFor derives the username from the repository URL.
//
// It is a heuristic, which is why Username exists: for self-hosted instances,
// which cannot be guessed from the host.
func DefaultTokenUserFor(url string) string {
	switch {
	case contains(url, "gitlab"):
		return GitLabTokenUser
	default:
		// x-access-token works on GitHub and also on Gitea and Forgejo, which
		// ignore the username when the password is a valid token.
		return GitHubTokenUser
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// NeedsTokenMinting says whether the operator must mint a token before the init
// container can clone.
//
// Only GitHubApp needs it: every other mechanism is passed straight to the
// container. This distinction decides whether the reconciler has to make a
// network call before creating the Job, and it is worth having in one place.
func (g *GitAuth) NeedsTokenMinting() bool {
	return g != nil && g.Type == AuthGitHubApp
}
