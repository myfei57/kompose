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

// Package manifest produces a traceability manifest describing how each
// generated Kubernetes/OpenShift object relates back to the input Compose
// configuration.
//
// The manifest is strictly additive: it is computed from side-channel
// provenance events recorded during conversion and never mutates, adds or
// removes any converted object. By design it only carries structural
// information (source service, input field paths, conversion stages). It
// never stores environment variable values, env_file contents, ConfigMap
// data or Secret data.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// APIVersion is the version of the manifest schema.
	APIVersion = "kompose.io/v1alpha1"
	// Kind is the type of a full conversion manifest.
	Kind = "ConversionManifest"
	// PageKind is the type of a single manifest page.
	PageKind = "ConversionManifestPage"

	// DefaultPageSize is the number of object records contained in one page
	// when the caller does not specify a size.
	DefaultPageSize = 100

	// Conversion stage names. They describe the pipeline position at which
	// an object was observed.
	StageLoad                = "load"
	StageTransformInitialize = "transform.initialize"
	StageTransformPopulate   = "transform.populate"
	StageAggregate           = "aggregate"
	StageOutput              = "output"
)

// SourceType describes where a source reference points to.
type SourceType string

const (
	// SourceService points at an entry under Compose "services:".
	SourceService SourceType = "service"
	// SourceRoot points at a top level Compose key, for example "secrets:".
	SourceRoot SourceType = "root"
	// SourceOption points at a command line option / conversion option.
	SourceOption SourceType = "option"
	// SourceDefault points at a value synthesized by kompose defaults.
	SourceDefault SourceType = "default"
	// SourceUnknown marks an object for which no source could be resolved.
	// Records carrying this source also carry a warning.
	SourceUnknown SourceType = "unknown"
)

// DocumentRef identifies a single YAML/JSON document inside an input file.
// Compose allows several files (and several YAML documents per stream); the
// same service name may therefore be declared more than once.
type DocumentRef struct {
	// File is the input file as given on the command line.
	File string `json:"file" yaml:"file"`
	// Document is the zero based index of the document inside the YAML
	// stream of File. It is 0 for ordinary single-document files.
	Document int `json:"document" yaml:"document"`
}

// Source is a single provenance reference for a generated object.
type Source struct {
	// Type is the kind of source.
	Type SourceType `json:"type" yaml:"type"`
	// Name is the service name (for Type=service), the top level key
	// (for Type=root) or the option name (for Type=option).
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Item further qualifies a root level source, for example the secret
	// name under the top level "secrets:" key.
	Item string `json:"item,omitempty" yaml:"item,omitempty"`
	// Documents lists every input document declaring the source. A service
	// defined in several Compose files yields several entries.
	Documents []DocumentRef `json:"documents,omitempty" yaml:"documents,omitempty"`
	// FieldPaths lists the originating Compose field paths, for example
	// "services.web.ports". Values are never included.
	FieldPaths []string `json:"fieldPaths,omitempty" yaml:"fieldPaths,omitempty"`
	// ValuesRedacted lists the categories of values that were intentionally
	// not recorded (for example environment variable values or Secret data).
	ValuesRedacted []string `json:"valuesRedacted,omitempty" yaml:"valuesRedacted,omitempty"`
}

// ObjectRef identifies one generated object.
type ObjectRef struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Name       string `json:"name" yaml:"name"`
	Namespace  string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// ObjectRecord is the manifest entry of a single generated object.
type ObjectRecord struct {
	// Object identifies the generated resource.
	Object ObjectRef `json:"object" yaml:"object"`
	// Sources lists every input source contributing to the object.
	Sources []Source `json:"sources" yaml:"sources"`
	// Stages lists the conversion stages the object went through, in order.
	Stages []string `json:"stages,omitempty" yaml:"stages,omitempty"`
	// Warnings carries provenance specific warnings (for example a missing
	// source).
	Warnings []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}

// Pagination describes how the object records are paginated.
type Pagination struct {
	PageSize int `json:"pageSize" yaml:"pageSize"`
	Pages    int `json:"pages" yaml:"pages"`
	Total    int `json:"total" yaml:"total"`
}

// Manifest is the traceability manifest produced for one conversion.
type Manifest struct {
	APIVersion     string     `json:"apiVersion" yaml:"apiVersion"`
	Kind           string     `json:"kind" yaml:"kind"`
	Provider       string     `json:"provider" yaml:"provider"`
	InputDocuments []string   `json:"inputDocuments,omitempty" yaml:"inputDocuments,omitempty"`
	GeneratedAt    string     `json:"generatedAt" yaml:"generatedAt"`
	Pagination     Pagination `json:"pagination" yaml:"pagination"`
	// Warnings carries conversion-wide provenance warnings.
	Warnings []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
	// Objects holds one record per generated object, in output order.
	Objects []ObjectRecord `json:"objects" yaml:"objects"`
}

// ManifestPage is a single page of object records.
type ManifestPage struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion"`
	Kind       string         `json:"kind" yaml:"kind"`
	Page       int            `json:"page" yaml:"page"`
	PageSize   int            `json:"pageSize" yaml:"pageSize"`
	Pages      int            `json:"pages" yaml:"pages"`
	Total      int            `json:"total" yaml:"total"`
	Objects    []ObjectRecord `json:"objects" yaml:"objects"`
}

// New builds a manifest from the finalized object records.
func New(provider string, inputDocuments []string, records []ObjectRecord, warnings []string, pageSize int) *Manifest {
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	m := &Manifest{
		APIVersion:     APIVersion,
		Kind:           Kind,
		Provider:       provider,
		InputDocuments: append([]string(nil), inputDocuments...),
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Warnings:       append([]string(nil), warnings...),
		Objects:        records,
	}
	m.Pagination = Pagination{
		PageSize: pageSize,
		Pages:    m.TotalPages(pageSize),
		Total:    len(records),
	}
	return m
}

// TotalPages returns the number of pages for the given page size. It always
// returns at least 1, even for an empty manifest.
func (m *Manifest) TotalPages(pageSize int) int {
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	pages := len(m.Objects) / pageSize
	if len(m.Objects)%pageSize != 0 || pages == 0 {
		pages++
	}
	return pages
}

// Page returns a 1-indexed page of object records. It returns an error when
// the page size is not positive or the page number is out of range, so that
// callers reading large manifests page by page cannot silently miss records.
func (m *Manifest) Page(page, pageSize int) (ManifestPage, error) {
	if pageSize <= 0 {
		return ManifestPage{}, fmt.Errorf("page size must be greater than 0")
	}
	totalPages := m.TotalPages(pageSize)
	if page < 1 || page > totalPages {
		return ManifestPage{}, fmt.Errorf("page %d out of range, manifest has %d page(s) of size %d", page, totalPages, pageSize)
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > len(m.Objects) {
		end = len(m.Objects)
	}
	return ManifestPage{
		APIVersion: APIVersion,
		Kind:       PageKind,
		Page:       page,
		PageSize:   pageSize,
		Pages:      totalPages,
		Total:      len(m.Objects),
		Objects:    append([]ObjectRecord(nil), m.Objects[start:end]...),
	}, nil
}

// Event is one provenance observation exposed to manifest builders.
type Event struct {
	SourceType SourceType
	Name       string
	Item       string
	Stage      string
}

// provenanceEvent is one observation made during conversion.
type provenanceEvent struct {
	key        string
	ref        ObjectRef
	sourceType SourceType
	name       string
	item       string
	stage      string
}

// Tracker collects provenance events while a conversion runs. A nil
// *Tracker is safe to use and every method becomes a no-op, which allows the
// converter to record events without changing its behavior when manifest
// generation is disabled.
type Tracker struct {
	mu     sync.Mutex
	events []provenanceEvent
}

// NewTracker returns an empty provenance tracker.
func NewTracker() *Tracker {
	return &Tracker{}
}

// Note records that the given objects were produced for service name at the
// given conversion stage.
func (t *Tracker) Note(name, stage string, objects ...runtime.Object) {
	t.record(SourceService, name, "", stage, objects)
}

// NoteRoot records that the given objects originate from a top level Compose
// key (for example "secrets:").
func (t *Tracker) NoteRoot(stage, item string, objects ...runtime.Object) {
	t.record(SourceRoot, "secrets", item, stage, objects)
}

// NoteOption records that the given objects originate from a conversion
// option (command line flag).
func (t *Tracker) NoteOption(stage, option string, objects ...runtime.Object) {
	t.record(SourceOption, option, "", stage, objects)
}

func (t *Tracker) record(sourceType SourceType, name, item, stage string, objects []runtime.Object) {
	if t == nil || len(objects) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, obj := range objects {
		if obj == nil {
			continue
		}
		ref, ok := RefOf(obj)
		if !ok {
			continue
		}
		t.events = append(t.events, provenanceEvent{
			key:        Key(ref),
			ref:        ref,
			sourceType: sourceType,
			name:       name,
			item:       item,
			stage:      stage,
		})
	}
}

// EventsFor returns the recorded events for the given object key, in order.
func (t *Tracker) EventsFor(key string) []Event {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Event
	for _, ev := range t.events {
		if ev.key == key {
			out = append(out, Event{
				SourceType: ev.sourceType,
				Name:       ev.name,
				Item:       ev.item,
				Stage:      ev.stage,
			})
		}
	}
	return out
}

// EventsForObject returns the recorded events for the given object. Unlike
// EventsFor, it matches by apiVersion/kind/name and tolerates a namespace
// that was assigned to the object after the event was recorded (the
// converter assigns namespaces at the end of the pipeline).
func (t *Tracker) EventsForObject(ref ObjectRef) []Event {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Event
	for _, ev := range t.events {
		if ev.ref.APIVersion != ref.APIVersion || ev.ref.Kind != ref.Kind || ev.ref.Name != ref.Name {
			continue
		}
		if ev.ref.Namespace != "" && ev.ref.Namespace != ref.Namespace {
			continue
		}
		out = append(out, Event{
			SourceType: ev.sourceType,
			Name:       ev.name,
			Item:       ev.item,
			Stage:      ev.stage,
		})
	}
	return out
}

// Key returns the stable identity key of an object reference.
func Key(ref ObjectRef) string {
	return ref.APIVersion + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// RefOf extracts an ObjectRef from a runtime object.
func RefOf(obj runtime.Object) (ObjectRef, bool) {
	if obj == nil {
		return ObjectRef{}, false
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if us, ok := obj.(*unstructured.Unstructured); ok {
		return ObjectRef{
			APIVersion: us.GetAPIVersion(),
			Kind:       us.GetKind(),
			Name:       us.GetName(),
			Namespace:  us.GetNamespace(),
		}, true
	}
	meta, ok := obj.(metav1.Object)
	if !ok {
		return ObjectRef{}, false
	}
	apiVersion := gvk.GroupVersion().String()
	if gvk.Group == "" {
		apiVersion = gvk.Version
	}
	return ObjectRef{
		APIVersion: apiVersion,
		Kind:       gvk.Kind,
		Name:       meta.GetName(),
		Namespace:  meta.GetNamespace(),
	}, true
}

// UniqueStrings returns the input values without duplicates, preserving the
// first-seen order.
func UniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// MergeSources merges several Source slices, deduplicating them by
// (Type, Name, Item) and unioning their documents and field paths.
func MergeSources(in ...Source) []Source {
	type identity struct {
		t    SourceType
		name string
		item string
	}
	order := []identity{}
	merged := map[identity]*Source{}
	for _, s := range in {
		id := identity{s.Type, s.Name, s.Item}
		if existing, ok := merged[id]; ok {
			existing.Documents = append(existing.Documents, s.Documents...)
			existing.FieldPaths = append(existing.FieldPaths, s.FieldPaths...)
			existing.ValuesRedacted = append(existing.ValuesRedacted, s.ValuesRedacted...)
			continue
		}
		copy := s
		merged[id] = &copy
		order = append(order, id)
	}
	out := make([]Source, 0, len(order))
	for _, id := range order {
		s := merged[id]
		s.Documents = uniqueDocuments(s.Documents)
		s.FieldPaths = UniqueStrings(s.FieldPaths)
		s.ValuesRedacted = UniqueStrings(s.ValuesRedacted)
		out = append(out, *s)
	}
	return out
}

func uniqueDocuments(refs []DocumentRef) []DocumentRef {
	seen := map[DocumentRef]struct{}{}
	var out []DocumentRef
	for _, ref := range refs {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Document < out[j].Document
	})
	return out
}

// Marshal serializes the manifest as YAML (asJSON == false) or JSON.
func (m *Manifest) Marshal(asJSON bool) ([]byte, error) {
	if asJSON {
		return json.MarshalIndent(m, "", "  ")
	}
	return yamlMarshal(m)
}

// MarshalPage serializes a manifest page as YAML or JSON.
func (p *ManifestPage) Marshal(asJSON bool) ([]byte, error) {
	if asJSON {
		return json.MarshalIndent(p, "", "  ")
	}
	return yamlMarshal(p)
}

func yamlMarshal(in interface{}) ([]byte, error) {
	// yaml.v3 emits map keys sorted and uses 2 spaces indentation by
	// default, matching the rest of the generated artifacts.
	return yaml.Marshal(in)
}

// ReadFile parses a manifest (or manifest page) from a YAML or JSON file,
// depending on its extension.
func ReadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unable to read manifest %q: %w", path, err)
	}
	return Parse(data, filepath.Ext(path) == ".json")
}

// Parse decodes a manifest from YAML or JSON.
func Parse(data []byte, asJSON bool) (*Manifest, error) {
	m := &Manifest{}
	if asJSON {
		if err := json.Unmarshal(data, m); err != nil {
			return nil, fmt.Errorf("unable to parse manifest: %w", err)
		}
		return m, nil
	}
	if err := yaml.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("unable to parse manifest: %w", err)
	}
	return m, nil
}

// ReadPageFile parses a single manifest page from a YAML or JSON file.
func ReadPageFile(path string) (*ManifestPage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unable to read manifest page %q: %w", path, err)
	}
	p := &ManifestPage{}
	if filepath.Ext(path) == ".json" {
		err = json.Unmarshal(data, p)
	} else {
		err = yaml.Unmarshal(data, p)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to parse manifest page: %w", err)
	}
	return p, nil
}

// Save writes the manifest into dir as manifest.yaml (or manifest.json) and,
// when more than one page is required, additionally writes one file per page
// under dir/manifest-pages. It returns the path of the main manifest and the
// paths of any page files.
func Save(m *Manifest, dir string, asJSON bool, pageSize int) (string, []string, error) {
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("unable to create manifest directory %q: %w", dir, err)
	}
	ext := "yaml"
	if asJSON {
		ext = "json"
	}
	mainPath := filepath.Join(dir, "manifest."+ext)
	data, err := m.Marshal(asJSON)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(mainPath, append(data, '\n'), 0o644); err != nil {
		return "", nil, fmt.Errorf("unable to write manifest %q: %w", mainPath, err)
	}

	var pagePaths []string
	totalPages := m.TotalPages(pageSize)
	if totalPages > 1 {
		pageDir := filepath.Join(dir, "manifest-pages")
		if err := os.MkdirAll(pageDir, 0o755); err != nil {
			return "", nil, fmt.Errorf("unable to create manifest page directory %q: %w", pageDir, err)
		}
		for page := 1; page <= totalPages; page++ {
			p, err := m.Page(page, pageSize)
			if err != nil {
				return "", nil, err
			}
			pageData, err := p.Marshal(asJSON)
			if err != nil {
				return "", nil, err
			}
			pagePath := filepath.Join(pageDir, fmt.Sprintf("manifest-page-%04d.%s", page, ext))
			if err := os.WriteFile(pagePath, append(pageData, '\n'), 0o644); err != nil {
				return "", nil, fmt.Errorf("unable to write manifest page %q: %w", pagePath, err)
			}
			pagePaths = append(pagePaths, pagePath)
		}
	}
	return mainPath, pagePaths, nil
}
