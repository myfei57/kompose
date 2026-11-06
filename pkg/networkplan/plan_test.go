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
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestBuildCrossNetworkConflict(t *testing.T) {
	plan := Build(Input{
		Networks: map[string]Network{
			"front": {Key: "front"},
			"back":  {Key: "back"},
		},
		Services: map[string]Service{
			// A service reachable under its own name on both networks must
			// not be reported as a conflict: it maps to one Service.
			"gateway": {
				Name:        "gateway",
				Attachments: map[string]Attachment{"front": {}, "back": {}},
			},
			"appA": {
				Name:        "appA",
				Attachments: map[string]Attachment{"front": {Aliases: []string{"SharedDB", "web"}}},
			},
			"appB": {
				Name:        "appB",
				Attachments: map[string]Attachment{"back": {Aliases: []string{"shareddb"}}},
			},
		},
	})

	if !plan.HasBlockingIssues() {
		t.Fatal("expected blocking issues for the cross-network conflict")
	}

	if len(plan.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	conflict := plan.Conflicts[0]
	if conflict.Canonical != "shareddb" {
		t.Errorf("conflict canonical = %q, want %q", conflict.Canonical, "shareddb")
	}
	if conflict.Scope != ConflictScopeCrossNetwork {
		t.Errorf("conflict scope = %q, want %q", conflict.Scope, ConflictScopeCrossNetwork)
	}
	if !reflect.DeepEqual(conflict.Networks, []string{"back", "front"}) {
		t.Errorf("conflict networks = %v, want [back front]", conflict.Networks)
	}
	if len(conflict.Claims) != 2 {
		t.Fatalf("expected 2 claims, got %d: %+v", len(conflict.Claims), conflict.Claims)
	}
	if conflict.Claims[0] != (Claim{Service: "appB", Network: "back", Source: ScopeAlias, Current: "shareddb"}) {
		t.Errorf("unexpected first claim: %+v", conflict.Claims[0])
	}
	if conflict.Claims[1] != (Claim{Service: "appA", Network: "front", Source: ScopeAlias, Current: "SharedDB"}) {
		t.Errorf("unexpected second claim: %+v", conflict.Claims[1])
	}

	// The uppercase alias must also be surfaced as a rename.
	rename := findRename(t, plan, "services.appA.networks.front.aliases[0]")
	if rename.Proposed != "shareddb" || !hasReason(rename.Reasons, ReasonUppercase) {
		t.Errorf("unexpected alias rename: %+v", rename)
	}
}

func TestBuildSameNetworkConflict(t *testing.T) {
	plan := Build(Input{
		Networks: map[string]Network{"front": {Key: "front"}},
		Services: map[string]Service{
			"c1": {Name: "c1", Attachments: map[string]Attachment{"front": {Aliases: []string{"cache"}}}},
			"c2": {Name: "c2", Attachments: map[string]Attachment{"front": {Aliases: []string{"Cache"}}}},
		},
	})

	if len(plan.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %+v", plan.Conflicts)
	}
	if plan.Conflicts[0].Scope != ConflictScopeSameNetwork {
		t.Errorf("scope = %q, want %q", plan.Conflicts[0].Scope, ConflictScopeSameNetwork)
	}
	if plan.Conflicts[0].Canonical != "cache" {
		t.Errorf("canonical = %q, want cache", plan.Conflicts[0].Canonical)
	}
}

func TestBuildReferencesAndEdgeCases(t *testing.T) {
	plan := Build(Input{
		Networks: map[string]Network{
			"Front": {Key: "Front"},
			"back":  {Key: "back"},
		},
		Services: map[string]Service{
			"web": {
				Name:        "web",
				Hostname:    "WebHost",
				DomainName:  "Example.COM.",
				NetworkMode: "service:Old_Svc",
				Links:       []string{"user_svc", "ghost:LocalAlias"},
				Attachments: map[string]Attachment{
					"Front": {Aliases: []string{"DB.", "", "-"}},
				},
			},
			"user_svc": {
				Name:        "user_svc",
				Attachments: map[string]Attachment{"Front": {}},
			},
			"Old_Svc": {
				Name:        "Old_Svc",
				Attachments: map[string]Attachment{"Front": {}},
			},
			"lonely": {
				Name:        "lonely",
				Attachments: map[string]Attachment{"back": {}},
				Links:       []string{"web"},
			},
		},
	})

	// Network name unification: "Front" -> "front".
	networkRename := findRename(t, plan, "networks.Front")
	if networkRename.Proposed != "front" || !hasReason(networkRename.Reasons, ReasonUppercase) {
		t.Errorf("unexpected network rename: %+v", networkRename)
	}

	// Service attachment references the network key, which differs from
	// the canonical Kubernetes network name.
	attachmentFix := findFix(t, plan, "services.web.networks.Front")
	if attachmentFix.Current != "Front" || attachmentFix.Proposed != "front" || !attachmentFix.Resolved {
		t.Errorf("unexpected attachment fix: %+v", attachmentFix)
	}

	// Trailing dot alias, empty alias and unreparable alias.
	aliasRename := findRename(t, plan, "services.web.networks.Front.aliases[0]")
	if aliasRename.Proposed != "db" || !hasReason(aliasRename.Reasons, ReasonTrailingDot) {
		t.Errorf("unexpected trailing-dot alias rename: %+v", aliasRename)
	}
	emptyDropped := findDropped(t, plan, "services.web.networks.Front.aliases[1]")
	if emptyDropped.Current != "" || !hasReason(emptyDropped.Reasons, ReasonEmptyAlias) {
		t.Errorf("empty alias was not dropped as such: %+v", emptyDropped)
	}
	hyphenDropped := findDropped(t, plan, "services.web.networks.Front.aliases[2]")
	if !hasReason(hyphenDropped.Reasons, ReasonNormalizedToEmpty) {
		t.Errorf("'-' alias was not dropped as normalized-to-empty: %+v", hyphenDropped)
	}

	// Link target normalization, unresolved target and link local alias.
	linkTarget := findFixByKind(t, plan, RefLinkTarget, "services.web.links[0]")
	if linkTarget.Current != "user_svc" || linkTarget.Proposed != "user-svc" || !linkTarget.Resolved {
		t.Errorf("unexpected link target fix: %+v", linkTarget)
	}
	linkGhost := findFixByKind(t, plan, RefLinkTarget, "services.web.links[1]")
	if linkGhost.Resolved || !hasReason(linkGhost.Reasons, ReasonUnresolvedTarget) {
		t.Errorf("unresolved link target was not reported: %+v", linkGhost)
	}
	linkAlias := findFixByKind(t, plan, RefLinkAlias, "services.web.links[1]")
	if linkAlias.Current != "LocalAlias" || linkAlias.Proposed != "localalias" {
		t.Errorf("unexpected link alias fix: %+v", linkAlias)
	}

	// network_mode: service:<name>.
	networkMode := findFix(t, plan, "services.web.network_mode")
	if networkMode.Current != "Old_Svc" || networkMode.Proposed != "old-svc" {
		t.Errorf("unexpected network_mode fix: %+v", networkMode)
	}

	// Hostname (single label) and domainname (subdomain).
	hostname := findRename(t, plan, "services.web.hostname")
	if hostname.Proposed != "webhost" {
		t.Errorf("unexpected hostname rename: %+v", hostname)
	}
	domainname := findRename(t, plan, "services.web.domainname")
	if domainname.Proposed != "example.com" {
		t.Errorf("unexpected domainname rename: %+v", domainname)
	}

	// Link to a service on another network only.
	isolatedLink := findFix(t, plan, "services.lonely.links[0]")
	if !hasReason(isolatedLink.Reasons, ReasonNoSharedNetwork) {
		t.Errorf("link without shared network was not reported: %+v", isolatedLink)
	}
}

func TestBuildUnresolvedNetworkAttachment(t *testing.T) {
	plan := Build(Input{
		Networks: map[string]Network{},
		Services: map[string]Service{
			"web": {
				Name:        "web",
				Attachments: map[string]Attachment{"ghostnet": {Aliases: []string{"web"}}},
			},
		},
	})

	fix := findFix(t, plan, "services.web.networks.ghostnet")
	if fix.Resolved || fix.Proposed != "" || !hasReason(fix.Reasons, ReasonUnresolvedTarget) {
		t.Errorf("unexpected unresolved network fix: %+v", fix)
	}
	// Claims on unknown networks must not produce phantom conflicts.
	if len(plan.Conflicts) != 0 {
		t.Errorf("expected no conflicts, got %+v", plan.Conflicts)
	}
}

func TestBuildCleanModel(t *testing.T) {
	plan := Build(Input{
		Networks: map[string]Network{"front": {Key: "front"}},
		Services: map[string]Service{
			"web": {
				Name:        "web",
				Hostname:    "web",
				DomainName:  "web.local",
				Attachments: map[string]Attachment{"front": {Aliases: []string{"www"}}},
			},
		},
	})
	if plan.HasChanges() {
		t.Errorf("expected an empty plan for a clean model, got %+v", plan)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	input := Input{
		Networks: map[string]Network{"front": {Key: "front"}, "back": {Key: "back"}},
		Services: map[string]Service{
			"appA": {Name: "appA", Attachments: map[string]Attachment{"front": {Aliases: []string{"SharedDB"}}}},
			"appB": {Name: "appB", Attachments: map[string]Attachment{"back": {Aliases: []string{"shareddb"}}}},
		},
	}
	first := Build(input)
	second := Build(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated builds of the same input returned different plans")
	}
}

func TestBuildDoesNotMutateInput(t *testing.T) {
	input := Input{
		Networks: map[string]Network{"Front": {Key: "Front"}},
		Services: map[string]Service{
			"web": {
				Name:        "web",
				Attachments: map[string]Attachment{"Front": {Aliases: []string{"DB.", "", "  "}}},
				Links:       []string{"WEB"},
			},
		},
	}

	_ = Build(input)

	if got := input.Networks["Front"].Key; got != "Front" {
		t.Errorf("network key mutated: %q", got)
	}
	aliases := input.Services["web"].Attachments["Front"].Aliases
	if !reflect.DeepEqual(aliases, []string{"DB.", "", "  "}) {
		t.Errorf("aliases mutated: %q", aliases)
	}
	if links := input.Services["web"].Links; !reflect.DeepEqual(links, []string{"WEB"}) {
		t.Errorf("links mutated: %q", links)
	}
}

func TestBuildFromComposeProject(t *testing.T) {
	project := &types.Project{
		Networks: types.Networks{
			"default": {Name: ""},
			"Front":   {Name: ""},
		},
		Services: types.Services{
			"Web": {
				Name:     "Web",
				Hostname: "Web",
				Networks: map[string]*types.ServiceNetworkConfig{
					"Front": {Aliases: []string{"WebDB."}},
				},
				Links: []string{"DB_SVC:DB"},
			},
			"DB_SVC": {
				Name:     "DB_SVC",
				Networks: map[string]*types.ServiceNetworkConfig{"Front": {}},
			},
			"plain": {Name: "plain"},
		},
	}

	// Snapshot used to prove the project is not mutated.
	before := *project
	beforeServices := make(types.Services, len(project.Services))
	for k, v := range project.Services {
		beforeServices[k] = v
	}

	input := InputFromComposeProject(project)

	// Services without explicit networks attach to the default network.
	if _, ok := input.Services["plain"].Attachments["default"]; !ok {
		t.Errorf("expected the implicit default network attachment, got %+v", input.Services["plain"].Attachments)
	}

	plan := BuildFromComposeProject(project)

	if !reflect.DeepEqual(project.Networks, before.Networks) {
		t.Error("project networks were mutated")
	}
	if !reflect.DeepEqual(project.Services, beforeServices) {
		t.Error("project services were mutated")
	}
	if !reflect.DeepEqual(*project, before) {
		t.Error("the compose project was mutated by the plan")
	}

	if findRename(t, plan, "networks.Front").Proposed != "front" {
		t.Error("network rename missing")
	}
	if findRename(t, plan, "services.Web").Proposed != "web" {
		t.Error("service rename missing")
	}
	if findRename(t, plan, "services.Web.networks.Front.aliases[0]").Proposed != "webdb" {
		t.Error("alias rename missing")
	}
	link := findFixByKind(t, plan, RefLinkTarget, "services.Web.links[0]")
	if link.Proposed != "db-svc" {
		t.Errorf("link target = %q, want db-svc", link.Proposed)
	}
	linkAlias := findFixByKind(t, plan, RefLinkAlias, "services.Web.links[0]")
	if linkAlias.Proposed != "db" {
		t.Errorf("link alias = %q, want db", linkAlias.Proposed)
	}
}

func findRename(t *testing.T, plan Plan, field string) Rename {
	t.Helper()
	for _, rename := range plan.Renames {
		if rename.Field == field {
			return rename
		}
	}
	t.Fatalf("rename for field %q not found; renames: %+v", field, plan.Renames)
	return Rename{}
}

func findFix(t *testing.T, plan Plan, field string) ReferenceFix {
	t.Helper()
	for _, fix := range plan.ReferenceFixes {
		if fix.Field == field {
			return fix
		}
	}
	t.Fatalf("reference fix for field %q not found; fixes: %+v", field, plan.ReferenceFixes)
	return ReferenceFix{}
}

func findFixByKind(t *testing.T, plan Plan, kind ReferenceKind, field string) ReferenceFix {
	t.Helper()
	for _, fix := range plan.ReferenceFixes {
		if fix.Kind == kind && fix.Field == field {
			return fix
		}
	}
	t.Fatalf("%s fix for field %q not found; fixes: %+v", kind, field, plan.ReferenceFixes)
	return ReferenceFix{}
}

func findDropped(t *testing.T, plan Plan, field string) DroppedName {
	t.Helper()
	for _, dropped := range plan.Dropped {
		if dropped.Field == field {
			return dropped
		}
	}
	t.Fatalf("dropped entry for field %q not found; dropped: %+v", field, plan.Dropped)
	return DroppedName{}
}

func hasReason(reasons []Reason, want Reason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
