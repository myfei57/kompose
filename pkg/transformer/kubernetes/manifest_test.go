/*
Copyright 2024 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubernetes

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/loader/compose"
	"github.com/kubernetes/kompose/pkg/manifest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	manifestEnvValue    = "super-secret-token-value-7c3a"
	manifestSecretValue = "db-secret-file-content-91e2"
)

func manifestKomposeObject(t *testing.T) kobject.KomposeObject {
	secretFile := filepath.Join(t.TempDir(), "dbsecret.txt")
	if err := os.WriteFile(secretFile, []byte(manifestSecretValue), 0o600); err != nil {
		t.Fatalf("unable to write secret file: %v", err)
	}
	return kobject.KomposeObject{
		Namespace: "trace-ns",
		Secrets: types.Secrets{
			"dbsecret": types.SecretConfig{
				Name: "dbsecret",
				File: secretFile,
			},
		},
		ServiceConfigs: map[string]kobject.ServiceConfig{
			"web": {
				Name:  "web",
				Image: "nginx:1.25",
				Environment: []kobject.EnvVar{
					{Name: "API_TOKEN", Value: manifestEnvValue},
					{Name: "LOG_LEVEL", Value: "info"},
				},
				Port: []kobject.Ports{{HostPort: 8080, ContainerPort: 80, Protocol: "TCP"}},
				// parseKomposeLabels in the compose loader moves this label
				// into ExposeService; emulate that here.
				ExposeService: "example.com",
				Labels: map[string]string{
					"kompose.service.expose": "example.com",
				},
			},
			"db": {
				Name:  "db",
				Image: "postgres:16",
				Volumes: []kobject.Volumes{{
					SvcName:   "db",
					Host:      "dbdata",
					Container: "/var/lib/postgresql/data",
					MountPath: "/var/lib/postgresql/data",
					PVCName:   "db-claim0",
				}},
				Secrets: []types.ServiceSecretConfig{{Source: "dbsecret"}},
			},
		},
	}
}

func findRecord(t *testing.T, m *manifest.Manifest, kind, name string) manifest.ObjectRecord {
	t.Helper()
	for _, record := range m.Objects {
		if record.Object.Kind == kind && record.Object.Name == name {
			return record
		}
	}
	t.Fatalf("manifest does not contain %s %q", kind, name)
	return manifest.ObjectRecord{}
}

func recordFieldPaths(record manifest.ObjectRecord) []string {
	var paths []string
	for _, source := range record.Sources {
		paths = append(paths, source.FieldPaths...)
	}
	return paths
}

func recordSourceTypes(record manifest.ObjectRecord) map[manifest.SourceType]bool {
	types := map[manifest.SourceType]bool{}
	for _, source := range record.Sources {
		types[source.Type] = true
	}
	return types
}

func TestBuildManifestProvenanceAndRedaction(t *testing.T) {
	opt := kobject.ConvertOptions{CreateD: true, Provider: "kubernetes", Replicas: 1}
	ko := manifestKomposeObject(t)
	tracker := manifest.NewTracker()
	k := &Kubernetes{Opt: opt, Provenance: tracker}

	objects, err := k.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}

	m := k.BuildManifest(objects, tracker, ko, opt, manifest.DocumentIndex{})
	if m.Kind != manifest.Kind {
		t.Fatalf("unexpected manifest kind %q", m.Kind)
	}
	if m.Pagination.Total != len(objects) {
		t.Fatalf("manifest records (%d) do not match objects (%d)", m.Pagination.Total, len(objects))
	}

	// Every object must be traceable and carry conversion stages.
	for _, record := range m.Objects {
		if len(record.Sources) == 0 {
			t.Errorf("object %s/%s has no sources", record.Object.Kind, record.Object.Name)
		}
		if len(record.Stages) < 3 {
			t.Errorf("object %s/%s has too few stages: %v", record.Object.Kind, record.Object.Name, record.Stages)
		}
		for _, source := range record.Sources {
			if source.Type == manifest.SourceUnknown {
				t.Errorf("object %s/%s has an unresolved source", record.Object.Kind, record.Object.Name)
			}
		}
	}

	data, err := m.Marshal(false)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(data)
	if strings.Contains(serialized, manifestEnvValue) {
		t.Errorf("manifest leaks environment variable value: %s", serialized)
	}
	if strings.Contains(serialized, manifestSecretValue) {
		t.Errorf("manifest leaks Secret file content: %s", serialized)
	}
	// Environment variable NAMES remain traceable, values do not.
	if !strings.Contains(serialized, "services.web.environment.API_TOKEN") {
		t.Errorf("manifest should trace the environment variable name API_TOKEN")
	}

	// Web Service traces ports only.
	serviceRecord := findRecord(t, m, "Service", "web")
	if paths := recordFieldPaths(serviceRecord); !containsString(paths, "services.web.ports") {
		t.Errorf("web Service should trace services.web.ports, got %v", paths)
	}

	// Web Deployment traces image/ports/environment names and redacts values.
	deploymentRecord := findRecord(t, m, "Deployment", "web")
	paths := recordFieldPaths(deploymentRecord)
	for _, want := range []string{"services.web.image", "services.web.ports", "services.web.environment", "services.web.labels.kompose.service.expose"} {
		if !containsString(paths, want) {
			t.Errorf("web Deployment missing field path %q in %v", want, paths)
		}
	}
	if !redactionPresent(deploymentRecord, "environment values") {
		t.Errorf("web Deployment should declare redacted environment values")
	}

	// Ingress traces the expose label.
	if _, ok := findKind(m, "Ingress"); ok {
		ingressRecord := findRecord(t, m, "Ingress", "web")
		if paths := recordFieldPaths(ingressRecord); !containsString(paths, "services.web.labels.kompose.service.expose") {
			t.Errorf("Ingress should trace the expose label, got %v", paths)
		}
	}

	// Secret traces the top level secrets key and the consuming service,
	// without any workload noise or secret content.
	secretRecord := findRecord(t, m, "Secret", "dbsecret")
	sourceTypes := recordSourceTypes(secretRecord)
	if !sourceTypes[manifest.SourceRoot] || !sourceTypes[manifest.SourceService] {
		t.Errorf("dbsecret Secret should have root and service sources, got %v", sourceTypes)
	}
	for _, source := range secretRecord.Sources {
		if source.Type == manifest.SourceService {
			if !containsString(source.FieldPaths, "services.db.secrets.dbsecret") {
				t.Errorf("dbsecret consumer should trace services.db.secrets.dbsecret, got %v", source.FieldPaths)
			}
			if containsString(source.FieldPaths, "services.db.image") {
				t.Errorf("Secret record must not carry workload field paths, got %v", source.FieldPaths)
			}
		}
		if !containsString(source.ValuesRedacted, "secret data") {
			t.Errorf("Secret source should declare redacted secret data")
		}
	}

	// PVC traces the service volume.
	pvcRecord := findRecord(t, m, "PersistentVolumeClaim", "db-claim0")
	if !containsString(recordFieldPaths(pvcRecord), "services.db.volumes") {
		t.Errorf("PVC should trace services.db.volumes, got %v", recordFieldPaths(pvcRecord))
	}

	// Namespace traces the --namespace option.
	namespaceRecord := findRecord(t, m, "Namespace", "trace-ns")
	if !recordSourceTypes(namespaceRecord)[manifest.SourceOption] {
		t.Errorf("Namespace should be traced to an option source, got %v", namespaceRecord.Sources)
	}
}

func TestManifestDoesNotChangeConversion(t *testing.T) {
	opt := kobject.ConvertOptions{CreateD: true, Provider: "kubernetes", Replicas: 1}
	ko := manifestKomposeObject(t)

	kPlain := &Kubernetes{Opt: opt}
	plain, err := kPlain.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform without tracker failed: %v", err)
	}

	tracker := manifest.NewTracker()
	kTracked := &Kubernetes{Opt: opt, Provenance: tracker}
	tracked, err := kTracked.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform with tracker failed: %v", err)
	}

	if len(plain) != len(tracked) {
		t.Fatalf("object count changed by provenance tracking: %d vs %d", len(plain), len(tracked))
	}
	for i := range plain {
		if !reflect.DeepEqual(plain[i], tracked[i]) {
			t.Errorf("object %d (%s) differs when provenance tracking is enabled", i, plain[i].GetObjectKind().GroupVersionKind())
		}
	}

	// Building the manifest must not mutate the converted objects either.
	m := kTracked.BuildManifest(tracked, tracker, ko, opt, manifest.DocumentIndex{})
	for i := range tracked {
		if !reflect.DeepEqual(plain[i], tracked[i]) {
			t.Errorf("object %d was mutated while building the manifest", i)
		}
	}
	if m.Pagination.Total != len(tracked) {
		t.Errorf("manifest must describe every converted object")
	}
}

func TestManifestGroupedServices(t *testing.T) {
	groupSvc := func(name string) kobject.ServiceConfig {
		return kobject.ServiceConfig{
			Name:   name,
			Image:  name + ":latest",
			Labels: map[string]string{"kompose.service.group": "tier"},
		}
	}
	ko := kobject.KomposeObject{
		ServiceConfigs: map[string]kobject.ServiceConfig{
			"api": groupSvc("api"),
			"web": func() kobject.ServiceConfig {
				s := groupSvc("web")
				s.Port = []kobject.Ports{{HostPort: 80, ContainerPort: 80, Protocol: "TCP"}}
				return s
			}(),
		},
	}
	opt := kobject.ConvertOptions{CreateD: true, Provider: "kubernetes", ServiceGroupMode: "label"}

	tracker := manifest.NewTracker()
	k := &Kubernetes{Opt: opt, Provenance: tracker}
	objects, err := k.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}
	m := k.BuildManifest(objects, tracker, ko, opt, manifest.DocumentIndex{})

	var groupWorkload *manifest.ObjectRecord
	for _, record := range m.Objects {
		if record.Object.Kind == "Deployment" {
			groupWorkload = &record
		}
	}
	if groupWorkload == nil {
		t.Fatal("no Deployment generated for the service group")
	}
	services := map[string]bool{}
	for _, source := range groupWorkload.Sources {
		if source.Type == manifest.SourceService {
			services[source.Name] = true
		}
	}
	if !services["api"] || !services["web"] {
		t.Fatalf("group workload must trace both member services, got %v", services)
	}
}

func TestManifestMissingSecretWarning(t *testing.T) {
	ko := kobject.KomposeObject{
		ServiceConfigs: map[string]kobject.ServiceConfig{
			"web": {
				Name:    "web",
				Image:   "nginx",
				Secrets: []types.ServiceSecretConfig{{Source: "ghost"}},
			},
		},
	}
	opt := kobject.ConvertOptions{CreateD: true, Provider: "kubernetes"}
	tracker := manifest.NewTracker()
	k := &Kubernetes{Opt: opt, Provenance: tracker}
	objects, err := k.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}
	m := k.BuildManifest(objects, tracker, ko, opt, manifest.DocumentIndex{})

	var warned bool
	for _, record := range m.Objects {
		for _, warning := range record.Warnings {
			if strings.Contains(warning, "ghost") {
				warned = true
			}
		}
	}
	if !warned {
		t.Fatalf("expected a warning about the undeclared ghost secret, got %+v", m.Objects)
	}
}

func TestManifestUnknownObjectWarning(t *testing.T) {
	ko := kobject.KomposeObject{
		ServiceConfigs: map[string]kobject.ServiceConfig{
			"web": {Name: "web", Image: "nginx"},
		},
	}
	opt := kobject.ConvertOptions{CreateD: true, Provider: "kubernetes"}
	k := &Kubernetes{Opt: opt}
	objects, err := k.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}
	unknown := &unstructured.Unstructured{}
	unknown.SetKind("Widget")
	unknown.SetAPIVersion("example.com/v1")
	unknown.SetName("mystery")
	objects = append(objects, unknown)

	m := k.BuildManifest(objects, nil, ko, opt, manifest.DocumentIndex{})
	record := findRecord(t, m, "Widget", "mystery")
	if len(record.Warnings) == 0 {
		t.Fatalf("expected a missing-source warning, got %+v", record)
	}
	if !recordSourceTypes(record)[manifest.SourceUnknown] {
		t.Fatalf("expected an unknown source marker, got %+v", record.Sources)
	}
	if len(m.Warnings) == 0 {
		t.Fatalf("missing-source warning must also be collected on the manifest")
	}
}

// TestManifestEndToEndWithDocuments loads real Compose files, including an
// override file that declares the same service twice, and verifies the
// manifest records every declaring document without touching values.
func TestManifestEndToEndWithDocuments(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	override := filepath.Join(dir, "override.yaml")
	if err := os.WriteFile(base, []byte(`
services:
  web:
    image: nginx:1.25
    environment:
      SHARED_TOKEN: shared-token-value-2d91
    ports:
      - "8080:80"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte(`
services:
  web:
    labels:
      kompose.service.type: nodeport
  db:
    image: postgres:16
`), 0o644); err != nil {
		t.Fatal(err)
	}

	files := []string{base, override}
	l := new(compose.Compose)
	ko, err := l.LoadFile(files, nil, true)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	ko.Namespace = ""

	opt := kobject.ConvertOptions{
		CreateD:    true,
		Provider:   "kubernetes",
		InputFiles: files,
		Replicas:   1,
	}
	tracker := manifest.NewTracker()
	k := &Kubernetes{Opt: opt, Provenance: tracker}
	objects, err := k.Transform(ko, opt)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}

	docs := manifest.ScanDocuments(files)
	m := k.BuildManifest(objects, tracker, ko, opt, docs)

	data, err := m.Marshal(false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "shared-token-value-2d91") {
		t.Fatalf("manifest leaks the environment value from the compose file")
	}

	webDeployment := findRecord(t, m, "Deployment", "web")
	var documentFiles []string
	for _, source := range webDeployment.Sources {
		if source.Type == manifest.SourceService && source.Name == "web" {
			for _, ref := range source.Documents {
				documentFiles = append(documentFiles, ref.File)
			}
		}
	}
	if len(documentFiles) != 2 {
		t.Fatalf("web is declared in both compose files, expected 2 document refs, got %v", documentFiles)
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func redactionPresent(record manifest.ObjectRecord, category string) bool {
	for _, source := range record.Sources {
		if containsString(source.ValuesRedacted, category) {
			return true
		}
	}
	return false
}

func findKind(m *manifest.Manifest, kind string) (manifest.ObjectRecord, bool) {
	for _, record := range m.Objects {
		if record.Object.Kind == kind {
			return record, true
		}
	}
	return manifest.ObjectRecord{}, false
}
