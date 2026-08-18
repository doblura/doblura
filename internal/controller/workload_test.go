package controller

import (
	"strings"
	"testing"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The whole point of the split, expressed as the one line that must be zero.
func TestWebTierDoesNotRunCronsWhenACronTierExists(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:    "img",
			Database: doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
		},
	}
	// No split: the historical behaviour, one deployment doing both.
	if got := envOdooConf(env); !strings.Contains(got, "max_cron_threads = 1") {
		t.Errorf("without a split the web tier should still run crons:\n%s", got)
	}
	// With a cron tier: crons happen in exactly one place, and it is not here.
	env.Spec.Workload = &doblurav1alpha1.WorkloadSplit{
		Cron: &doblurav1alpha1.CronTier{Replicas: 1, Threads: 2},
	}
	got := envOdooConf(env)
	if !strings.Contains(got, "max_cron_threads = 0") {
		t.Fatalf("with a cron tier the web tier MUST NOT run crons — they would run twice:\n%s", got)
	}
	// A cron tier scaled to zero is not a cron tier.
	env.Spec.Workload.Cron.Replicas = 0
	if got := envOdooConf(env); !strings.Contains(got, "max_cron_threads = 1") {
		t.Errorf("a cron tier at 0 replicas means nothing runs crons at all:\n%s", got)
	}
}
