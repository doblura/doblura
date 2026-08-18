// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The image catalogue, edited.
//
// Adding an entry is ordinary. Changing which one is DEFAULT is not, and the
// difference is the whole reason this is not one form with a checkbox: moving the
// default between builds of the same Odoo version is a rollout, and moving it
// across versions rewrites the database in place with no way back except a
// restore from before it started.
//
// The console does not decide which of those it is. It asks for the change and
// lets the admission webhook answer, because that webhook can read the rehearsal
// that would authorise it and this page cannot. What the console does do is
// SHOW which of the two you are about to do, before you click.

func (s *Server) handleAddImage(w http.ResponseWriter, r *http.Request, id Identity) {
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
	var t doblurav1alpha1.OdooTenant
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}

	entry := doblurav1alpha1.ImageCatalogueEntry{
		Name:        strings.TrimSpace(r.FormValue("imageName")),
		Image:       strings.TrimSpace(r.FormValue("image")),
		OdooVersion: strings.TrimSpace(r.FormValue("odooVersion")),
		Notes:       strings.TrimSpace(r.FormValue("notes")),
		Flavor:      doblurav1alpha1.ImageFlavor(r.FormValue("flavor")),
	}
	// The first entry is the default, because a catalogue of one with nothing
	// marked is a catalogue that answers no question.
	if len(t.Spec.Images) == 0 {
		entry.Default = true
	}

	replaced := false
	for i := range t.Spec.Images {
		if t.Spec.Images[i].Name == entry.Name {
			// Keep whatever it was: adding an entry is not how somebody promotes
			// one, and quietly moving the default here would skip the check that
			// exists to stop a major upgrade happening by accident.
			entry.Default = t.Spec.Images[i].Default
			t.Spec.Images[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		t.Spec.Images = append(t.Spec.Images, entry)
	}

	if err := c.Update(r.Context(), &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/c/"+ns+"/"+name, http.StatusSeeOther)
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request, id Identity) {
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
	var t doblurav1alpha1.OdooTenant
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}

	want := r.FormValue("imageName")
	kept := t.Spec.Images[:0]
	for _, e := range t.Spec.Images {
		if e.Name != want {
			kept = append(kept, e)
		}
	}
	t.Spec.Images = kept

	if err := c.Update(r.Context(), &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/c/"+ns+"/"+name, http.StatusSeeOther)
}

// handlePromoteImage moves the default.
//
// One entry, and the acknowledgement travels with it when the form said so. The
// console does not judge whether this is a major upgrade — it sends the change
// and the webhook, which can read the rehearsal, decides.
func (s *Server) handlePromoteImage(w http.ResponseWriter, r *http.Request, id Identity) {
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
	var t doblurav1alpha1.OdooTenant
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}

	want := r.FormValue("imageName")
	found := false
	for i := range t.Spec.Images {
		t.Spec.Images[i].Default = t.Spec.Images[i].Name == want
		if t.Spec.Images[i].Name == want {
			found = true
		}
	}
	if !found {
		s.failBack(w, r, id, fmt.Errorf("%q is not in this customer's catalogue", want))
		return
	}

	if ref := strings.TrimSpace(r.FormValue("rehearsalRef")); ref != "" {
		t.Spec.MajorUpgrade = &doblurav1alpha1.MajorUpgrade{
			ToImage:         want,
			RehearsalRef:    ref,
			Acknowledgement: doblurav1alpha1.MajorUpgradeAck,
		}
	} else {
		// Cleared, so an authorisation left over from a previous upgrade cannot
		// wave the next one through. The API refuses it anyway; clearing it here
		// means the object does not carry a stale claim.
		t.Spec.MajorUpgrade = nil
	}

	if err := c.Update(r.Context(), &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/c/"+ns+"/"+name, http.StatusSeeOther)
}
