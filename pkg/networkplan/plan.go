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

package networkplan

import (
	"sort"
	"strconv"
	"strings"
)

// NameScope identifies which layer of the unified naming a result applies to.
type NameScope string

const (
	// ScopeNetwork is a top-level Compose network name.
	ScopeNetwork NameScope = "network"
	// ScopeService is the implicit DNS name derived from a service key.
	ScopeService NameScope = "service"
	// ScopeAlias is a per-network service alias.
	ScopeAlias NameScope = "alias"
	// ScopeHostname is the pod hostname derived from the Compose hostname.
	ScopeHostname NameScope = "hostname"
	// ScopeSubdomain is the pod subdomain derived from the Compose domain name.
	ScopeSubdomain NameScope = "subdomain"
)

// ReferenceKind identifies an occurrence in the model that references a
// name and that has to be updated.
type ReferenceKind string

const (
	// RefServiceNetwork is a service-to-network attachment
	// (services.<svc>.networks.<net>).
	RefServiceNetwork ReferenceKind = "service-network"
	// RefLinkTarget is the target part of a service link.
	RefLinkTarget ReferenceKind = "link-target"
	// RefLinkAlias is the local alias part of a service link.
	RefLinkAlias ReferenceKind = "link-alias"
	// RefNetworkModeService is the target of network_mode: service:<name>.
	RefNetworkModeService ReferenceKind = "network-mode-service"
)

// Input is the neutral, framework independent model analyzed by Build.
// Build only reads Input; it never mutates the maps, slices or strings
// it contains.
type Input struct {
	// Networks maps the Compose network key to its definition.
	Networks map[string]Network
	// Services maps the Compose service key to its naming relevant data.
	Services map[string]Service
}

// Network describes one top-level Compose network.
type Network struct {
	// Key is the key used to reference the network from services.
	Key string
	// Name is the effective network name (the Compose "name" field);
	// when empty the Key is used.
	Name string
}

// EffectiveName returns the Compose name of the network or its key.
func (n Network) EffectiveName() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Key
}

// Service gathers the naming relevant fields of one Compose service.
type Service struct {
	// Name is the Compose service key; the Services map key is the
	// authoritative identity, Name is provided for convenience and
	// mirrors compose-go's ServiceConfig.Name field.
	Name string
	// Hostname is the optional Compose "hostname" field.
	Hostname string
	// DomainName is the optional Compose "domainname" field.
	DomainName string
	// NetworkMode is the raw Compose "network_mode" field.
	NetworkMode string
	// Links holds the raw Compose "links" entries ("target" or "target:alias").
	Links []string
	// Attachments maps the referenced network key to its per-service
	// configuration, including the declared aliases.
	Attachments map[string]Attachment
}

// Attachment is the per-service configuration of one network.
type Attachment struct {
	Aliases []string
}

// Rename describes one canonical name unification: the value currently
// found in the model and the Kubernetes compliant value it must become.
type Rename struct {
	// Scope is the naming layer the entry belongs to.
	Scope NameScope
	// Service is the owning service key (empty for networks).
	Service string
	// Network is the network key for network and alias scopes.
	Network string
	// Field is the model path of the entry, for example
	// "services.web.networks.front.aliases[0]".
	Field string
	// Current is the raw value found in the model.
	Current string
	// Proposed is the canonical Kubernetes DNS name.
	Proposed string
	// Reasons explains every applied transformation.
	Reasons []Reason
}

// ReferenceFix describes one occurrence that references a name and has
// to be rewritten to the canonical value.
type ReferenceFix struct {
	// Kind classifies the occurrence.
	Kind ReferenceKind
	// Service is the service containing the occurrence.
	Service string
	// Network is the referenced network key when applicable.
	Network string
	// Field is the model path of the occurrence.
	Field string
	// Current is the raw reference text.
	Current string
	// Proposed is the rewritten reference. It is empty when Resolved
	// is false and the reference must be repaired manually.
	Proposed string
	// Resolved is false when the referenced target does not exist.
	Resolved bool
	// Reasons explains why the reference must change or why it is broken.
	Reasons []Reason
}

// Claim records one DNS name offered by one service on one network.
type Claim struct {
	// Service is the service key offering the name.
	Service string
	// Network is the network key on which the name is offered.
	Network string
	// Source is either ScopeService (implicit service name) or
	// ScopeAlias (explicit per-network alias).
	Source NameScope
	// Current is the raw value as declared in the model.
	Current string
}

// Conflict reports a canonical DNS name claimed by more than one service.
type Conflict struct {
	// Canonical is the normalized Kubernetes DNS name involved.
	Canonical string
	// Scope is "cross-network" when claims live on different Compose
	// networks and "same-network" otherwise.
	Scope string
	// Networks lists the networks involved, sorted and deduplicated.
	Networks []string
	// Claims lists every claim on the canonical name, sorted.
	Claims []Claim
	// Reasons always contains the reason matching Scope.
	Reasons []Reason
}

const (
	// ConflictScopeCrossNetwork qualifies conflicts between networks.
	ConflictScopeCrossNetwork = "cross-network"
	// ConflictScopeSameNetwork qualifies conflicts within one network.
	ConflictScopeSameNetwork = "same-network"
)

// DroppedName describes an entry that cannot be normalized into a valid
// DNS name and therefore has no proposed value; it must be removed or
// replaced by the user. Empty aliases are reported here as well.
type DroppedName struct {
	// Scope is the naming layer the entry belongs to.
	Scope NameScope
	// Service is the owning service key (empty for networks).
	Service string
	// Network is the network key for alias scopes.
	Network string
	// Field is the model path of the entry.
	Field string
	// Current is the raw value found in the model.
	Current string
	// Reasons explains why the entry is dropped (EmptyAlias,
	// NormalizedToEmpty, ...).
	Reasons []Reason
}

// Plan is the complete read-only migration plan.
type Plan struct {
	// Renames lists every canonical name that differs from the raw value.
	Renames []Rename
	// ReferenceFixes lists references that must be rewritten, including
	// references that cannot be resolved against the model.
	ReferenceFixes []ReferenceFix
	// Conflicts lists same-name collisions, in particular same names
	// claimed on different Compose networks.
	Conflicts []Conflict
	// Dropped lists entries that normalize to no name at all, such as
	// empty aliases.
	Dropped []DroppedName
}

// HasChanges reports whether the plan contains any modification,
// conflict or invalid entry.
func (p Plan) HasChanges() bool {
	return len(p.Renames) > 0 || len(p.ReferenceFixes) > 0 || len(p.Conflicts) > 0 || len(p.Dropped) > 0
}

// HasBlockingIssues reports whether conversion would be ambiguous or
// produce invalid or colliding names (conflicts, unresolved references,
// dropped entries).
func (p Plan) HasBlockingIssues() bool {
	for _, fix := range p.ReferenceFixes {
		if !fix.Resolved {
			return true
		}
	}
	return len(p.Conflicts) > 0 || len(p.Dropped) > 0
}

type builder struct {
	input Input
	plan  Plan
	// serviceDNS caches the canonical DNS name of each service key.
	serviceDNS map[string]DNSNameResult
}

// Build analyzes input and returns the migration plan. It is a pure
// read-only analysis: no file is written, no DNS lookup is performed and
// input (including all nested maps and slices) is never modified.
func Build(input Input) Plan {
	b := &builder{
		input:      input,
		serviceDNS: make(map[string]DNSNameResult, len(input.Services)),
	}

	for key := range input.Services {
		b.serviceDNS[key] = NormalizeServiceDNSName(key)
	}

	b.planNetworks()
	b.planServices()
	b.planConflicts()

	sortPlan(&b.plan)
	return b.plan
}

func (b *builder) planNetworks() {
	for key, network := range b.input.Networks {
		raw := network.EffectiveName()
		result := NormalizeNetworkName(raw)
		field := "networks." + key
		switch {
		case result.Drop:
			b.plan.Dropped = append(b.plan.Dropped, DroppedName{
				Scope:   ScopeNetwork,
				Network: key,
				Field:   field,
				Current: result.Original,
				Reasons: result.Reasons,
			})
		case result.Changed():
			b.plan.Renames = append(b.plan.Renames, Rename{
				Scope:    ScopeNetwork,
				Network:  key,
				Field:    field,
				Current:  result.Original,
				Proposed: result.Name,
				Reasons:  result.Reasons,
			})
		}
	}
}

func (b *builder) planServices() {
	for key, service := range b.input.Services {
		b.planServiceIdentity(key, service)
		b.planAttachments(key, service)
		b.planLinks(key, service)
		b.planNetworkMode(key, service)
	}
}

func (b *builder) planServiceIdentity(key string, service Service) {
	if result := b.serviceDNS[key]; result.Changed() && !result.Drop {
		b.plan.Renames = append(b.plan.Renames, Rename{
			Scope:    ScopeService,
			Service:  key,
			Field:    "services." + key,
			Current:  result.Original,
			Proposed: result.Name,
			Reasons:  result.Reasons,
		})
	}

	b.planPodName(ScopeHostname, key, "services."+key+".hostname", service.Hostname)
	b.planPodName(ScopeSubdomain, key, "services."+key+".domainname", service.DomainName)
}

// planPodName normalizes hostname (single label) and domainname
// (subdomain) fields.
func (b *builder) planPodName(scope NameScope, key, field, raw string) {
	if raw == "" {
		return
	}
	profile := ProfileSubdomain
	if scope == ScopeHostname {
		profile = ProfileLabel
	}
	result := NormalizeDNSName(raw, profile)
	if result.Drop {
		b.plan.Dropped = append(b.plan.Dropped, DroppedName{
			Scope:   scope,
			Service: key,
			Field:   field,
			Current: result.Original,
			Reasons: result.Reasons,
		})
		return
	}
	if result.Changed() {
		b.plan.Renames = append(b.plan.Renames, Rename{
			Scope:    scope,
			Service:  key,
			Field:    field,
			Current:  result.Original,
			Proposed: result.Name,
			Reasons:  result.Reasons,
		})
	}
}

func (b *builder) planAttachments(key string, service Service) {
	for networkKey, attachment := range service.Attachments {
		field := "services." + key + ".networks." + networkKey

		network, ok := b.input.Networks[networkKey]
		if !ok {
			b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, ReferenceFix{
				Kind:     RefServiceNetwork,
				Service:  key,
				Network:  networkKey,
				Field:    field,
				Current:  networkKey,
				Resolved: false,
				Reasons:  []Reason{ReasonUnresolvedTarget},
			})
		} else {
			canonical := NormalizeNetworkName(network.EffectiveName())
			if !canonical.Drop && canonical.Name != networkKey {
				b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, ReferenceFix{
					Kind:     RefServiceNetwork,
					Service:  key,
					Network:  networkKey,
					Field:    field,
					Current:  networkKey,
					Proposed: canonical.Name,
					Resolved: true,
					Reasons:  []Reason{ReasonNetworkReferenceMismatch},
				})
			}
		}

		for index, rawAlias := range attachment.Aliases {
			aliasField := field + ".aliases[" + strconv.Itoa(index) + "]"
			result := NormalizeDNSName(rawAlias, ProfileSubdomain)
			switch {
			case result.Drop:
				b.plan.Dropped = append(b.plan.Dropped, DroppedName{
					Scope:   ScopeAlias,
					Service: key,
					Network: networkKey,
					Field:   aliasField,
					Current: result.Original,
					Reasons: result.Reasons,
				})
			case result.Changed():
				b.plan.Renames = append(b.plan.Renames, Rename{
					Scope:    ScopeAlias,
					Service:  key,
					Network:  networkKey,
					Field:    aliasField,
					Current:  result.Original,
					Proposed: result.Name,
					Reasons:  result.Reasons,
				})
			}
		}
	}
}

func (b *builder) planLinks(key string, service Service) {
	for index, raw := range service.Links {
		field := "services." + key + ".links[" + strconv.Itoa(index) + "]"
		target, alias, hasAlias := strings.Cut(raw, ":")

		targetResult, ok := b.serviceDNS[target]
		if !ok {
			b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, ReferenceFix{
				Kind:     RefLinkTarget,
				Service:  key,
				Field:    field,
				Current:  target,
				Resolved: false,
				Reasons:  []Reason{ReasonUnresolvedTarget},
			})
		} else {
			var reasons []Reason
			if targetResult.Changed() {
				reasons = append(reasons, targetResult.Reasons...)
			}
			if targetService, ok := b.input.Services[target]; ok &&
				len(targetService.Attachments) > 0 && len(service.Attachments) > 0 &&
				!shareNetwork(service.Attachments, targetService.Attachments) {
				reasons = append(reasons, ReasonNoSharedNetwork)
			}
			if len(reasons) > 0 {
				b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, ReferenceFix{
					Kind:     RefLinkTarget,
					Service:  key,
					Field:    field,
					Current:  target,
					Proposed: targetResult.Name,
					Resolved: true,
					Reasons:  reasons,
				})
			}
		}

		if hasAlias {
			b.planLinkAlias(key, field, alias)
		}
	}
}

func (b *builder) planLinkAlias(serviceKey, linkField, rawAlias string) {
	result := NormalizeDNSName(rawAlias, ProfileSubdomain)
	if result.Drop {
		b.plan.Dropped = append(b.plan.Dropped, DroppedName{
			Scope:   ScopeAlias,
			Service: serviceKey,
			Field:   linkField,
			Current: result.Original,
			Reasons: result.Reasons,
		})
		return
	}
	if result.Changed() {
		b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, ReferenceFix{
			Kind:     RefLinkAlias,
			Service:  serviceKey,
			Field:    linkField,
			Current:  result.Original,
			Proposed: result.Name,
			Resolved: true,
			Reasons:  result.Reasons,
		})
	}
}

func (b *builder) planNetworkMode(key string, service Service) {
	target, ok := strings.CutPrefix(service.NetworkMode, "service:")
	if !ok {
		return
	}
	field := "services." + key + ".network_mode"
	fix := ReferenceFix{
		Kind:    RefNetworkModeService,
		Service: key,
		Field:   field,
		Current: target,
	}
	result, exists := b.serviceDNS[target]
	if !exists {
		fix.Resolved = false
		fix.Reasons = []Reason{ReasonUnresolvedTarget}
		b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, fix)
		return
	}
	fix.Resolved = true
	fix.Proposed = result.Name
	if result.Changed() {
		fix.Reasons = result.Reasons
	}
	if fix.Proposed != fix.Current || len(fix.Reasons) > 0 {
		b.plan.ReferenceFixes = append(b.plan.ReferenceFixes, fix)
	}
}

// planConflicts collects every DNS name offered on every network and
// reports names claimed by different services. Claims on different
// Compose networks are cross-network conflicts because Kubernetes
// Services share one flat DNS namespace.
func (b *builder) planConflicts() {
	claimsByCanonical := map[string][]Claim{}

	for serviceKey, service := range b.input.Services {
		serviceDNS := b.serviceDNS[serviceKey]
		for networkKey := range service.Attachments {
			if _, ok := b.input.Networks[networkKey]; !ok {
				continue
			}
			if !serviceDNS.Drop {
				claimsByCanonical[serviceDNS.Name] = append(claimsByCanonical[serviceDNS.Name], Claim{
					Service: serviceKey,
					Network: networkKey,
					Source:  ScopeService,
					Current: serviceKey,
				})
			}
		}

		for networkKey, attachment := range service.Attachments {
			if _, ok := b.input.Networks[networkKey]; !ok {
				continue
			}
			for _, rawAlias := range attachment.Aliases {
				result := NormalizeDNSName(rawAlias, ProfileSubdomain)
				if result.Drop {
					continue
				}
				claimsByCanonical[result.Name] = append(claimsByCanonical[result.Name], Claim{
					Service: serviceKey,
					Network: networkKey,
					Source:  ScopeAlias,
					Current: rawAlias,
				})
			}
		}
	}

	canonicals := make([]string, 0, len(claimsByCanonical))
	for canonical := range claimsByCanonical {
		canonicals = append(canonicals, canonical)
	}
	sort.Strings(canonicals)

	for _, canonical := range canonicals {
		claims := deduplicateClaims(claimsByCanonical[canonical])
		if conflict, ok := buildConflict(canonical, claims); ok {
			b.plan.Conflicts = append(b.plan.Conflicts, conflict)
		}
	}
}

// buildConflict returns a conflict when distinct services claim the same
// canonical name. Cross-network conflicts take precedence over
// same-network duplicates.
func buildConflict(canonical string, claims []Claim) (Conflict, bool) {
	services := map[string]struct{}{}
	networks := map[string]struct{}{}
	servicesByNetwork := map[string]map[string]struct{}{}
	for _, claim := range claims {
		services[claim.Service] = struct{}{}
		networks[claim.Network] = struct{}{}
		if _, ok := servicesByNetwork[claim.Network]; !ok {
			servicesByNetwork[claim.Network] = map[string]struct{}{}
		}
		servicesByNetwork[claim.Network][claim.Service] = struct{}{}
	}

	if len(services) <= 1 {
		// The same service answering to its name on several networks is
		// expected and maps to a single Kubernetes Service.
		return Conflict{}, false
	}

	conflict := Conflict{
		Canonical: canonical,
		Claims:    claims,
	}

	for network := range networks {
		conflict.Networks = append(conflict.Networks, network)
	}
	sort.Strings(conflict.Networks)

	if len(networks) > 1 {
		conflict.Scope = ConflictScopeCrossNetwork
		conflict.Reasons = []Reason{ReasonCrossNetworkConflict}
	} else {
		for _, networkServices := range servicesByNetwork {
			if len(networkServices) > 1 {
				conflict.Scope = ConflictScopeSameNetwork
				conflict.Reasons = []Reason{ReasonSameNetworkConflict}
				break
			}
		}
	}
	if conflict.Scope == "" {
		return Conflict{}, false
	}
	return conflict, true
}

// deduplicateClaims removes identical claims and returns them sorted.
func deduplicateClaims(claims []Claim) []Claim {
	seen := map[Claim]struct{}{}
	result := make([]Claim, 0, len(claims))
	for _, claim := range claims {
		if _, ok := seen[claim]; ok {
			continue
		}
		seen[claim] = struct{}{}
		result = append(result, claim)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Network != result[j].Network {
			return result[i].Network < result[j].Network
		}
		if result[i].Service != result[j].Service {
			return result[i].Service < result[j].Service
		}
		if result[i].Source != result[j].Source {
			return result[i].Source < result[j].Source
		}
		return result[i].Current < result[j].Current
	})
	return result
}

func shareNetwork(a, b map[string]Attachment) bool {
	for networkKey := range a {
		if _, ok := b[networkKey]; ok {
			return true
		}
	}
	return false
}

func sortPlan(plan *Plan) {
	sort.Slice(plan.Renames, func(i, j int) bool {
		return renameKey(plan.Renames[i]) < renameKey(plan.Renames[j])
	})
	sort.Slice(plan.ReferenceFixes, func(i, j int) bool {
		return fixKey(plan.ReferenceFixes[i]) < fixKey(plan.ReferenceFixes[j])
	})
	sort.Slice(plan.Dropped, func(i, j int) bool {
		return droppedKey(plan.Dropped[i]) < droppedKey(plan.Dropped[j])
	})
	sort.Slice(plan.Conflicts, func(i, j int) bool {
		if plan.Conflicts[i].Canonical != plan.Conflicts[j].Canonical {
			return plan.Conflicts[i].Canonical < plan.Conflicts[j].Canonical
		}
		return plan.Conflicts[i].Scope < plan.Conflicts[j].Scope
	})
}

func renameKey(r Rename) string {
	return strings.Join([]string{string(r.Scope), r.Network, r.Service, r.Field, r.Current, r.Proposed}, "\x00")
}

func fixKey(f ReferenceFix) string {
	resolved := "1"
	if !f.Resolved {
		resolved = "0"
	}
	return strings.Join([]string{string(f.Kind), f.Network, f.Service, f.Field, f.Current, f.Proposed, resolved}, "\x00")
}

func droppedKey(d DroppedName) string {
	return strings.Join([]string{string(d.Scope), d.Network, d.Service, d.Field, d.Current}, "\x00")
}
