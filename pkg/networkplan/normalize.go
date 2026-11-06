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

// Package networkplan produces a read-only migration plan that unifies
// Compose network names, per-network service aliases and Kubernetes DNS
// names.
//
// The plan only inspects its input: it never writes files, never performs
// DNS or network lookups and never mutates the input model (neither the
// neutral Input value nor the compose-go project the adapter is built
// from). Every suggested modification is returned as data, together with
// the reason why it is needed.
package networkplan

import (
	"regexp"
	"strings"
)

const (
	// dnsLabelMaxLength is the maximum length of one RFC 1123 DNS label.
	dnsLabelMaxLength = 63
	// dnsSubdomainMaxLength is the maximum length of an RFC 1123 DNS subdomain.
	dnsSubdomainMaxLength = 253
)

// invalidDNSNameChars matches every character that is not allowed in an
// RFC 1123 DNS subdomain (lowercase alphanumeric characters, '-' and '.').
var invalidDNSNameChars = regexp.MustCompile(`[^a-z0-9.-]+`)

// Profile selects how dots are handled while normalizing a DNS name.
type Profile int

const (
	// ProfileLabel normalizes the value into a single RFC 1123 DNS label:
	// dots are turned into hyphens, as kompose does for Service names.
	ProfileLabel Profile = iota
	// ProfileSubdomain keeps dots as separators between RFC 1123 labels,
	// which is valid for Kubernetes resource names, aliases and subdomains.
	ProfileSubdomain
)

// Reason explains why a name has to be changed or why an entry is invalid.
// Reasons are stable machine-readable identifiers; use Reason.Description
// when a human readable message is required.
type Reason string

const (
	// ReasonEmptyAlias marks an entry that is empty (or whitespace only).
	// Such an alias cannot produce any DNS name and must be removed.
	ReasonEmptyAlias Reason = "empty-alias"
	// ReasonSurroundingWhitespace marks leading or trailing whitespace.
	ReasonSurroundingWhitespace Reason = "surrounding-whitespace"
	// ReasonTrailingDot marks a trailing dot (the DNS FQDN root marker).
	// Kubernetes DNS names must not end with a dot, so it is stripped.
	ReasonTrailingDot Reason = "trailing-dot"
	// ReasonUppercase marks uppercase characters. DNS is case insensitive
	// while Kubernetes names must be lowercase, so the name is lowercased.
	ReasonUppercase Reason = "uppercase"
	// ReasonUnderscore marks underscores, which are replaced by hyphens.
	ReasonUnderscore Reason = "underscore"
	// ReasonDotInLabel marks dots in a value that must be a single DNS
	// label (for example a Service name); they are replaced by hyphens.
	ReasonDotInLabel Reason = "dot-in-label"
	// ReasonInvalidCharacter marks characters that are not allowed in
	// RFC 1123 DNS names; they are dropped.
	ReasonInvalidCharacter Reason = "invalid-character"
	// ReasonEmptyLabel marks an empty DNS label caused by consecutive
	// dots or by a dot at the beginning of the name; it is removed.
	ReasonEmptyLabel Reason = "empty-label"
	// ReasonLeadingOrTrailingHyphen marks a label that starts or ends
	// with a hyphen; RFC 1123 labels must start and end alphanumerically,
	// so the boundary hyphens are trimmed.
	ReasonLeadingOrTrailingHyphen Reason = "leading-or-trailing-hyphen"
	// ReasonLabelTooLong marks an RFC 1123 label longer than 63 characters;
	// it is truncated to 63 characters.
	ReasonLabelTooLong Reason = "label-too-long"
	// ReasonNameTooLong marks an RFC 1123 subdomain longer than 253
	// characters. The proposed value is returned as-is for review.
	ReasonNameTooLong Reason = "name-too-long"
	// ReasonNormalizedToEmpty marks a non-empty entry whose normalized
	// form is empty (for example "_"); it must be removed or replaced.
	ReasonNormalizedToEmpty Reason = "normalized-to-empty"
	// ReasonNetworkReferenceMismatch marks a service attachment whose
	// network key does not match the canonical Kubernetes network name.
	ReasonNetworkReferenceMismatch Reason = "network-reference-mismatch"
	// ReasonUnresolvedTarget marks a reference (link, network_mode
	// service target or network attachment) that cannot be resolved
	// against any service or network declared in the model.
	ReasonUnresolvedTarget Reason = "unresolved-target"
	// ReasonNoSharedNetwork marks a link whose source and target
	// services are not attached to a common network.
	ReasonNoSharedNetwork Reason = "no-shared-network"
	// ReasonCrossNetworkConflict marks the same canonical DNS name being
	// claimed by different services on different Compose networks.
	// Compose isolates DNS per network while Kubernetes Services share a
	// flat DNS namespace, so the names collide after conversion.
	ReasonCrossNetworkConflict Reason = "cross-network-conflict"
	// ReasonSameNetworkConflict marks the same canonical DNS name being
	// claimed by different services on one Compose network.
	ReasonSameNetworkConflict Reason = "same-network-conflict"
)

// Description returns a short human readable explanation of the reason.
func (r Reason) Description() string {
	switch r {
	case ReasonEmptyAlias:
		return "empty alias is not a valid DNS name and must be removed"
	case ReasonSurroundingWhitespace:
		return "leading or trailing whitespace is not allowed in DNS names"
	case ReasonTrailingDot:
		return "trailing dot denotes the DNS root and is not allowed in Kubernetes names"
	case ReasonUppercase:
		return "Kubernetes DNS names must be lowercase"
	case ReasonUnderscore:
		return "underscores are not allowed in Kubernetes DNS names and are replaced by hyphens"
	case ReasonDotInLabel:
		return "dots are not allowed in a single DNS label and are replaced by hyphens"
	case ReasonInvalidCharacter:
		return "characters outside [a-z0-9.-] are not allowed and are removed"
	case ReasonEmptyLabel:
		return "empty DNS labels caused by consecutive or leading dots are removed"
	case ReasonLeadingOrTrailingHyphen:
		return "DNS labels must start and end with an alphanumeric character; boundary hyphens are trimmed"
	case ReasonLabelTooLong:
		return "DNS labels are limited to 63 characters; the label is truncated"
	case ReasonNameTooLong:
		return "DNS subdomains are limited to 253 characters; the proposed name needs manual review"
	case ReasonNormalizedToEmpty:
		return "the entry normalizes to an empty name and must be removed or replaced"
	case ReasonNetworkReferenceMismatch:
		return "the referenced network key differs from the canonical Kubernetes network name"
	case ReasonUnresolvedTarget:
		return "the referenced name does not match any network or service in the model"
	case ReasonNoSharedNetwork:
		return "the linked services are not attached to a common network"
	case ReasonCrossNetworkConflict:
		return "different services claim the same DNS name on different networks; Kubernetes uses a flat DNS namespace"
	case ReasonSameNetworkConflict:
		return "different services claim the same DNS name on the same network"
	default:
		return string(r)
	}
}

// DNSNameResult is the outcome of normalizing one raw DNS name.
type DNSNameResult struct {
	// Original is the raw value as found in the model.
	Original string
	// Name is the proposed RFC 1123 compliant name. It is empty when
	// Drop is true.
	Name string
	// Reasons lists, in order, every transformation or problem found.
	Reasons []Reason
	// Drop is true when the entry cannot be turned into a DNS name,
	// either because it was empty or because normalization removed all
	// of its characters.
	Drop bool
}

// Changed reports whether the proposed name differs from the raw value.
func (r DNSNameResult) Changed() bool {
	return r.Original != r.Name
}

// NormalizeDNSName normalizes raw into a Kubernetes compatible DNS name
// following the given profile. The function is pure: it performs no I/O
// and no DNS lookups.
//
// Uppercase characters are lowercased, trailing dots are stripped, empty
// entries are explicitly rejected (Drop is true and ReasonEmptyAlias is
// returned) and every other deviation from RFC 1123 is reported in
// Reasons together with the deterministic repair that was applied.
func NormalizeDNSName(raw string, profile Profile) DNSNameResult {
	result := DNSNameResult{Original: raw}

	name := raw
	if trimmed := strings.TrimSpace(name); trimmed != name {
		result.Reasons = appendReason(result.Reasons, ReasonSurroundingWhitespace)
		name = trimmed
	}

	if name == "" {
		result.Drop = true
		result.Reasons = appendReason(result.Reasons, ReasonEmptyAlias)
		return result
	}

	// A trailing dot is the DNS FQDN root marker and is meaningless in
	// Kubernetes resource names. Strip it before further processing so
	// that it is not mistaken for an empty label.
	if strings.HasSuffix(name, ".") {
		result.Reasons = appendReason(result.Reasons, ReasonTrailingDot)
		name = strings.TrimRight(name, ".")
	}

	if lowered := strings.ToLower(name); lowered != name {
		result.Reasons = appendReason(result.Reasons, ReasonUppercase)
		name = lowered
	}

	if strings.Contains(name, "_") {
		result.Reasons = appendReason(result.Reasons, ReasonUnderscore)
		name = strings.ReplaceAll(name, "_", "-")
	}

	if profile == ProfileLabel && strings.Contains(name, ".") {
		result.Reasons = appendReason(result.Reasons, ReasonDotInLabel)
		name = strings.ReplaceAll(name, ".", "-")
	}

	if cleaned := invalidDNSNameChars.ReplaceAllString(name, ""); cleaned != name {
		result.Reasons = appendReason(result.Reasons, ReasonInvalidCharacter)
		name = cleaned
	}

	labels := strings.Split(name, ".")
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == "" {
			result.Reasons = appendReason(result.Reasons, ReasonEmptyLabel)
			continue
		}
		trimmed := strings.Trim(label, "-")
		if trimmed != label {
			result.Reasons = appendReason(result.Reasons, ReasonLeadingOrTrailingHyphen)
		}
		if trimmed == "" {
			result.Reasons = appendReason(result.Reasons, ReasonEmptyLabel)
			continue
		}
		if len(trimmed) > dnsLabelMaxLength {
			result.Reasons = appendReason(result.Reasons, ReasonLabelTooLong)
			trimmed = trimmed[:dnsLabelMaxLength]
		}
		normalized = append(normalized, trimmed)
	}

	name = strings.Join(normalized, ".")
	if len(name) > dnsSubdomainMaxLength {
		result.Reasons = appendReason(result.Reasons, ReasonNameTooLong)
	}
	if name == "" {
		result.Drop = true
		result.Reasons = appendReason(result.Reasons, ReasonNormalizedToEmpty)
	}

	result.Name = name
	return result
}

// NormalizeNetworkName normalizes the effective name of a Compose network
// into the name used for the generated Kubernetes objects. It mirrors the
// compose loader rules (lowercase, underscores to hyphens, invalid
// characters dropped) and additionally repairs RFC 1123 boundary and
// length problems.
func NormalizeNetworkName(raw string) DNSNameResult {
	return NormalizeDNSName(raw, ProfileSubdomain)
}

// NormalizeServiceDNSName normalizes a Compose service key into the DNS
// name of the generated Kubernetes Service. Dots and underscores become
// hyphens, matching kompose's existing service name normalization.
func NormalizeServiceDNSName(raw string) DNSNameResult {
	return NormalizeDNSName(raw, ProfileLabel)
}

// appendReason appends reason only if it is not already present, keeping
// the reported reasons deterministic and free of duplicates.
func appendReason(reasons []Reason, reason Reason) []Reason {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
