/*
Copyright 2026 The Kubernetes Authors All rights reserved.

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
	"fmt"
	"sort"
	"strconv"
	"strings"

	deployapi "github.com/openshift/api/apps/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	autoscalingv2beta2 "k8s.io/api/autoscaling/v2beta2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// This file implements a deterministic, in-memory resolver for Kubernetes
// resource naming conflicts in a conversion result.
//
// It never writes manifests (YAML/JSON) and never contacts a cluster: all
// analysis is performed on the []runtime.Object produced by a transformer.
//
// Determinism contract: every slice in NameConflictReport is sorted by a total
// order over content-derived fields, so reordering the input objects never
// changes the report. Object identity is content based (kind + declared name +
// occurrence assigned after sorting), never slice position.

// maxDNSLabelLength is the DNS-1123 label limit applied to generated names.
const maxDNSLabelLength = 63

// NameCategory identifies one of the four name classes tracked in the unified
// namespace index. Kubernetes names are scoped per resource kind, but the four
// classes below let us distinguish same-kind collisions (invalid) from names
// shared across kinds or classes (legal, but reported separately).
type NameCategory string

const (
	// CategoryWorkload covers pod controllers: Deployment, DaemonSet,
	// StatefulSet, ReplicaSet, ReplicationController, Pod, Job and CronJob.
	CategoryWorkload NameCategory = "workload"
	// CategoryService covers the Kubernetes Service kind.
	CategoryService NameCategory = "service"
	// CategoryConfig covers ConfigMap.
	CategoryConfig NameCategory = "config"
	// CategoryVolume covers PersistentVolumeClaim.
	CategoryVolume NameCategory = "volume"
)

// nameCategoryByKind maps an indexed Kubernetes kind to its NameCategory.
// Kinds absent from this map (Secret, Ingress, NetworkPolicy, HPA, Namespace,
// ...) do not own a slot in the unified index, although some of them may still
// reference indexed resources.
var nameCategoryByKind = map[string]NameCategory{
	"Deployment":            CategoryWorkload,
	"DaemonSet":             CategoryWorkload,
	"StatefulSet":           CategoryWorkload,
	"ReplicaSet":            CategoryWorkload,
	"ReplicationController": CategoryWorkload,
	"Pod":                   CategoryWorkload,
	"Job":                   CategoryWorkload,
	"CronJob":               CategoryWorkload,

	"Service": CategoryService,

	"ConfigMap": CategoryConfig,

	"PersistentVolumeClaim": CategoryVolume,
}

// kindSlug is the deterministic, human readable suffix first attempted when a
// replacement name must be generated for a resource of the given kind.
var kindSlug = map[string]string{
	"Deployment":            "deployment",
	"DaemonSet":             "daemonset",
	"StatefulSet":           "statefulset",
	"ReplicaSet":            "rs",
	"ReplicationController": "rc",
	"Pod":                   "pod",
	"Job":                   "job",
	"CronJob":               "cronjob",
	"Service":               "service",
	"ConfigMap":             "configmap",
	"PersistentVolumeClaim": "pvc",
}

// categoryRank defines the total, request-independent tie-break order between
// categories (workload < service < config < volume).
func categoryRank(category NameCategory) int {
	switch category {
	case CategoryWorkload:
		return 0
	case CategoryService:
		return 1
	case CategoryConfig:
		return 2
	case CategoryVolume:
		return 3
	default:
		return 99
	}
}

// normalizeName is the single canonicalization rule used by the unified index.
// It mirrors the name normalization kompose itself applies when generating
// PersistentVolumeClaim/Secret names (strings.ToLower plus "_" -> "-"), which
// means two distinct declared names can collapse onto the same index key; such
// cases are reported as normalization conflicts, never silently merged.
func normalizeName(raw string) string {
	return strings.ToLower(strings.ReplaceAll(raw, "_", "-"))
}

// ObjectRef is a content-based, position-independent pointer to one occurrence
// of a converted resource.
type ObjectRef struct {
	// Namespace scopes the name (empty means the converted manifest's default
	// namespace). Same names in different namespaces never conflict.
	Namespace string
	// Category is the indexed class; empty for referrers that are not indexed
	// (e.g. an Ingress pointing at a Service).
	Category NameCategory
	// Kind is the Kubernetes kind, e.g. "Deployment".
	Kind string
	// Name is the declared (raw) metadata.name.
	Name string
	// Canonical is the normalized index key.
	Canonical string
	// Occurrence is a 1-based ordinal assigned after deterministic sorting;
	// it disambiguates otherwise identical declarations.
	Occurrence int
}

// EmptyNameEntry reports a resource of an indexed kind whose metadata.name is
// empty. Empty names cannot be indexed or renamed and are reported separately
// from normalization and collision issues.
type EmptyNameEntry struct {
	Namespace  string
	Category   NameCategory
	Kind       string
	Occurrence int
}

// InvalidNameEntry reports a non-empty declared name that is not a valid DNS
// subdomain (see RFC 1123), together with the API-server style validation
// reasons. Such names usually indicate that normalization would alter them.
type InvalidNameEntry struct {
	Ref     ObjectRef
	Reasons []string
}

// NameConflict is a set of resource occurrences sharing one canonical index
// key. Kind is set for same-kind conflicts and empty for category scoped
// reports; Members is always deterministically sorted.
type NameConflict struct {
	Namespace string
	Category  NameCategory
	// Kind is empty for cross-kind (category level) reports.
	Kind      string
	Canonical string
	Members   []ObjectRef
}

// CrossCategoryName reports a canonical name legally shared by resources of
// different categories (e.g. a Service and a Deployment both called "web").
// Kubernetes allows this because names are scoped per kind, but a unified
// namespace view must surface it distinctly from true conflicts.
type CrossCategoryName struct {
	Namespace string
	Canonical string
	Members   []ObjectRef
}

// ReferenceUpdate is one inbound reference that must be rewritten when a
// conflict loser is renamed. FieldPath is anchored at the referrer object root
// (e.g. "spec.template.spec.volumes[0].persistentVolumeClaim.claimName").
type ReferenceUpdate struct {
	Referrer  ObjectRef
	FieldPath string
	OldName   string
	NewName   string
}

// RenameStep renames one conflict loser and rewrites every reference that
// currently spells its declared name. Apply order within a step: the
// ReferenceUpdates must be applied first, and the resource definition renamed
// last, so that at no point two definitions carry the old name.
type RenameStep struct {
	Target           ObjectRef
	NewName          string
	ReferenceUpdates []ReferenceUpdate
}

// RenamePlan resolves one same-kind (hard or normalization) conflict: Winner
// keeps the name, every step in Steps renames one loser deterministically.
// WinnerReferenceUpdates lists the references spelling the winner's declared
// name: they keep pointing at the surviving resource (OldName == NewName) and
// are reported so every inbound reference is accounted for.
type RenamePlan struct {
	Namespace              string
	Category               NameCategory
	Kind                   string
	Canonical              string
	Winner                 ObjectRef
	WinnerReferenceUpdates []ReferenceUpdate
	Steps                  []RenameStep
}

// NameConflictReport is the deterministic result of indexing a conversion
// result. The issue classes are deliberately kept separate:
//
//   - EmptyNames: missing metadata.name (invalid, cannot be auto-resolved).
//   - InvalidNames: non-empty names failing DNS-1123 validation.
//   - HardConflicts: same namespace/kind and identical declared names
//     (duplicate definitions rejected by the API server).
//   - NormalizationConflicts: same namespace/kind, distinct declared names that
//     normalize to the same key (e.g. "My_CM" and "my-cm").
//   - CategoryConflicts: same category, different kinds, same canonical name
//     (legal in Kubernetes, but ambiguous in the unified index).
//   - CrossCategoryNames: same canonical name across categories (legal,
//     informational).
//   - RenamePlans: deterministic candidate names and reference update order
//     for HardConflicts and NormalizationConflicts.
type NameConflictReport struct {
	// Indexed lists every non-empty indexed name, deterministically sorted.
	Indexed []ObjectRef
	// EmptyNames lists indexed resources with an empty metadata.name.
	EmptyNames []EmptyNameEntry
	// InvalidNames lists non-empty declared names failing DNS-1123 validation.
	InvalidNames []InvalidNameEntry
	// HardConflicts groups exact duplicate declarations (same raw name).
	HardConflicts []NameConflict
	// NormalizationConflicts groups distinct raw names collapsing to one key.
	NormalizationConflicts []NameConflict
	// CategoryConflicts groups same-category, cross-kind shares of one key.
	CategoryConflicts []NameConflict
	// CrossCategoryNames lists keys shared across two or more categories.
	CrossCategoryNames []CrossCategoryName
	// RenamePlans holds the deterministic resolution plans.
	RenamePlans []RenamePlan
	// UnindexedKinds lists, sorted and de-duplicated, the kinds seen in the
	// input that do not own a slot in the unified index.
	UnindexedKinds []string
}

// HasBlockingIssues reports whether the conversion result contains a name that
// is empty, invalid, or involved in a same-kind conflict. Category and
// cross-category shares are legal in Kubernetes and are informational only.
func (r *NameConflictReport) HasBlockingIssues() bool {
	return len(r.EmptyNames) > 0 || len(r.InvalidNames) > 0 ||
		len(r.HardConflicts) > 0 || len(r.NormalizationConflicts) > 0
}

// nameOwner is the neutral internal representation of one declared name.
type nameOwner struct {
	namespace string
	category  NameCategory
	kind      string
	name      string
}

// ownerIdentity identifies one (namespace, kind, declared-name) triple.
type ownerIdentity struct {
	namespace string
	kind      string
	name      string
}

// nameReference is one inbound name reference found inside a converted object.
type nameReference struct {
	fromNamespace string
	fromKind      string
	fromName      string
	fieldPath     string
	toKind        string
	toName        string
}

// ResolveNameConflicts builds the unified namespace index from a conversion
// result and deterministically reports naming conflicts, safe rename
// candidates and the reference update order. It is a pure analysis step: it
// does not marshal YAML/JSON, does not touch the filesystem and does not
// contact a Kubernetes cluster.
func ResolveNameConflicts(objects []runtime.Object) *NameConflictReport {
	owners := make([]nameOwner, 0, len(objects))
	var refs []nameReference
	unindexed := make(map[string]struct{})

	for _, obj := range objects {
		if obj == nil {
			continue
		}
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		namespace, name := "", ""
		if meta, ok := obj.(metav1.Object); ok {
			namespace = meta.GetNamespace()
			name = meta.GetName()
		}
		if category, ok := nameCategoryByKind[kind]; ok {
			owners = append(owners, nameOwner{
				namespace: namespace,
				category:  category,
				kind:      kind,
				name:      name,
			})
		} else {
			unindexed[kind] = struct{}{}
		}
		refs = append(refs, extractNameReferences(obj, namespace, kind, name)...)
	}

	return analyzeNameOwners(owners, refs, sortedStringSet(unindexed))
}

// analyzeNameOwners is the deterministic core of the resolver; it is split
// from the runtime.Object adapter so the logic does not depend on object slice
// ordering or Kubernetes object construction.
func analyzeNameOwners(owners []nameOwner, refs []nameReference, unindexedKinds []string) *NameConflictReport {
	report := &NameConflictReport{UnindexedKinds: unindexedKinds}

	// Sort all declarations by a content-based total order before assigning
	// occurrence ordinals, so input ordering cannot leak into the report.
	sortedOwners := make([]nameOwner, len(owners))
	copy(sortedOwners, owners)
	sort.SliceStable(sortedOwners, func(i, j int) bool {
		return ownerLess(sortedOwners[i], sortedOwners[j])
	})

	occurrence := make(map[ownerIdentity]int)
	occurrenceCounters := make(map[ownerIdentity]int)
	emptyCounters := make(map[string]int)

	allRefs := make([]ObjectRef, 0, len(sortedOwners))
	reserved := make(map[string]map[string]struct{}) // namespace -> canonical names

	// sameKindGroup groups non-empty owners by (namespace, kind, canonical).
	type groupKey struct {
		namespace string
		kind      string
		canonical string
	}
	sameKindGroups := make(map[groupKey][]ObjectRef)
	// categoryGroup groups by (namespace, category, canonical).
	type categoryKey struct {
		namespace string
		category  NameCategory
		canonical string
	}
	categoryGroups := make(map[categoryKey][]ObjectRef)
	// crossGroup groups by (namespace, canonical).
	type crossKey struct {
		namespace string
		canonical string
	}
	crossGroups := make(map[crossKey][]ObjectRef)

	ensureReserved := func(namespace string) map[string]struct{} {
		if reserved[namespace] == nil {
			reserved[namespace] = make(map[string]struct{})
		}
		return reserved[namespace]
	}

	for _, o := range sortedOwners {
		if o.name == "" {
			emptyCounters[o.namespace+"\x00"+o.kind]++
			report.EmptyNames = append(report.EmptyNames, EmptyNameEntry{
				Namespace:  o.namespace,
				Category:   o.category,
				Kind:       o.kind,
				Occurrence: emptyCounters[o.namespace+"\x00"+o.kind],
			})
			continue
		}

		id := ownerIdentity{namespace: o.namespace, kind: o.kind, name: o.name}
		occurrenceCounters[id]++
		occ := occurrenceCounters[id]

		ref := ObjectRef{
			Namespace:  o.namespace,
			Category:   o.category,
			Kind:       o.kind,
			Name:       o.name,
			Canonical:  normalizeName(o.name),
			Occurrence: occ,
		}
		allRefs = append(allRefs, ref)

		if reasons := validation.IsDNS1123Subdomain(o.name); len(reasons) > 0 {
			report.InvalidNames = append(report.InvalidNames, InvalidNameEntry{
				Ref:     ref,
				Reasons: append([]string(nil), reasons...),
			})
		}

		ensureReserved(o.namespace)[ref.Canonical] = struct{}{}
		sameKindGroups[groupKey{o.namespace, o.kind, ref.Canonical}] =
			append(sameKindGroups[groupKey{o.namespace, o.kind, ref.Canonical}], ref)
		categoryGroups[categoryKey{o.namespace, o.category, ref.Canonical}] =
			append(categoryGroups[categoryKey{o.namespace, o.category, ref.Canonical}], ref)
		crossGroups[crossKey{o.namespace, ref.Canonical}] =
			append(crossGroups[crossKey{o.namespace, ref.Canonical}], ref)
	}

	sortObjectRefs(allRefs)
	report.Indexed = allRefs

	// Same-kind groups: identical raw names are hard conflicts; groups whose
	// raw names differ only collapse because of normalization.
	var blockingGroups []NameConflict
	for key, members := range sameKindGroups {
		if len(members) < 2 {
			continue
		}
		sortObjectRefs(members)
		conflict := NameConflict{
			Namespace: key.namespace,
			Category:  nameCategoryByKind[key.kind],
			Kind:      key.kind,
			Canonical: key.canonical,
			Members:   members,
		}
		if distinctRawNames(members) {
			report.NormalizationConflicts = append(report.NormalizationConflicts, conflict)
		} else {
			report.HardConflicts = append(report.HardConflicts, conflict)
		}
		blockingGroups = append(blockingGroups, conflict)
	}
	sortConflicts(report.HardConflicts)
	sortConflicts(report.NormalizationConflicts)

	// Same category, different kinds (legal, but a unified-index ambiguity).
	for key, members := range categoryGroups {
		if len(members) < 2 || !hasMultipleKinds(members) {
			continue
		}
		sortObjectRefs(members)
		report.CategoryConflicts = append(report.CategoryConflicts, NameConflict{
			Namespace: key.namespace,
			Category:  key.category,
			Canonical: key.canonical,
			Members:   members,
		})
	}
	sortConflicts(report.CategoryConflicts)

	// Same canonical name across categories (legal, informational).
	for key, members := range crossGroups {
		if len(members) < 2 || !hasMultipleCategories(members) {
			continue
		}
		sortObjectRefs(members)
		report.CrossCategoryNames = append(report.CrossCategoryNames, CrossCategoryName{
			Namespace: key.namespace,
			Canonical: key.canonical,
			Members:   members,
		})
	}
	sort.Slice(report.CrossCategoryNames, func(i, j int) bool {
		if report.CrossCategoryNames[i].Namespace != report.CrossCategoryNames[j].Namespace {
			return report.CrossCategoryNames[i].Namespace < report.CrossCategoryNames[j].Namespace
		}
		return report.CrossCategoryNames[i].Canonical < report.CrossCategoryNames[j].Canonical
	})

	sort.Slice(report.EmptyNames, func(i, j int) bool {
		a, b := report.EmptyNames[i], report.EmptyNames[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Kind < b.Kind
	})
	sort.Slice(report.InvalidNames, func(i, j int) bool {
		return objectRefLess(report.InvalidNames[i].Ref, report.InvalidNames[j].Ref)
	})

	report.RenamePlans = buildRenamePlans(blockingGroups, refs, occurrence, reserved)
	return report
}

// buildRenamePlans turns same-kind conflicts into deterministic plans: the
// occurrence with the most inbound references keeps its name (renaming it
// would cost the most reference updates); remaining occurrences are renamed.
// Ties are broken by declared name then occurrence, never by input order.
func buildRenamePlans(
	groups []NameConflict,
	refs []nameReference,
	occurrence map[ownerIdentity]int,
	reserved map[string]map[string]struct{},
) []RenamePlan {
	sortConflicts(groups)

	// Resolve a referrer (which is usually itself an indexed owner) to its
	// deterministic ObjectRef, including occurrence when available.
	referrerRef := func(namespace, kind, name string) ObjectRef {
		ref := ObjectRef{
			Namespace: namespace,
			Category:  nameCategoryByKind[kind],
			Kind:      kind,
			Name:      name,
			Canonical: normalizeName(name),
		}
		if name != "" {
			if occ, ok := occurrence[ownerIdentity{namespace, kind, name}]; ok {
				ref.Occurrence = occ
			}
		}
		return ref
	}

	var plans []RenamePlan
	for _, group := range groups {
		members := append([]ObjectRef(nil), group.Members...)
		sortObjectRefs(members)

		// Attribute inbound references to members. The first sorted member
		// owning each exact raw spelling receives every reference that spells
		// it: exact duplicates cannot own references uniquely, they collapse
		// onto the surviving occurrence. Exact spelling matters for
		// normalization conflicts ("Shared" vs "shared"), where each raw
		// variant keeps its own referrers.
		ownerByRaw := make(map[string]int)
		for i, m := range members { // members are already sorted
			if _, ok := ownerByRaw[m.Name]; !ok {
				ownerByRaw[m.Name] = i
			}
		}
		inbound := make(map[int]int) // member index -> reference count
		updatesFor := make([][]ReferenceUpdate, len(members))
		for _, r := range refs {
			if r.fromNamespace != group.Namespace || r.toKind != group.Kind {
				continue
			}
			idx, ok := ownerByRaw[r.toName]
			if !ok {
				continue
			}
			inbound[idx]++
			updatesFor[idx] = append(updatesFor[idx], ReferenceUpdate{
				Referrer:  referrerRef(r.fromNamespace, r.fromKind, r.fromName),
				FieldPath: r.fieldPath,
				OldName:   r.toName,
			})
		}

		// Winner: max inbound references, then smallest raw name, then
		// smallest occurrence.
		winnerIdx := 0
		for i := 1; i < len(members); i++ {
			if inbound[i] > inbound[winnerIdx] ||
				(inbound[i] == inbound[winnerIdx] && objectRefLess(members[i], members[winnerIdx])) {
				winnerIdx = i
			}
		}

		losers := make([]ObjectRef, 0, len(members)-1)
		for i, m := range members {
			if i != winnerIdx {
				losers = append(losers, m)
			}
		}
		sortObjectRefs(losers)

		nsReserved := reserved[group.Namespace]
		if nsReserved == nil {
			nsReserved = map[string]struct{}{}
			reserved[group.Namespace] = nsReserved
		}

		winnerUpdates := append([]ReferenceUpdate(nil), updatesFor[winnerIdx]...)
		for u := range winnerUpdates {
			// References to the winner are not rewritten; NewName repeats the
			// surviving declared name to make the no-op explicit.
			winnerUpdates[u].NewName = members[winnerIdx].Name
		}
		sortReferenceUpdates(winnerUpdates)

		plan := RenamePlan{
			Namespace:              group.Namespace,
			Category:               group.Category,
			Kind:                   group.Kind,
			Canonical:              group.Canonical,
			Winner:                 members[winnerIdx],
			WinnerReferenceUpdates: winnerUpdates,
		}

		for _, loser := range losers {
			loserIdx := -1
			for j, m := range members {
				if m == loser {
					loserIdx = j
					break
				}
			}
			newName := generateCandidateName(group.Canonical, group.Kind, nsReserved)
			nsReserved[normalizeName(newName)] = struct{}{}

			updates := append([]ReferenceUpdate(nil), updatesFor[loserIdx]...)
			for u := range updates {
				updates[u].NewName = newName
			}
			sortReferenceUpdates(updates)

			plan.Steps = append(plan.Steps, RenameStep{
				Target:           loser,
				NewName:          newName,
				ReferenceUpdates: updates,
			})
		}

		plans = append(plans, plan)
	}
	return plans
}

// generateCandidateName returns a deterministic, DNS-1123 subdomain valid
// replacement name, capped at 63 characters, that is not present in reserved.
// It first tries "<base>-<kind-slug>" for readability, then falls back to
// "<base>-2", "<base>-3", ...; base is truncated (without leaving a trailing
// dash or dot) to respect the 63-character limit.
func generateCandidateName(base, kind string, reserved map[string]struct{}) string {
	slug, ok := kindSlug[kind]
	if !ok {
		slug = strings.ToLower(kind)
	}
	try := func(suffix string) string {
		limit := maxDNSLabelLength - len(suffix) - 1
		if limit <= 0 {
			return ""
		}
		b := base
		if len(b) > limit {
			b = strings.TrimRight(b[:limit], "-.")
		}
		if b == "" {
			return ""
		}
		return b + "-" + suffix
	}
	acceptable := func(candidate string) bool {
		if candidate == "" || len(candidate) > maxDNSLabelLength {
			return false
		}
		if len(validation.IsDNS1123Subdomain(candidate)) > 0 {
			return false
		}
		if _, taken := reserved[normalizeName(candidate)]; taken {
			return false
		}
		return true
	}

	if candidate := try(slug); acceptable(candidate) {
		return candidate
	}
	for n := 2; ; n++ {
		suffix := strconv.Itoa(n)
		candidate := try(suffix)
		if candidate == "" {
			// The base carries no usable DNS character (e.g. all symbols);
			// fall back to a fixed seed so the search always terminates.
			candidate = "resource-" + suffix
		}
		if acceptable(candidate) {
			return candidate
		}
	}
}

// extractNameReferences walks the typed Kubernetes objects kompose produces
// and returns every inbound reference to an indexed name.
//
// Pod-local volume names (pod.spec.volumes[].name paired with
// volumeMounts[].name) are deliberately not collected: they are scoped to one
// pod, not to the namespace, and therefore cannot conflict with cluster
// resources. Service selectors and NetworkPolicy selectors match labels, not
// names, and are ignored for the same reason.
func extractNameReferences(obj runtime.Object, namespace, kind, name string) []nameReference {
	var refs []nameReference
	add := func(fieldPath, toKind, toName string) {
		if toName == "" {
			return
		}
		refs = append(refs, nameReference{
			fromNamespace: namespace,
			fromKind:      kind,
			fromName:      name,
			fieldPath:     fieldPath,
			toKind:        toKind,
			toName:        toName,
		})
	}

	switch t := obj.(type) {
	case *corev1.Service:
		// Spec.Selector matches labels; no name references.

	case *networkingv1.Ingress:
		for ri := range t.Spec.Rules {
			http := t.Spec.Rules[ri].HTTP
			if http == nil {
				continue
			}
			for pi := range http.Paths {
				backend := http.Paths[pi].Backend.Service
				if backend == nil {
					continue
				}
				add(fmt.Sprintf("spec.rules[%d].http.paths[%d].backend.service.name", ri, pi),
					"Service", backend.Name)
			}
		}

	case *autoscalingv2.HorizontalPodAutoscaler:
		if t.Spec.ScaleTargetRef.Name != "" {
			toKind := t.Spec.ScaleTargetRef.Kind
			if toKind == "" {
				toKind = "Deployment"
			}
			add("spec.scaleTargetRef.name", toKind, t.Spec.ScaleTargetRef.Name)
		}

	case *autoscalingv2beta2.HorizontalPodAutoscaler:
		if t.Spec.ScaleTargetRef.Name != "" {
			toKind := t.Spec.ScaleTargetRef.Kind
			if toKind == "" {
				toKind = "Deployment"
			}
			add("spec.scaleTargetRef.name", toKind, t.Spec.ScaleTargetRef.Name)
		}

	case *appsv1.Deployment:
		collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)

	case *appsv1.DaemonSet:
		collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)

	case *appsv1.StatefulSet:
		collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)
		if t.Spec.ServiceName != "" {
			add("spec.serviceName", "Service", t.Spec.ServiceName)
		}

	case *appsv1.ReplicaSet:
		collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)

	case *corev1.ReplicationController:
		if t.Spec.Template != nil {
			collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)
		}

	case *corev1.Pod:
		collectPodSpecReferences("spec.", &t.Spec, add)

	case *batchv1.Job:
		collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)

	case *batchv1.CronJob:
		collectPodSpecReferences("spec.jobTemplate.spec.template.spec.",
			&t.Spec.JobTemplate.Spec.Template.Spec, add)

	case *deployapi.DeploymentConfig:
		if t.Spec.Template != nil {
			collectPodSpecReferences("spec.template.spec.", &t.Spec.Template.Spec, add)
		}
	}

	return refs
}

// collectPodSpecReferences collects namespace-scoped name references from a
// PodSpec using JSON-style field paths anchored at prefix.
func collectPodSpecReferences(prefix string, ps *corev1.PodSpec, add func(fieldPath, toKind, toName string)) {
	for i := range ps.Volumes {
		v := &ps.Volumes[i]
		vp := fmt.Sprintf("%svolumes[%d].", prefix, i)
		if v.PersistentVolumeClaim != nil {
			add(vp+"persistentVolumeClaim.claimName",
				"PersistentVolumeClaim", v.PersistentVolumeClaim.ClaimName)
		}
		if v.ConfigMap != nil {
			add(vp+"configMap.name", "ConfigMap", v.ConfigMap.Name)
		}
		if v.Ephemeral != nil {
			add(vp+"ephemeral.volumeClaimTemplate.metadata.name",
				"PersistentVolumeClaim", v.Ephemeral.VolumeClaimTemplate.Name)
		}
	}

	walkContainers := func(section string, containers []corev1.Container) {
		for ci := range containers {
			c := &containers[ci]
			for ei := range c.EnvFrom {
				if c.EnvFrom[ei].ConfigMapRef != nil {
					add(fmt.Sprintf("%s%s[%d].envFrom[%d].configMapRef.name", prefix, section, ci, ei),
						"ConfigMap", c.EnvFrom[ei].ConfigMapRef.Name)
				}
			}
			for ei := range c.Env {
				if vf := c.Env[ei].ValueFrom; vf != nil && vf.ConfigMapKeyRef != nil {
					add(fmt.Sprintf("%s%s[%d].env[%d].valueFrom.configMapKeyRef.name", prefix, section, ci, ei),
						"ConfigMap", vf.ConfigMapKeyRef.Name)
				}
			}
		}
	}
	walkContainers("initContainers", ps.InitContainers)
	walkContainers("containers", ps.Containers)
}

// --- deterministic ordering helpers ---

func ownerLess(a, b nameOwner) bool {
	if a.namespace != b.namespace {
		return a.namespace < b.namespace
	}
	if ra, rb := categoryRank(a.category), categoryRank(b.category); ra != rb {
		return ra < rb
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	ca, cb := normalizeName(a.name), normalizeName(b.name)
	if ca != cb {
		return ca < cb
	}
	if a.name != b.name {
		return a.name < b.name
	}
	return false
}

func objectRefLess(a, b ObjectRef) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if ra, rb := categoryRank(a.Category), categoryRank(b.Category); ra != rb {
		return ra < rb
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Canonical != b.Canonical {
		return a.Canonical < b.Canonical
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Occurrence < b.Occurrence
}

func sortObjectRefs(refs []ObjectRef) {
	sort.Slice(refs, func(i, j int) bool { return objectRefLess(refs[i], refs[j]) })
}

func sortConflicts(conflicts []NameConflict) {
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i], conflicts[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Canonical != b.Canonical {
			return a.Canonical < b.Canonical
		}
		if ra, rb := categoryRank(a.Category), categoryRank(b.Category); ra != rb {
			return ra < rb
		}
		return a.Kind < b.Kind
	})
}

func sortReferenceUpdates(updates []ReferenceUpdate) {
	sort.Slice(updates, func(i, j int) bool {
		a, b := updates[i], updates[j]
		if a.Referrer != b.Referrer {
			if a.Referrer.Namespace != b.Referrer.Namespace {
				return a.Referrer.Namespace < b.Referrer.Namespace
			}
			if ra, rb := categoryRank(a.Referrer.Category), categoryRank(b.Referrer.Category); ra != rb {
				return ra < rb
			}
			if a.Referrer.Kind != b.Referrer.Kind {
				return a.Referrer.Kind < b.Referrer.Kind
			}
			if a.Referrer.Name != b.Referrer.Name {
				return a.Referrer.Name < b.Referrer.Name
			}
			if a.Referrer.Occurrence != b.Referrer.Occurrence {
				return a.Referrer.Occurrence < b.Referrer.Occurrence
			}
		}
		return a.FieldPath < b.FieldPath
	})
}

func distinctRawNames(members []ObjectRef) bool {
	first := members[0].Name
	for _, m := range members[1:] {
		if m.Name != first {
			return true
		}
	}
	return false
}

func hasMultipleKinds(members []ObjectRef) bool {
	kind := members[0].Kind
	for _, m := range members[1:] {
		if m.Kind != kind {
			return true
		}
	}
	return false
}

func hasMultipleCategories(members []ObjectRef) bool {
	category := members[0].Category
	for _, m := range members[1:] {
		if m.Category != category {
			return true
		}
	}
	return false
}

func sortedStringSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
