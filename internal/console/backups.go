// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Backups, and putting one back.
//
// The page shows the copies and, beside each, the one action that matters. It
// makes the consequence loud rather than hiding it behind a confirm dialog
// nobody reads: the panel names the environment being replaced, and the form
// carries the acknowledgement the API demands — which names that environment
// too, so a page showing one customer cannot submit a restore into another.

type backupView struct {
	Backup     *doblurav1alpha1.OdooBackup
	State      string
	Word       string
	Copies     []doblurav1alpha1.BackupCopy
	History    []doblurav1alpha1.OdooRestore
	CanRestore bool
	// Environments this copy could go into, so restoring production's data into
	// staging is a choice on the page rather than a YAML file somebody writes.
	Environments []string
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	var b doblurav1alpha1.OdooBackup
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
		s.fail(w, id, err)
		return
	}

	view := backupView{Backup: &b, Copies: b.Status.Copies}
	view.State, view.Word = backupState(&b)

	// Newest first: the copy somebody wants after a mistake is almost always the
	// most recent one from before it.
	sort.Slice(view.Copies, func(i, j int) bool {
		return view.Copies[i].TakenAt.After(view.Copies[j].TakenAt.Time)
	})

	var envs doblurav1alpha1.OdooEnvironmentList
	if err := c.List(r.Context(), &envs, client.InNamespace(ns)); err == nil {
		for i := range envs.Items {
			view.Environments = append(view.Environments, envs.Items[i].Name)
		}
		sort.Strings(view.Environments)
	}

	var restores doblurav1alpha1.OdooRestoreList
	if err := c.List(r.Context(), &restores, client.InNamespace(ns)); err == nil {
		for i := range restores.Items {
			if restores.Items[i].Spec.Backup == b.Name {
				view.History = append(view.History, restores.Items[i])
			}
		}
		sort.Slice(view.History, func(i, j int) bool {
			a, z := view.History[i].Status.StartedAt, view.History[j].Status.StartedAt
			switch {
			case a == nil:
				return false
			case z == nil:
				return true
			}
			return a.After(z.Time)
		})
	}

	perms, err := s.allowed(r.Context(), id, Verb{"create", "odoorestores", ns, ""})
	if err == nil {
		view.CanRestore = perms[Verb{"create", "odoorestores", ns, ""}.key()]
	}

	s.renderFor(w, r, "backup.html", page{Title: b.Name, Identity: id, Data: view})
}

func backupState(b *doblurav1alpha1.OdooBackup) (state, word string) {
	if b.Spec.Suspend {
		return "asleep", "Paused"
	}
	if b.Status.LastSuccess == nil {
		if b.Status.LastRun != nil {
			return "down", "Never succeeded"
		}
		return "building", "Waiting for its first run"
	}
	for _, c := range b.Status.Conditions {
		if c.Type == "Backing" && c.Status != "True" {
			return "degraded", "Something is wrong"
		}
	}
	return "up", "Keeping copies"
}

// handleRestore creates the restore object. The API decides whether it may.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	copyName := strings.TrimSpace(r.FormValue("copy"))
	into := strings.TrimSpace(r.FormValue("into"))
	neutralize := r.FormValue("neutralize") == "on"

	// The name carries the copy and the target, so two restores of different
	// copies do not collide and the object list reads as a history.
	obj := &doblurav1alpha1.OdooRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restoreName(name, copyName, into),
			Namespace: ns,
		},
		Spec: doblurav1alpha1.OdooRestoreSpec{
			Backup: name, Copy: copyName, Into: into,
			// Built from the target rather than taken from the form: a hidden
			// field carrying the acknowledgement would be a hidden field an
			// attacker controls, and the whole point of the literal is that it
			// matches the environment actually being replaced.
			Acknowledgement: doblurav1alpha1.RestoreAckFor(into),
			Neutralize:      &neutralize,
		},
	}
	if err := c.Create(r.Context(), obj); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/b/"+ns+"/"+name, http.StatusSeeOther)
}

// restoreName is stable per (backup, copy, target) and a legal DNS label.
func restoreName(backup, copyName, into string) string {
	base := "restore-" + into + "-" + shortLabel(copyName)
	if len(base) > 60 {
		base = base[:60]
	}
	return strings.Trim(base, "-")
}

// shortLabel makes a timestamp into something a DNS label accepts.
func shortLabel(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	return strings.Trim(string(out), "-")
}
