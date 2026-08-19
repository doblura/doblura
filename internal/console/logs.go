// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Logs.
//
// The persona roles have granted pods/log since they were written — support gets
// it because "it does not work" needs an answer, and viewer deliberately does
// not, because logs are the one read that carries live customer data. The
// console then never showed them, so the permission existed and reached nothing.
//
// Read as the person, like everything else. Somebody without pods/log gets the
// API server's refusal rather than a page that pretends the logs are empty.
//
// Not streamed. Following a log needs a WebSocket or polling, and this console
// loads no JavaScript at all — a property worth keeping on a page holding a
// session that can act on a cluster. A page of the last few hundred lines with a
// reload button answers the question people actually arrive with, and the ones
// who want to follow output have kubectl.

// logTail is how much is fetched. Enough to see a traceback and its cause, small
// enough that a pod which has been logging for a week does not arrive as a
// twenty-megabyte page.
const logTail = 400

type logsView struct {
	Env        *doblurav1alpha1.OdooEnvironment
	Pods       []logPod
	Selected   string
	Container  string
	Containers []string
	Text       string
	Err        string
	// ExecCommand is the kubectl line for somebody who wants a real shell,
	// because this page is not one.
	ExecCommand string
}

type logPod struct {
	Name string
	Tier string
	// State is the pod's own word for what it is doing. "not ready" was shown
	// for everything, which is wrong for a phase pod that COMPLETED — it did not
	// fail to become ready, it finished.
	State     string
	Good      bool
	Restarts  int32
	Container string
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	var env doblurav1alpha1.OdooEnvironment
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &env); err != nil {
		s.fail(w, id, err)
		return
	}

	var pods corev1.PodList
	if err := c.List(r.Context(), &pods, client.InNamespace(ns),
		client.MatchingLabels{"doblura.dev/environment": name}); err != nil {
		s.fail(w, id, err)
		return
	}

	view := logsView{Env: &env}
	for i := range pods.Items {
		p := &pods.Items[i]
		lp := logPod{
			Name: p.Name,
			Tier: p.Labels["doblura.dev/tier"],
			// The first container is the one worth reading: the phase pods name
			// theirs after the phase, and the serving pods call it odoo.
			Container: firstContainerName(p),
		}
		if lp.Tier == "" {
			lp.Tier = p.Labels["app.kubernetes.io/component"]
		}
		for _, cs := range p.Status.ContainerStatuses {
			lp.Restarts += cs.RestartCount
		}
		lp.State, lp.Good = podWord(p)
		view.Pods = append(view.Pods, lp)
	}
	// Newest first: the pod somebody wants is almost always the one that just
	// restarted, and it is the one at the bottom of an alphabetical list.
	sort.Slice(view.Pods, func(i, j int) bool {
		return view.Pods[i].Name > view.Pods[j].Name
	})

	if len(view.Pods) == 0 {
		s.renderFor(w, r, "logs.html", page{
			Title: name + " · logs", Identity: id, Data: view,
		})
		return
	}

	view.Selected = r.URL.Query().Get("pod")
	if !knownPod(view.Pods, view.Selected) {
		view.Selected = view.Pods[0].Name
	}
	for i := range pods.Items {
		if pods.Items[i].Name != view.Selected {
			continue
		}
		for _, ctr := range pods.Items[i].Spec.Containers {
			view.Containers = append(view.Containers, ctr.Name)
		}
		for _, ctr := range pods.Items[i].Spec.InitContainers {
			view.Containers = append(view.Containers, ctr.Name)
		}
	}
	view.Container = r.URL.Query().Get("container")
	if !contains(view.Containers, view.Container) && len(view.Containers) > 0 {
		view.Container = view.Containers[0]
	}
	view.ExecCommand = fmt.Sprintf("kubectl -n %s exec -it %s -c %s -- /bin/bash",
		ns, view.Selected, view.Container)

	text, err := s.podLogs(r, id, ns, view.Selected, view.Container)
	if err != nil {
		view.Err = err.Error()
	} else {
		view.Text = text
	}

	s.renderFor(w, r, "logs.html", page{
		Title: name + " · logs", Identity: id, Data: view,
	})
}

// podLogs reads them AS THE PERSON.
//
// A typed clientset rather than the controller-runtime client, because logs are
// a subresource that returns a stream and the generic client does not model
// them. The REST config still carries the impersonation, so the authorisation is
// the same as everywhere else here.
func (s *Server) podLogs(r *http.Request, id Identity, ns, pod, container string) (string, error) {
	cfg, err := s.impersonatedConfig(id)
	if err != nil {
		return "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}

	tail := int64(logTail)
	req := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
	})
	stream, err := req.Stream(r.Context())
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	// Bounded read. A container writing a megabyte a second between the request
	// and the read would otherwise decide how much memory this console uses.
	var b strings.Builder
	sc := bufio.NewScanner(io.LimitReader(stream, 4<<20))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "", nil
	}
	return b.String(), nil
}

// podWord says what the pod is doing, in its own terms.
//
// A phase pod that Succeeded is finished, not unready; a serving pod that is
// Running but not ready is the interesting case and deserves its own word. Using
// the container readiness for both made a completed restore look like a failure.
func podWord(p *corev1.Pod) (word string, good bool) {
	switch p.Status.Phase {
	case corev1.PodSucceeded:
		return "finished", true
	case corev1.PodFailed:
		return "failed", false
	case corev1.PodPending:
		return "starting", false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			return "ready", true
		}
	}
	return "not ready yet", false
}

func firstContainerName(p *corev1.Pod) string {
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Name
	}
	return ""
}

func knownPod(pods []logPod, name string) bool {
	for _, p := range pods {
		if p.Name == name {
			return true
		}
	}
	return false
}

func contains(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
