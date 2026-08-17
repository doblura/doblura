// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// ─────────────── Talking to Runboat ───────────────
//
// A small client for the endpoints Doblura actually uses, written against
// Runboat's own router rather than guessed:
//
//	GET  /api/v1/builds[?repo=&target_branch=]   → the list
//	POST /api/v1/builds/{name}/{start,stop,reset}
//
// The list endpoint is unauthenticated. So are start, stop and reset, in
// Runboat itself — see the note on RunboatAction. The credential is sent anyway
// when one is configured, because a deployment behind a reverse proxy that adds
// its own basic auth is the normal way people close that gap.

// RunboatCredentials is a resolved basic-auth pair.
type RunboatCredentials struct {
	Username string
	Password string
}

// runboatAPI is the surface the reconciler depends on, as an interface so the
// tests can drive it without an HTTP server. The alternative — spinning up
// httptest for every case — makes the tests about transport rather than about
// the mirroring logic, which is where the bugs are.
type runboatAPI interface {
	Builds(ctx context.Context, spec *doblurav1alpha1.RunboatLinkSpec, creds *RunboatCredentials) ([]RunboatRemoteBuild, error)
	Act(ctx context.Context, spec *doblurav1alpha1.RunboatLinkSpec, creds *RunboatCredentials, build string, action doblurav1alpha1.RunboatAction) error
}

// RunboatRemoteBuild is Runboat's Build, as it arrives on the wire.
//
// Only the fields Doblura mirrors are declared. Runboat sends more
// (deploy_link_mailhog, repo_commit_link, repo_target_branch_link); ignoring
// them here rather than storing them keeps the mirrored object small, and
// unknown fields are ignored by encoding/json anyway, so a Runboat that adds one
// does not break this.
type RunboatRemoteBuild struct {
	Name       string `json:"name"`
	CommitInfo struct {
		Repo         string `json:"repo"`
		TargetBranch string `json:"target_branch"`
		PR           *int32 `json:"pr"`
		GitCommit    string `json:"git_commit"`
	} `json:"commit_info"`
	DeployLink string `json:"deploy_link"`
	WebuiLink  string `json:"webui_link"`
	PRLink     string `json:"repo_pr_link"`
	Status     string `json:"status"`
	Created    string `json:"created"`
	LastScaled string `json:"last_scaled"`
}

// httpRunboat is the real client.
type httpRunboat struct {
	client *http.Client
}

func newHTTPRunboat() *httpRunboat {
	return &httpRunboat{
		// A timeout, not the default. http.Client's zero value waits forever, and
		// a Runboat that accepts the connection and never answers would wedge a
		// worker goroutine permanently — the reconcile would never return, so no
		// requeue, no error, no status update. Silent, and it looks like the
		// controller stopped caring.
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *httpRunboat) Builds(ctx context.Context, spec *doblurav1alpha1.RunboatLinkSpec, creds *RunboatCredentials) ([]RunboatRemoteBuild, error) {
	u := spec.APIPath() + "/builds"

	// Push the filter server-side when it is a single repo: Runboat can do the
	// selecting, and a shared instance may hold thousands of builds. With several
	// repos there is no repeated-parameter form for it, so the filtering happens
	// here instead.
	q := url.Values{}
	if f := spec.Filter; f != nil {
		if len(f.Repos) == 1 {
			q.Set("repo", strings.ToLower(f.Repos[0]))
		}
		if f.TargetBranch != "" {
			q.Set("target_branch", f.TargetBranch)
		}
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	body, err := h.do(ctx, http.MethodGet, u, creds)
	if err != nil {
		return nil, err
	}
	var out []RunboatRemoteBuild
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("runboat returned something that is not a build list: %w", err)
	}
	return out, nil
}

func (h *httpRunboat) Act(ctx context.Context, spec *doblurav1alpha1.RunboatLinkSpec, creds *RunboatCredentials, build string, action doblurav1alpha1.RunboatAction) error {
	verb, ok := runboatActionPath(action)
	if !ok {
		return fmt.Errorf("unsupported action %q", action)
	}
	// The build name goes into the path, so it is escaped. Runboat build names are
	// tame in practice, but a name is remote input and building a URL by
	// concatenation is how a path traversal gets in.
	u := spec.APIPath() + "/builds/" + url.PathEscape(build) + "/" + verb
	_, err := h.do(ctx, http.MethodPost, u, creds)
	return err
}

func (h *httpRunboat) do(ctx context.Context, method, u string, creds *RunboatCredentials) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if creds != nil {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// The URL can carry no credential — they go in a header — but it can carry
		// an internal hostname, and this string ends up in a status message that
		// anyone with read access sees. It is Runboat's own address, which is not
		// a secret, so it stays: an error naming no host is unactionable.
		return nil, fmt.Errorf("cannot reach runboat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read. Runboat's log endpoints return whole build logs, and while
	// Doblura does not call those, an unbounded ReadAll against a misconfigured URL
	// is how a controller gets OOM-killed by a response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("cannot read runboat's answer: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, runboatHTTPError(resp.StatusCode, body)
	}
	return body, nil
}

// runboatHTTPError turns a status code into something a person can act on.
//
// The bare codes are all ambiguous here, and each one has exactly one likely
// cause in this integration, so saying it is worth more than forwarding a number.
func runboatHTTPError(code int, body []byte) error {
	hint := ""
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint = ": check spec.auth.basicAuthSecret — runboat uses one shared admin credential"
	case http.StatusNotFound:
		hint = ": the build is gone, or spec.url does not point at a runboat (its API lives under /api/v1)"
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		hint = ": runboat itself is unhealthy; the mirror keeps the builds it already had"
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg != "" {
		msg = " " + msg
	}
	return fmt.Errorf("runboat answered %d%s:%s", code, hint, msg)
}

func runboatActionPath(a doblurav1alpha1.RunboatAction) (string, bool) {
	switch a {
	case doblurav1alpha1.RunboatStart:
		return "start", true
	case doblurav1alpha1.RunboatStop:
		return "stop", true
	case doblurav1alpha1.RunboatReset:
		return "reset", true
	}
	return "", false
}

// toMirror converts what Runboat sent into what goes in status.
func toMirror(b RunboatRemoteBuild) doblurav1alpha1.RunboatBuild {
	return doblurav1alpha1.RunboatBuild{
		Name:         b.Name,
		Repo:         b.CommitInfo.Repo,
		TargetBranch: b.CommitInfo.TargetBranch,
		PR:           b.CommitInfo.PR,
		Commit:       b.CommitInfo.GitCommit,
		// Passed through unmapped, including a value this build of Doblura has
		// never heard of. A Runboat that adds a state should show that state, not
		// an empty column.
		Status:     doblurav1alpha1.RunboatBuildStatus(b.Status),
		DeployLink: b.DeployLink,
		WebuiLink:  b.WebuiLink,
		PRLink:     b.PRLink,
		Created:    parseRunboatTime(b.Created),
		LastScaled: parseRunboatTime(b.LastScaled),
	}
}

// parseRunboatTime accepts what FastAPI actually emits.
//
// Runboat's model uses datetime.datetime, and whether those carry a timezone
// depends on how they were constructed — so the wire format is RFC 3339 with an
// offset sometimes, and without one other times. Parsing only RFC 3339 would
// drop every timestamp from a Runboat storing naive datetimes, leaving the mirror
// with no ages at all and no clue why.
//
// A naive value is read as UTC. That is an assumption, and the alternative is
// discarding the field; an age that could be off by a timezone still tells you
// whether a build is from today.
func parseRunboatTime(s string) *metav1.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return &metav1.Time{Time: t.UTC()}
		}
	}
	return nil
}
