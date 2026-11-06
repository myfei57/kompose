/*
Copyright 2026 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubernetes

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2beta2 "k8s.io/api/autoscaling/v2beta2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// --- object builders -------------------------------------------------------

func deploy(name string, mutate func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if mutate != nil {
		mutate(d)
	}
	return d
}

func deployWithPodSpec(name string, ps corev1.PodSpec) *appsv1.Deployment {
	return deploy(name, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec = ps
	})
}

func ds(name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{Kind: "DaemonSet", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func svc(name string) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{Kind: "Service", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func cm(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func pvc(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{Kind: "PersistentVolumeClaim", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func pvcVolume(claim string) corev1.Volume {
	return corev1.Volume{
		Name: "vol-" + claim,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		},
	}
}

func cmVolume(name string) corev1.Volume {
	return corev1.Volume{
		Name: "vol-" + name,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		},
	}
}

func envFromCM(name string) corev1.EnvFromSource {
	return corev1.EnvFromSource{
		ConfigMapRef: &corev1.ConfigMapEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
		},
	}
}

func envCMKeyRef(name, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: key,
		ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Key:                  key,
			},
		},
	}
}

func findPlan(t *testing.T, plans []RenamePlan, kind, canonical string) RenamePlan {
	t.Helper()
	for _, p := range plans {
		if p.Kind == kind && p.Canonical == canonical {
			return p
		}
	}
	t.Fatalf("rename plan for %s/%s not found", kind, canonical)
	return RenamePlan{}
}

// allUpdatePaths returns every accounted-for inbound reference path of a
// plan, both the winner references that stay put and the loser references
// that must be rewritten.
func allUpdatePaths(plan RenamePlan) []string {
	var paths []string
	for _, u := range plan.WinnerReferenceUpdates {
		paths = append(paths, u.FieldPath)
	}
	for _, step := range plan.Steps {
		for _, u := range step.ReferenceUpdates {
			paths = append(paths, u.FieldPath)
		}
	}
	return paths
}

func stepUpdatePaths(plan RenamePlan) []string {
	var paths []string
	for _, step := range plan.Steps {
		for _, u := range step.ReferenceUpdates {
			paths = append(paths, u.FieldPath)
		}
	}
	return paths
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// --- tests -----------------------------------------------------------------

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"web":        "web",
		"My_Service": "my-service",
		"A_B_C":      "a-b-c",
		"":           "",
		"foo.bar":    "foo.bar",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveNameConflicts_NilInput(t *testing.T) {
	report := ResolveNameConflicts(nil)
	if report == nil {
		t.Fatal("expected non-nil report for nil input")
	}
	if report.HasBlockingIssues() {
		t.Errorf("empty input must not report blocking issues: %+v", report)
	}
	if len(report.Indexed) != 0 || len(report.UnindexedKinds) != 0 {
		t.Errorf("empty input must produce empty index, got %+v", report)
	}
}

func TestResolveNameConflicts_HardConflict(t *testing.T) {
	report := ResolveNameConflicts([]runtime.Object{
		deploy("web", nil), deploy("web", nil),
	})

	if len(report.HardConflicts) != 1 {
		t.Fatalf("want 1 hard conflict, got %d", len(report.HardConflicts))
	}
	if len(report.NormalizationConflicts) != 0 {
		t.Fatalf("want 0 normalization conflicts, got %d", len(report.NormalizationConflicts))
	}
	c := report.HardConflicts[0]
	if c.Kind != "Deployment" || c.Canonical != "web" || len(c.Members) != 2 {
		t.Fatalf("unexpected conflict: %+v", c)
	}
	if c.Members[0].Occurrence != 1 || c.Members[1].Occurrence != 2 {
		t.Errorf("occurrences must be 1 and 2: %+v", c.Members)
	}

	if len(report.RenamePlans) != 1 {
		t.Fatalf("want 1 rename plan, got %d", len(report.RenamePlans))
	}
	plan := report.RenamePlans[0]
	if plan.Winner.Name != "web" || plan.Winner.Occurrence != 1 {
		t.Errorf("unexpected winner: %+v", plan.Winner)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("want 1 rename step, got %d", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Target.Occurrence != 2 || step.NewName != "web-deployment" {
		t.Errorf("unexpected rename step: %+v", step)
	}
}

func TestResolveNameConflicts_HardConflictMultipleLosers(t *testing.T) {
	report := ResolveNameConflicts([]runtime.Object{
		deploy("dup", nil), deploy("dup", nil), deploy("dup", nil),
	})
	plan := report.RenamePlans[0]
	if len(plan.Steps) != 2 {
		t.Fatalf("want 2 rename steps, got %d", len(plan.Steps))
	}
	if plan.Steps[0].Target.Occurrence != 2 || plan.Steps[0].NewName != "dup-deployment" {
		t.Errorf("unexpected first step: %+v", plan.Steps[0])
	}
	// The first generated candidate is now reserved, so the numeric fallback
	// must be chosen deterministically.
	if plan.Steps[1].Target.Occurrence != 3 || plan.Steps[1].NewName != "dup-2" {
		t.Errorf("unexpected second step: %+v", plan.Steps[1])
	}
}

func TestResolveNameConflicts_CandidateFallback(t *testing.T) {
	report := ResolveNameConflicts([]runtime.Object{
		deploy("web", nil), deploy("web", nil),
		deploy("web-deployment", nil), deploy("web-2", nil),
	})
	plan := findPlan(t, report.RenamePlans, "Deployment", "web")
	if got := plan.Steps[0].NewName; got != "web-3" {
		t.Errorf("candidate must skip reserved names, got %q", got)
	}
}

func TestResolveNameConflicts_CandidateTruncation(t *testing.T) {
	long := strings.Repeat("a", 70)
	report := ResolveNameConflicts([]runtime.Object{
		deploy(long, nil), deploy(long, nil),
	})
	plan := report.RenamePlans[0]
	newName := plan.Steps[0].NewName
	if len(newName) > 63 {
		t.Errorf("candidate exceeds DNS label limit: len=%d (%q)", len(newName), newName)
	}
	if !strings.HasSuffix(newName, "-deployment") {
		t.Errorf("candidate should keep readable suffix: %q", newName)
	}
	if strings.Contains(newName, "--") {
		t.Errorf("candidate must not leave a dangling dash after truncation: %q", newName)
	}
}

func TestResolveNameConflicts_NormalizationCollision(t *testing.T) {
	// "Shared" and "shared" are distinct declared names that collapse onto
	// the same canonical key. Each is referenced by a distinct workload.
	app1 := deployWithPodSpec("app1", corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "app1", EnvFrom: []corev1.EnvFromSource{envFromCM("Shared")}},
		},
	})
	app2 := deployWithPodSpec("app2", corev1.PodSpec{
		Volumes: []corev1.Volume{cmVolume("shared")},
	})
	report := ResolveNameConflicts([]runtime.Object{app1, app2, cm("Shared"), cm("shared")})

	if len(report.NormalizationConflicts) != 1 || len(report.HardConflicts) != 0 {
		t.Fatalf("want exactly 1 normalization conflict, got hard=%d norm=%d",
			len(report.HardConflicts), len(report.NormalizationConflicts))
	}
	// The uppercase raw name is invalid DNS and reported independently.
	var invalidRaws []string
	for _, inv := range report.InvalidNames {
		invalidRaws = append(invalidRaws, inv.Ref.Name)
		if len(inv.Reasons) == 0 {
			t.Errorf("invalid name entry must carry reasons: %+v", inv)
		}
	}
	if got := strings.Join(invalidRaws, ","); got != "Shared" {
		t.Errorf("only the uppercase raw name should be invalid, got %q", got)
	}

	plan := findPlan(t, report.RenamePlans, "ConfigMap", "shared")
	// Equal inbound counts -> smallest raw name ("Shared" < "shared") wins.
	if plan.Winner.Name != "Shared" {
		t.Errorf("unexpected winner: %q", plan.Winner.Name)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Target.Name != "shared" || step.NewName != "shared-configmap" {
		t.Errorf("unexpected target/name: %+v", step)
	}
	if len(step.ReferenceUpdates) != 1 {
		t.Fatalf("only the referrer spelling the loser raw name is rewritten, got %d", len(step.ReferenceUpdates))
	}
	u := step.ReferenceUpdates[0]
	if u.Referrer.Name != "app2" ||
		u.FieldPath != "spec.template.spec.volumes[0].configMap.name" ||
		u.OldName != "shared" || u.NewName != "shared-configmap" {
		t.Errorf("unexpected reference update: %+v", u)
	}
	// The winner keeps the reference spelling its own raw variant.
	if len(plan.WinnerReferenceUpdates) != 1 {
		t.Fatalf("winner keeps exactly one reference, got %d", len(plan.WinnerReferenceUpdates))
	}
	w := plan.WinnerReferenceUpdates[0]
	if w.Referrer.Name != "app1" || w.OldName != "Shared" || w.NewName != "Shared" ||
		w.FieldPath != "spec.template.spec.containers[0].envFrom[0].configMapRef.name" {
		t.Errorf("unexpected winner reference: %+v", w)
	}
}

func TestResolveNameConflicts_WinnerKeepsMostReferenced(t *testing.T) {
	// Two configmaps named "cfg"; referenced by three vs one workloads. The
	// one with more inbound references keeps its name regardless of input
	// ordering.
	referenced := func(suffix string) *appsv1.Deployment {
		return deployWithPodSpec("w-"+suffix, corev1.PodSpec{
			Volumes: []corev1.Volume{cmVolume("cfg")},
		})
	}
	objects := []runtime.Object{
		cm("cfg"), cm("cfg"),
		referenced("1"), referenced("2"), referenced("3"), // three referrers ...
		referenced("4"),
	}
	report := ResolveNameConflicts(objects)
	plan := findPlan(t, report.RenamePlans, "ConfigMap", "cfg")

	// All four referrers spell the same raw name, so they belong to the
	// winner: the loser step must carry zero updates while the winner keeps
	// the name.
	if plan.Winner.Occurrence != 1 {
		t.Errorf("identical raw names tie on reference counts; smallest occurrence wins, got %d", plan.Winner.Occurrence)
	}
	if len(plan.WinnerReferenceUpdates) != 4 {
		t.Errorf("winner must keep all four inbound references, got %d", len(plan.WinnerReferenceUpdates))
	}
	var loserUpdates int
	for _, s := range plan.Steps {
		loserUpdates += len(s.ReferenceUpdates)
	}
	if loserUpdates != 0 {
		t.Errorf("exact-duplicate loser cannot own references, got %d updates", loserUpdates)
	}
}

func TestResolveNameConflicts_EmptyNames(t *testing.T) {
	report := ResolveNameConflicts([]runtime.Object{svc(""), deploy("", nil), svc(""), deploy("ok", nil)})

	if len(report.EmptyNames) != 3 {
		t.Fatalf("want 3 empty name entries, got %d", len(report.EmptyNames))
	}
	if report.EmptyNames[0].Kind != "Deployment" || report.EmptyNames[0].Category != CategoryWorkload {
		t.Errorf("empty entries sorted by kind first: %+v", report.EmptyNames[0])
	}
	for _, e := range report.Indexed {
		if e.Name == "" {
			t.Errorf("empty names must not be indexed: %+v", e)
		}
	}
	if !report.HasBlockingIssues() {
		t.Errorf("empty names are blocking issues")
	}
}

func TestResolveNameConflicts_CrossCategorySharing(t *testing.T) {
	report := ResolveNameConflicts([]runtime.Object{deploy("cache", nil), svc("cache")})

	if report.HasBlockingIssues() {
		t.Errorf("same name across kinds is legal in Kubernetes: %+v", report)
	}
	if len(report.CrossCategoryNames) != 1 {
		t.Fatalf("want 1 cross-category share, got %d", len(report.CrossCategoryNames))
	}
	share := report.CrossCategoryNames[0]
	if share.Canonical != "cache" || len(share.Members) != 2 {
		t.Fatalf("unexpected share: %+v", share)
	}
	if share.Members[0].Category != CategoryWorkload || share.Members[1].Category != CategoryService {
		t.Errorf("members ordered by category rank: %+v", share.Members)
	}
	if len(report.CategoryConflicts) != 0 || len(report.RenamePlans) != 0 {
		t.Errorf("cross-category shares must not create conflicts or plans")
	}
}

func TestResolveNameConflicts_SameCategoryDifferentKind(t *testing.T) {
	report := ResolveNameConflicts([]runtime.Object{deploy("x", nil), ds("x")})

	if len(report.CategoryConflicts) != 1 {
		t.Fatalf("want 1 category conflict, got %d", len(report.CategoryConflicts))
	}
	c := report.CategoryConflicts[0]
	if c.Category != CategoryWorkload || c.Kind != "" {
		t.Errorf("category reports span kinds, Kind must be empty: %+v", c)
	}
	if len(report.HardConflicts) != 0 || len(report.NormalizationConflicts) != 0 {
		t.Errorf("different kinds must not be same-kind conflicts")
	}
}

func TestResolveNameConflicts_NamespaceIsolation(t *testing.T) {
	d1 := deploy("same", nil)
	d1.Namespace = "one"
	d2 := deploy("same", nil)
	d2.Namespace = "two"

	report := ResolveNameConflicts([]runtime.Object{d1, d2})
	if len(report.HardConflicts) != 0 || len(report.NormalizationConflicts) != 0 ||
		len(report.CategoryConflicts) != 0 || len(report.CrossCategoryNames) != 0 {
		t.Errorf("identical names in different namespaces never conflict: %+v", report)
	}
	if len(report.Indexed) != 2 {
		t.Errorf("both occurrences must be indexed, got %d", len(report.Indexed))
	}
}

func TestResolveNameConflicts_ReferencesAndPlans(t *testing.T) {
	// StatefulSet referencing a Service, two PVCs (one duplicated) and
	// ConfigMaps through volume/envFrom/env valueFrom paths.
	stateful := &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{Kind: "StatefulSet", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "db"},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "db-headless",
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{pvcVolume("db-data"), cmVolume("db-env")},
				Containers: []corev1.Container{{
					Name:    "db",
					EnvFrom: []corev1.EnvFromSource{envFromCM("db-env")},
					Env:     []corev1.EnvVar{envCMKeyRef("db-cfg", "KEY")},
				}},
			}},
		},
	}

	// CronJob referencing a duplicated configmap.
	cron := &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{Kind: "CronJob", APIVersion: "batch/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "cj"},
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{cmVolume("cj-cfg")},
				}},
			}},
		},
	}

	// Ingress referencing a duplicated Service.
	pathType := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{Kind: "Ingress", APIVersion: "networking.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "web-ing"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     "/",
					PathType: &pathType,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}

	// HPA (v2beta2, the version kompose emits) referencing a duplicated
	// Deployment.
	hpa := &autoscalingv2beta2.HorizontalPodAutoscaler{
		TypeMeta:   metav1.TypeMeta{Kind: "HorizontalPodAutoscaler", APIVersion: "autoscaling/v2beta2"},
		ObjectMeta: metav1.ObjectMeta{Name: "api-hpa"},
		Spec: autoscalingv2beta2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2beta2.CrossVersionObjectReference{
				Kind: "Deployment", Name: "api",
			},
		},
	}

	// A pod with an emptyDir volume called "db-data": pod-local volume names
	// are not namespace references and must not appear in any plan.
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "db-data",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
	}

	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "s"},
	}

	objects := []runtime.Object{
		stateful, svc("db-headless"),
		pvc("db-data"), pvc("db-data"),
		cm("db-env"), cm("db-env"), cm("db-cfg"),
		cron, cm("cj-cfg"), cm("cj-cfg"),
		ingress, svc("web"), svc("web"),
		hpa, deploy("api", nil), deploy("api", nil),
		pod, secret,
	}
	report := ResolveNameConflicts(objects)

	// Plans are globally ordered by canonical name.
	var canonicals []string
	for _, p := range report.RenamePlans {
		canonicals = append(canonicals, p.Kind+"/"+p.Canonical)
	}
	wantOrder := []string{
		"Deployment/api",
		"ConfigMap/cj-cfg",
		"PersistentVolumeClaim/db-data",
		"ConfigMap/db-env",
		"Service/web",
	}
	if diff := cmp.Diff(wantOrder, canonicals); diff != "" {
		t.Errorf("plans not deterministically ordered (-want +got):\n%s", diff)
	}

	// All conflicts below are hard (exact duplicate) conflicts: references
	// must stay attached to the winner, and loser steps must carry zero
	// rewrites.

	// PVC claim reference.
	pvcPlan := findPlan(t, report.RenamePlans, "PersistentVolumeClaim", "db-data")
	pvcPaths := allUpdatePaths(pvcPlan)
	if !containsPath(pvcPaths, "spec.template.spec.volumes[0].persistentVolumeClaim.claimName") {
		t.Errorf("missing PVC claim reference in %v", pvcPaths)
	}
	for _, p := range pvcPaths {
		if strings.Contains(p, "EmptyDir") {
			t.Errorf("emptyDir pod volumes are not references: %s", p)
		}
	}
	if pvcPlan.Steps[0].NewName != "db-data-pvc" {
		t.Errorf("unexpected PVC candidate: %q", pvcPlan.Steps[0].NewName)
	}
	if len(stepUpdatePaths(pvcPlan)) != 0 {
		t.Errorf("hard-conflict loser must not move references: %v", stepUpdatePaths(pvcPlan))
	}
	for _, u := range pvcPlan.WinnerReferenceUpdates {
		if u.OldName != u.NewName || u.NewName != "db-data" {
			t.Errorf("winner references stay unchanged, got %q -> %q", u.OldName, u.NewName)
		}
	}

	// ConfigMap referenced from both a volume and envFrom; both references
	// stay with the winner and are sorted by field path.
	envPlan := findPlan(t, report.RenamePlans, "ConfigMap", "db-env")
	envPaths := allUpdatePaths(envPlan)
	wantPaths := []string{
		"spec.template.spec.containers[0].envFrom[0].configMapRef.name",
		"spec.template.spec.volumes[1].configMap.name",
	}
	if diff := cmp.Diff(wantPaths, envPaths); diff != "" {
		t.Errorf("configmap reference paths mismatch (-want +got):\n%s", diff)
	}
	if len(stepUpdatePaths(envPlan)) != 0 {
		t.Errorf("hard-conflict loser must not move references: %v", stepUpdatePaths(envPlan))
	}

	// CronJob pod-spec nesting.
	cjPlan := findPlan(t, report.RenamePlans, "ConfigMap", "cj-cfg")
	if !containsPath(allUpdatePaths(cjPlan),
		"spec.jobTemplate.spec.template.spec.volumes[0].configMap.name") {
		t.Errorf("missing cronjob configmap reference: %v", allUpdatePaths(cjPlan))
	}

	// Ingress -> Service, referrer is unindexed but still reported.
	webPlan := findPlan(t, report.RenamePlans, "Service", "web")
	if len(webPlan.WinnerReferenceUpdates) != 1 {
		t.Errorf("want 1 ingress winner reference, got %d", len(webPlan.WinnerReferenceUpdates))
	}
	if len(stepUpdatePaths(webPlan)) != 0 {
		t.Errorf("hard-conflict loser must not move references: %v", stepUpdatePaths(webPlan))
	}
	ing := webPlan.WinnerReferenceUpdates[0]
	if ing.Referrer.Kind != "Ingress" || ing.Referrer.Category != "" ||
		ing.FieldPath != "spec.rules[0].http.paths[0].backend.service.name" {
		t.Errorf("unexpected ingress update: %+v", ing)
	}

	// HPA -> workload.
	apiPlan := findPlan(t, report.RenamePlans, "Deployment", "api")
	hpaUpdate := apiPlan.WinnerReferenceUpdates
	if len(hpaUpdate) != 1 || hpaUpdate[0].Referrer.Kind != "HorizontalPodAutoscaler" ||
		hpaUpdate[0].FieldPath != "spec.scaleTargetRef.name" {
		t.Errorf("unexpected hpa update: %+v", hpaUpdate)
	}
	if len(stepUpdatePaths(apiPlan)) != 0 {
		t.Errorf("hard-conflict loser must not move references: %v", stepUpdatePaths(apiPlan))
	}

	// Selectors/labels are never treated as name references.
	for _, plan := range report.RenamePlans {
		for _, p := range allUpdatePaths(plan) {
			if strings.Contains(strings.ToLower(p), "selector") {
				t.Errorf("selectors match labels, not names, must not be updated: %s", p)
			}
		}
	}

	// Referencer-only kinds are unindexed but accounted for transparently.
	for _, kind := range []string{"Ingress", "HorizontalPodAutoscaler", "Secret"} {
		if !containsPath(report.UnindexedKinds, kind) {
			t.Errorf("UnindexedKinds must contain %q: %v", kind, report.UnindexedKinds)
		}
	}
	if containsPath(report.UnindexedKinds, "Service") || containsPath(report.UnindexedKinds, "") {
		t.Errorf("indexed kinds and objects without GVK must not be unindexed: %v", report.UnindexedKinds)
	}
}

func TestResolveNameConflicts_DeterministicUnderReordering(t *testing.T) {
	build := func() []runtime.Object {
		app1 := deployWithPodSpec("app1", corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app1", EnvFrom: []corev1.EnvFromSource{envFromCM("Shared")}},
			},
		})
		app2 := deployWithPodSpec("app2", corev1.PodSpec{
			Volumes: []corev1.Volume{cmVolume("shared")},
		})
		emptySvc := svc("")
		cmUpper := cm("Shared")
		cmLower := cm("shared")
		d1 := deploy("cache", nil)
		cacheSvc := svc("cache")
		dup1 := deploy("dup", nil)
		dup2 := deploy("dup", nil)
		dup3 := deploy("dup", nil)
		nsPVC1 := pvc("data")
		nsPVC2 := pvc("data")
		return []runtime.Object{
			app1, app2, emptySvc, cmUpper, cmLower,
			d1, cacheSvc, dup1, dup2, dup3, nsPVC1, nsPVC2,
		}
	}

	first := ResolveNameConflicts(build())

	// Feed the same objects in two different orders.
	objs := build()
	rotated := append(append([]runtime.Object{}, objs[5:]...), objs[:5]...)
	reversed := make([]runtime.Object, len(objs))
	for i := range objs {
		reversed[i] = objs[len(objs)-1-i]
	}

	if diff := cmp.Diff(first, ResolveNameConflicts(rotated)); diff != "" {
		t.Errorf("rotated input changed the report (-first +rotated):\n%s", diff)
	}
	if diff := cmp.Diff(first, ResolveNameConflicts(reversed)); diff != "" {
		t.Errorf("reversed input changed the report (-first +reversed):\n%s", diff)
	}
}
