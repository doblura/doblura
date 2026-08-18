// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Asking a forge what is open.
//
// Two providers, one shape. Deliberately small: this asks for open pull requests
// and for branch names, and nothing else. A fuller client would be a dependency
// with its own release cadence and its own CVEs, in a process that already holds
// a token.
//
// The token is sent in the header and never in the URL. A URL ends up in
// proxy logs, in error messages and in the manager's own logs when a request
// fails; a header does not.

// forgeRef is one thing worth an environment.
type forgeRef struct {
	Kind   string // PullRequest | Branch
	Ref    string // the branch to build
	Number int32
	Title  string
	URL    string
	Labels []string
}

// listRefs asks the forge.
func listRefs(
	ctx context.Context,
	repo doblurav1alpha1.ReviewRepository,
	watch doblurav1alpha1.ReviewWatch,
	token string,
) ([]forgeRef, error) {
	owner, name, err := repoPath(repo.URL)
	if err != nil {
		return nil, err
	}

	var out []forgeRef
	if watch.PullRequests {
		prs, err := listPullRequests(ctx, repo, owner, name, token)
		if err != nil {
			return nil, err
		}
		out = append(out, prs...)
	}
	if len(watch.Branches) > 0 {
		branches, err := listBranches(ctx, repo, owner, name, token)
		if err != nil {
			return nil, err
		}
		for _, b := range branches {
			if matchesAny(b, watch.Branches) {
				out = append(out, forgeRef{Kind: "Branch", Ref: b})
			}
		}
	}
	return out, nil
}

// repoPath pulls owner and repository out of a clone URL.
//
// Both forms, because people paste whichever their forge showed them:
// https://github.com/acme/hms.git and git@github.com:acme/hms.git.
func repoPath(raw string) (owner, name string, err error) {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	if i := strings.Index(s, "@"); i >= 0 && !strings.Contains(s, "://") {
		s = s[i+1:]
		s = strings.Replace(s, ":", "/", 1)
	} else if u, e := url.Parse(s); e == nil && u.Host != "" {
		s = u.Host + u.Path
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("cannot tell the owner and repository from %q", raw)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

func apiBase(repo doblurav1alpha1.ReviewRepository) string {
	if repo.APIBase != "" {
		return strings.TrimSuffix(repo.APIBase, "/")
	}
	if repo.Provider == doblurav1alpha1.ForgeGitLab {
		return "https://gitlab.com/api/v4"
	}
	return "https://api.github.com"
}

func listPullRequests(
	ctx context.Context,
	repo doblurav1alpha1.ReviewRepository,
	owner, name, token string,
) ([]forgeRef, error) {
	if repo.Provider == doblurav1alpha1.ForgeGitLab {
		var got []struct {
			IID          int32    `json:"iid"`
			Title        string   `json:"title"`
			WebURL       string   `json:"web_url"`
			SourceBranch string   `json:"source_branch"`
			Labels       []string `json:"labels"`
		}
		u := fmt.Sprintf("%s/projects/%s/merge_requests?state=opened&per_page=100",
			apiBase(repo), url.PathEscape(owner+"/"+name))
		if err := getJSON(ctx, u, token, repo.Provider, &got); err != nil {
			return nil, err
		}
		out := make([]forgeRef, 0, len(got))
		for _, m := range got {
			out = append(out, forgeRef{
				Kind: "PullRequest", Ref: m.SourceBranch, Number: m.IID,
				Title: m.Title, URL: m.WebURL, Labels: m.Labels,
			})
		}
		return out, nil
	}

	var got []struct {
		Number  int32  `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100", apiBase(repo), owner, name)
	if err := getJSON(ctx, u, token, repo.Provider, &got); err != nil {
		return nil, err
	}
	out := make([]forgeRef, 0, len(got))
	for _, p := range got {
		r := forgeRef{
			Kind: "PullRequest", Ref: p.Head.Ref, Number: p.Number,
			Title: p.Title, URL: p.HTMLURL,
		}
		for _, l := range p.Labels {
			r.Labels = append(r.Labels, l.Name)
		}
		out = append(out, r)
	}
	return out, nil
}

func listBranches(
	ctx context.Context,
	repo doblurav1alpha1.ReviewRepository,
	owner, name, token string,
) ([]string, error) {
	if repo.Provider == doblurav1alpha1.ForgeGitLab {
		var got []struct {
			Name string `json:"name"`
		}
		u := fmt.Sprintf("%s/projects/%s/repository/branches?per_page=100",
			apiBase(repo), url.PathEscape(owner+"/"+name))
		if err := getJSON(ctx, u, token, repo.Provider, &got); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(got))
		for _, b := range got {
			out = append(out, b.Name)
		}
		return out, nil
	}

	var got []struct {
		Name string `json:"name"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/branches?per_page=100", apiBase(repo), owner, name)
	if err := getJSON(ctx, u, token, repo.Provider, &got); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(got))
	for _, b := range got {
		out = append(out, b.Name)
	}
	return out, nil
}

func getJSON(ctx context.Context, u, token string, p doblurav1alpha1.ForgeProvider, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		// Header, never the URL: a URL with a token in it ends up in proxy logs,
		// in error messages and in this manager's own logs the moment a request
		// fails.
		if p == doblurav1alpha1.ForgeGitLab {
			req.Header.Set("PRIVATE-TOKEN", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		}
	}

	client := &http.Client{Timeout: 20 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode/100 != 2 {
		// The status and the forge's own words, truncated. A 404 from GitHub for
		// a private repository with a token that cannot see it is indistinguishable
		// from a repository that does not exist, and saying so beats guessing.
		var b strings.Builder
		_, _ = fmt.Fprintf(&b, "%s said %s", p, res.Status)
		if res.StatusCode == http.StatusNotFound {
			b.WriteString(" — either the repository does not exist, or the token cannot see it")
		}
		if rem := res.Header.Get("X-RateLimit-Remaining"); rem == "0" {
			b.WriteString("; the rate limit is exhausted")
		}
		return fmt.Errorf("%s", b.String())
	}
	return json.NewDecoder(res.Body).Decode(into)
}

// matchesAny is glob matching over branch names.
func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}
