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

package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sampleRecords(n int) []ObjectRecord {
	records := make([]ObjectRecord, 0, n)
	for i := 0; i < n; i++ {
		records = append(records, ObjectRecord{
			Object: ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-" + string(rune('a'+i%26)) + "-" + itoa(i)},
			Sources: []Source{{
				Type:       SourceService,
				Name:       "web",
				FieldPaths: []string{"services.web.configs"},
			}},
		})
	}
	return records
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestPagination(t *testing.T) {
	records := sampleRecords(5)
	m := New("kubernetes", []string{"compose.yaml"}, records, nil, 2)

	if m.Pagination.Total != 5 || m.Pagination.Pages != 3 || m.Pagination.PageSize != 2 {
		t.Fatalf("unexpected pagination: %+v", m.Pagination)
	}

	page1, err := m.Page(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Objects) != 2 || page1.Total != 5 || page1.Pages != 3 {
		t.Fatalf("unexpected first page: %d objects, total=%d pages=%d", len(page1.Objects), page1.Total, page1.Pages)
	}

	page3, err := m.Page(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3.Objects) != 1 {
		t.Fatalf("expected 1 object on the last page, got %d", len(page3.Objects))
	}

	// Every object must be reachable page by page and exactly once.
	seen := map[string]bool{}
	for page := 1; page <= m.TotalPages(2); page++ {
		p, err := m.Page(page, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range p.Objects {
			key := Key(record.Object)
			if seen[key] {
				t.Fatalf("object %s returned twice while reading pages", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct objects across pages, got %d", len(seen))
	}
}

func TestPaginationErrors(t *testing.T) {
	m := New("kubernetes", nil, sampleRecords(2), nil, DefaultPageSize)

	if _, err := m.Page(0, 2); err == nil {
		t.Error("expected an error for page 0")
	}
	if _, err := m.Page(4, 2); err == nil {
		t.Error("expected an error for a page out of range")
	}
	if _, err := m.Page(1, 0); err == nil {
		t.Error("expected an error for a zero page size")
	}
}

func TestEmptyManifestHasOnePage(t *testing.T) {
	m := New("kubernetes", nil, nil, nil, DefaultPageSize)
	if m.TotalPages(DefaultPageSize) != 1 {
		t.Fatalf("expected 1 page for an empty manifest, got %d", m.TotalPages(DefaultPageSize))
	}
	page, err := m.Page(1, DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 0 {
		t.Fatalf("expected an empty first page, got %d objects", len(page.Objects))
	}
}

func TestSaveAndReadWithPages(t *testing.T) {
	dir := t.TempDir()
	m := New("kubernetes", []string{"compose.yaml"}, sampleRecords(5), []string{"a warning"}, 2)

	mainPath, pagePaths, err := Save(m, dir, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(mainPath) != "manifest.yaml" {
		t.Fatalf("unexpected manifest path %q", mainPath)
	}
	if len(pagePaths) != 3 {
		t.Fatalf("expected 3 page files, got %d: %v", len(pagePaths), pagePaths)
	}

	read, err := ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if read.Kind != Kind || read.Pagination.Total != 5 {
		t.Fatalf("unexpected manifest read back: %+v", read)
	}
	if len(read.Warnings) != 1 || read.Warnings[0] != "a warning" {
		t.Fatalf("warnings were not persisted: %v", read.Warnings)
	}

	page2, err := ReadPageFile(pagePaths[1])
	if err != nil {
		t.Fatal(err)
	}
	if page2.Kind != PageKind || page2.Page != 2 || page2.Pages != 3 || len(page2.Objects) != 2 {
		t.Fatalf("unexpected second page: %+v", page2)
	}
}

func TestSaveJSON(t *testing.T) {
	dir := t.TempDir()
	m := New("kubernetes", nil, sampleRecords(1), nil, DefaultPageSize)
	mainPath, pagePaths, err := Save(m, dir, true, DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(mainPath) != ".json" {
		t.Fatalf("expected a json manifest, got %q", mainPath)
	}
	if len(pagePaths) != 0 {
		t.Fatalf("expected no page files for a single page, got %v", pagePaths)
	}
	data, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"kind": "ConversionManifest"`) {
		t.Fatalf("manifest does not look like JSON: %s", data)
	}
}

func TestTrackerNilSafe(t *testing.T) {
	var tracker *Tracker
	// Must not panic when manifest generation is disabled.
	tracker.Note("web", StageTransformInitialize, &corev1.ConfigMap{})
	tracker.NoteRoot(StageTransformInitialize, "s", &corev1.Secret{})
	if events := tracker.EventsFor("anything"); events != nil {
		t.Fatalf("nil tracker should report no events, got %v", events)
	}
}

func TestTrackerRecordsEvents(t *testing.T) {
	tracker := NewTracker()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "env"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}}

	tracker.Note("web", StageTransformInitialize, cm)
	tracker.Note("web", StageTransformPopulate, cm)
	tracker.NoteRoot(StageTransformInitialize, "s", secret)

	ref, _ := RefOf(cm)
	events := tracker.EventsFor(Key(ref))
	if len(events) != 2 {
		t.Fatalf("expected 2 events for the configmap, got %d", len(events))
	}
	if events[0].Name != "web" || events[0].SourceType != SourceService {
		t.Fatalf("unexpected first event: %+v", events[0])
	}
}

func TestMergeSources(t *testing.T) {
	sources := MergeSources(
		Source{Type: SourceService, Name: "web", FieldPaths: []string{"services.web.image"}, Documents: []DocumentRef{{File: "a.yaml", Document: 0}}},
		Source{Type: SourceService, Name: "web", FieldPaths: []string{"services.web.ports"}, Documents: []DocumentRef{{File: "b.yaml", Document: 0}, {File: "a.yaml", Document: 0}}},
		Source{Type: SourceRoot, Name: "secrets", Item: "s", FieldPaths: []string{"secrets.s.file"}},
	)
	if len(sources) != 2 {
		t.Fatalf("expected 2 merged sources, got %d", len(sources))
	}
	if sources[0].Type != SourceService || len(sources[0].FieldPaths) != 2 || len(sources[0].Documents) != 2 {
		t.Fatalf("service sources were not merged correctly: %+v", sources[0])
	}
	if sources[0].Documents[0].File != "a.yaml" {
		t.Fatalf("documents should be sorted by file, got %+v", sources[0].Documents)
	}
}
