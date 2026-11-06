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
	"strings"
	"testing"
)

func TestNormalizeDNSName(t *testing.T) {
	longLabel := strings.Repeat("a", 70)
	truncatedLabel := strings.Repeat("a", dnsLabelMaxLength)

	tests := []struct {
		name     string
		raw      string
		profile  Profile
		wantName string
		wantDrop bool
		want     []Reason
	}{
		{
			name:     "already valid",
			raw:      "web-1",
			profile:  ProfileSubdomain,
			wantName: "web-1",
		},
		{
			name:     "uppercase is lowercased",
			raw:      "Web_DB",
			profile:  ProfileLabel,
			wantName: "web-db",
			want:     []Reason{ReasonUppercase, ReasonUnderscore},
		},
		{
			name:     "single trailing dot is stripped",
			raw:      "db.",
			profile:  ProfileSubdomain,
			wantName: "db",
			want:     []Reason{ReasonTrailingDot},
		},
		{
			name:     "fqdn trailing dot is stripped but inner dots remain",
			raw:      "svc.Local.",
			profile:  ProfileSubdomain,
			wantName: "svc.local",
			want:     []Reason{ReasonTrailingDot, ReasonUppercase},
		},
		{
			name:     "multiple trailing dots are stripped",
			raw:      "db...",
			profile:  ProfileSubdomain,
			wantName: "db",
			want:     []Reason{ReasonTrailingDot},
		},
		{
			name:     "empty alias",
			raw:      "",
			profile:  ProfileSubdomain,
			wantName: "",
			wantDrop: true,
			want:     []Reason{ReasonEmptyAlias},
		},
		{
			name:     "whitespace only alias is empty",
			raw:      "   ",
			profile:  ProfileSubdomain,
			wantName: "",
			wantDrop: true,
			want:     []Reason{ReasonSurroundingWhitespace, ReasonEmptyAlias},
		},
		{
			name:     "surrounding whitespace trimmed",
			raw:      " db ",
			profile:  ProfileSubdomain,
			wantName: "db",
			want:     []Reason{ReasonSurroundingWhitespace},
		},
		{
			name:     "invalid characters normalize to empty",
			raw:      "!!",
			profile:  ProfileSubdomain,
			wantName: "",
			wantDrop: true,
			want:     []Reason{ReasonInvalidCharacter, ReasonEmptyLabel, ReasonNormalizedToEmpty},
		},
		{
			name:     "hyphens only normalize to empty",
			raw:      "-",
			profile:  ProfileSubdomain,
			wantName: "",
			wantDrop: true,
			want:     []Reason{ReasonLeadingOrTrailingHyphen, ReasonEmptyLabel, ReasonNormalizedToEmpty},
		},
		{
			name:     "dots become hyphens in label profile",
			raw:      "My.Service",
			profile:  ProfileLabel,
			wantName: "my-service",
			want:     []Reason{ReasonUppercase, ReasonDotInLabel},
		},
		{
			name:     "dots are preserved in subdomain profile",
			raw:      "My.Service",
			profile:  ProfileSubdomain,
			wantName: "my.service",
			want:     []Reason{ReasonUppercase},
		},
		{
			name:     "boundary hyphens trimmed per label",
			raw:      "-foo-.bar-",
			profile:  ProfileSubdomain,
			wantName: "foo.bar",
			want:     []Reason{ReasonLeadingOrTrailingHyphen},
		},
		{
			name:     "consecutive dots produce empty labels",
			raw:      "a..b.",
			profile:  ProfileSubdomain,
			wantName: "a.b",
			want:     []Reason{ReasonTrailingDot, ReasonEmptyLabel},
		},
		{
			name:     "leading dot produces an empty label",
			raw:      ".foo",
			profile:  ProfileSubdomain,
			wantName: "foo",
			want:     []Reason{ReasonEmptyLabel},
		},
		{
			name:     "underscores and invalid characters",
			raw:      "DB_1.x!",
			profile:  ProfileSubdomain,
			wantName: "db-1.x",
			want:     []Reason{ReasonUppercase, ReasonUnderscore, ReasonInvalidCharacter},
		},
		{
			name:     "label longer than 63 characters is truncated",
			raw:      longLabel,
			profile:  ProfileLabel,
			wantName: truncatedLabel,
			want:     []Reason{ReasonLabelTooLong},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeDNSName(tt.raw, tt.profile)
			if got.Name != tt.wantName {
				t.Errorf("NormalizeDNSName(%q).Name = %q, want %q", tt.raw, got.Name, tt.wantName)
			}
			if got.Drop != tt.wantDrop {
				t.Errorf("NormalizeDNSName(%q).Drop = %v, want %v", tt.raw, got.Drop, tt.wantDrop)
			}
			if tt.want != nil && !equalReasons(got.Reasons, tt.want) {
				t.Errorf("NormalizeDNSName(%q).Reasons = %v, want %v", tt.raw, got.Reasons, tt.want)
			}
			if got.Original != tt.raw {
				t.Errorf("NormalizeDNSName(%q).Original = %q, want the raw value untouched", tt.raw, got.Original)
			}
		})
	}
}

func TestNormalizeNetworkNameMatchesLoaderRules(t *testing.T) {
	// The compose loader lowercases, turns underscores into hyphens and
	// drops characters outside [A-Za-z0-9.-].
	got := NormalizeNetworkName("My_Net!")
	if got.Name != "my-net" {
		t.Errorf("NormalizeNetworkName().Name = %q, want %q", got.Name, "my-net")
	}
}

func TestNormalizeServiceDNSNameMatchesLoaderRules(t *testing.T) {
	// The compose loader lowercases service names and turns dots and
	// underscores into hyphens.
	got := NormalizeServiceDNSName("My.Cool_Service")
	if got.Name != "my-cool-service" {
		t.Errorf("NormalizeServiceDNSName().Name = %q, want %q", got.Name, "my-cool-service")
	}
}

func equalReasons(got, want []Reason) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
