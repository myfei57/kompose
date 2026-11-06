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

package portalloc

import (
	"reflect"
	"testing"
)

func mustParse(t *testing.T, specs ...string) []MappingSpec {
	t.Helper()
	var out []MappingSpec
	for _, spec := range specs {
		parsed, err := ParseMapping(spec)
		if err != nil {
			t.Fatalf("ParseMapping(%q) returned unexpected error: %v", spec, err)
		}
		out = append(out, parsed...)
	}
	return out
}

func mustParseOne(t *testing.T, spec string) MappingSpec {
	t.Helper()
	parsed := mustParse(t, spec)
	if len(parsed) != 1 {
		t.Fatalf("ParseMapping(%q) expanded to %d mappings, want 1", spec, len(parsed))
	}
	return parsed[0]
}

func service(name string, mappings ...MappingSpec) ServiceSpec {
	return ServiceSpec{Name: name, Mappings: mappings}
}

func refs(service string, indexes ...int) []Ref {
	refs := make([]Ref, len(indexes))
	for i, idx := range indexes {
		refs[i] = Ref{Service: service, Index: idx}
	}
	return refs
}

func conflictKinds(plan *Plan) []ConflictKind {
	kinds := make([]ConflictKind, len(plan.Conflicts))
	for i, c := range plan.Conflicts {
		kinds[i] = c.Kind
	}
	return kinds
}

func mergeKinds(plan *Plan) []MergeKind {
	kinds := make([]MergeKind, len(plan.Merges))
	for i, m := range plan.Merges {
		kinds[i] = m.Kind
	}
	return kinds
}

func TestParseMapping(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []MappingSpec
	}{
		{
			name: "container port only",
			spec: "80",
			want: []MappingSpec{{Published: 0, Target: 80}},
		},
		{
			name: "host and container with protocol",
			spec: "8080:80/tcp",
			want: []MappingSpec{{Published: 8080, Target: 80, Protocol: "tcp"}},
		},
		{
			name: "ipv4 bind address",
			spec: "127.0.0.1:8080:80",
			want: []MappingSpec{{HostIP: "127.0.0.1", Published: 8080, Target: 80}},
		},
		{
			name: "ipv4 bind without host port",
			spec: "127.0.0.1::80",
			want: []MappingSpec{{HostIP: "127.0.0.1", Published: 0, Target: 80}},
		},
		{
			name: "bracketed ipv6",
			spec: "[::1]:8080:80/tcp",
			want: []MappingSpec{{HostIP: "::1", Published: 8080, Target: 80, Protocol: "tcp"}},
		},
		{
			name: "bracketed ipv6 wildcard",
			spec: "[::]:8080:80",
			want: []MappingSpec{{HostIP: "::", Published: 8080, Target: 80}},
		},
		{
			name: "bracketed ipv6 without host port",
			spec: "[::1]::80",
			want: []MappingSpec{{HostIP: "::1", Published: 0, Target: 80}},
		},
		{
			name: "container range",
			spec: "3000-3002",
			want: []MappingSpec{
				{Target: 3000}, {Target: 3001}, {Target: 3002},
			},
		},
		{
			name: "host and container ranges",
			spec: "3000-3002:5000-5002/udp",
			want: []MappingSpec{
				{Published: 3000, Target: 5000, Protocol: "udp"},
				{Published: 3001, Target: 5001, Protocol: "udp"},
				{Published: 3002, Target: 5002, Protocol: "udp"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMapping(tt.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseMapping(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestParseMappingErrors(t *testing.T) {
	for _, spec := range []string{
		"",
		"99999",
		"5000-3000:1-3",
		"1-2:3-5",
		"[::1",
		"80:80:80:80",
		"80:",
		"::1:80:80",
		"80/",
		"[::1]:80",
		"127.0.0.1:80",
	} {
		t.Run(spec, func(t *testing.T) {
			if _, err := ParseMapping(spec); err == nil {
				t.Errorf("ParseMapping(%q) expected error, got nil", spec)
			}
		})
	}
}

func TestAllocateBasicPlan(t *testing.T) {
	svc := service("api", mustParse(t, "8080:80/tcp", "53:53/udp")...)
	plan := Allocate([]ServiceSpec{svc})

	if plan.HasConflicts() {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}
	if len(plan.Services) != 1 {
		t.Fatalf("expected 1 service plan, got %d", len(plan.Services))
	}
	sp := plan.Services[0]
	if sp.Type != typeClusterIP || !reflect.DeepEqual(sp.Targets, []string{"api"}) {
		t.Errorf("unexpected type/targets: %q %v", sp.Type, sp.Targets)
	}
	wantPorts := []PlannedPort{
		{ServicePort: 8080, TargetPort: 80, Protocol: ProtocolTCP, Bindings: []string{""}, Members: refs("api", 0)},
		{ServicePort: 53, TargetPort: 53, Protocol: ProtocolUDP, Bindings: []string{""}, Members: refs("api", 1)},
	}
	if !reflect.DeepEqual(sp.Ports, wantPorts) {
		t.Errorf("planned ports = %+v, want %+v", sp.Ports, wantPorts)
	}
}

func TestDefaultedProtocolClassification(t *testing.T) {
	svc := service("web", mustParse(t, "80", "53/udp")...)
	plan := Allocate([]ServiceSpec{svc})

	if !reflect.DeepEqual(plan.DefaultedProtocol, refs("web", 0)) {
		t.Errorf("DefaultedProtocol = %+v, want [web#0]", plan.DefaultedProtocol)
	}
	// The omitted protocol becomes TCP on the plan.
	if plan.Services[0].Ports[0].Protocol != ProtocolTCP {
		t.Errorf("expected defaulted protocol TCP, got %q", plan.Services[0].Ports[0].Protocol)
	}
	// TCP and UDP on the same port coexist in a regular Service.
	if len(plan.Conflicts) != 0 {
		t.Errorf("expected no conflicts, got %+v", plan.Conflicts)
	}
}

func TestIPv6BindingClassification(t *testing.T) {
	svc := service("web",
		mustParseOne(t, "[::1]:8080:80"),
		mustParseOne(t, "[2001:db8::1]:8081:81"),
		mustParseOne(t, "127.0.0.1:8082:82"),
	)
	plan := Allocate([]ServiceSpec{svc})

	if !reflect.DeepEqual(plan.IPv6Bindings, refs("web", 0, 1)) {
		t.Errorf("IPv6Bindings = %+v, want [web#0 web#1]", plan.IPv6Bindings)
	}
	gotBindings := []string{
		plan.Services[0].Ports[0].Bindings[0],
		plan.Services[0].Ports[1].Bindings[0],
	}
	if !reflect.DeepEqual(gotBindings, []string{"::1", "2001:db8::1"}) {
		t.Errorf("canonical ipv6 bindings = %v", gotBindings)
	}
}

func TestDuplicateDeclarationsClassification(t *testing.T) {
	svc := service("web", mustParse(t, "80/tcp", "80")...)
	plan := Allocate([]ServiceSpec{svc})

	// The later, protocol-defaulted copy is both defaulted and duplicate.
	if !reflect.DeepEqual(plan.DuplicateDeclarations, refs("web", 1)) {
		t.Errorf("DuplicateDeclarations = %+v, want [web#1]", plan.DuplicateDeclarations)
	}
	if !reflect.DeepEqual(plan.DefaultedProtocol, refs("web", 1)) {
		t.Errorf("DefaultedProtocol = %+v, want [web#1]", plan.DefaultedProtocol)
	}
	if len(plan.Merges) != 1 || plan.Merges[0].Kind != MergeExactDuplicate {
		t.Errorf("expected one exact-duplicate merge, got %+v", plan.Merges)
	}
	if !reflect.DeepEqual(plan.Merges[0].Refs, refs("web", 0, 1)) {
		t.Errorf("duplicate merge refs = %+v", plan.Merges[0].Refs)
	}
}

func TestHostTargetMismatchConflict(t *testing.T) {
	svc := service("web", mustParse(t, "80:8080", "80:9090")...)
	plan := Allocate([]ServiceSpec{svc})

	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != ConflictHostTargetMismatch {
		t.Fatalf("expected one host-target-mismatch conflict, got %+v", plan.Conflicts)
	}
	if !reflect.DeepEqual(plan.Conflicts[0].Refs, refs("web", 0, 1)) {
		t.Errorf("conflict refs = %+v", plan.Conflicts[0].Refs)
	}
	port := plan.Services[0].Ports[0]
	if !port.Conflicting || port.ServicePort != 80 {
		t.Errorf("planned port should remain and be marked conflicting: %+v", port)
	}
}

func TestMergeBindAddressIgnored(t *testing.T) {
	svc := service("web", mustParse(t, "127.0.0.1:80:80", "127.0.0.2:80:80")...)
	plan := Allocate([]ServiceSpec{svc})

	if len(plan.Merges) != 1 || plan.Merges[0].Kind != MergeAddressBindingIgnored {
		t.Fatalf("expected address-binding-ignored merge, got %+v", plan.Merges)
	}
	if !reflect.DeepEqual(plan.Services[0].Ports[0].Bindings, []string{"127.0.0.1", "127.0.0.2"}) {
		t.Errorf("bindings = %v", plan.Services[0].Ports[0].Bindings)
	}
}

func TestMergeWildcardAndSpecificBindingAreExactDuplicate(t *testing.T) {
	svc := service("web", mustParse(t, "[::]:80:80", "0.0.0.0:80:80")...)
	plan := Allocate([]ServiceSpec{svc})

	// Unspecified IPv6 and IPv4 wildcards canonicalize to the same binding.
	if !reflect.DeepEqual(plan.IPv6Bindings, refs("web", 0)) {
		t.Errorf("IPv6Bindings = %+v", plan.IPv6Bindings)
	}
	if len(plan.Merges) != 1 || plan.Merges[0].Kind != MergeExactDuplicate {
		t.Fatalf("expected exact-duplicate merge, got %+v", plan.Merges)
	}
	if !reflect.DeepEqual(plan.DuplicateDeclarations, refs("web", 1)) {
		t.Errorf("DuplicateDeclarations = %+v", plan.DuplicateDeclarations)
	}
}

func TestMergePublishedCoalesced(t *testing.T) {
	svc := service("web", mustParse(t, "80", "80:80")...)
	plan := Allocate([]ServiceSpec{svc})

	if len(plan.Merges) != 1 || plan.Merges[0].Kind != MergePublishedCoalesced {
		t.Fatalf("expected published-coalesced merge, got %+v", plan.Merges)
	}
	if len(plan.Services[0].Ports) != 1 || plan.Services[0].Ports[0].ServicePort != 80 {
		t.Errorf("expected a single Service port 80, got %+v", plan.Services[0].Ports)
	}
}

func TestMergeContainerPortDedup(t *testing.T) {
	svc := service("web", mustParse(t, "80", "8080:80")...)
	plan := Allocate([]ServiceSpec{svc})

	if len(plan.Services[0].Ports) != 2 {
		t.Fatalf("expected two Service ports, got %+v", plan.Services[0].Ports)
	}
	if len(plan.Merges) != 1 || plan.Merges[0].Kind != MergeContainerPortDedup {
		t.Fatalf("expected container-port-dedup merge, got %+v", plan.Merges)
	}
	if !reflect.DeepEqual(plan.Merges[0].Refs, refs("web", 0, 1)) {
		t.Errorf("dedup refs = %+v", plan.Merges[0].Refs)
	}
}

func TestLoadBalancerSplitsProtocolsAndSkipsIngress(t *testing.T) {
	svc := service("web", mustParse(t, "80", "53/udp")...)
	svc.Type = "LoadBalancer"
	svc.ExposeService = "example.com"
	plan := Allocate([]ServiceSpec{svc})

	sp := plan.Services[0]
	if !reflect.DeepEqual(sp.Targets, []string{"web-tcp", "web-udp"}) {
		t.Errorf("targets = %v", sp.Targets)
	}
	if sp.Ingress != nil {
		t.Errorf("LoadBalancer services must not get an Ingress plan, got %+v", sp.Ingress)
	}
	if len(plan.Conflicts) != 0 {
		t.Errorf("mixed protocols coexist through split Services: %+v", plan.Conflicts)
	}
}

func TestNodePortReuseConflict(t *testing.T) {
	svc := service("web", mustParse(t, "80", "81")...)
	svc.Type = "NodePort"
	svc.NodePort = 30000
	plan := Allocate([]ServiceSpec{svc})

	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != ConflictNodePortReuse {
		t.Fatalf("expected node-port-reuse conflict, got %+v", plan.Conflicts)
	}
	for _, p := range plan.Services[0].Ports {
		if p.NodePort != 30000 {
			t.Errorf("planned port %d missing fixed node port", p.ServicePort)
		}
	}
}

func TestNodePortCollisionAcrossServices(t *testing.T) {
	a := service("a", mustParseOne(t, "80"))
	a.Type = "NodePort"
	a.NodePort = 30000
	b := service("b", mustParseOne(t, "80"))
	b.Type = "NodePort"
	b.NodePort = 30000
	plan := Allocate([]ServiceSpec{a, b})

	var collision *Conflict
	for i := range plan.Conflicts {
		if plan.Conflicts[i].Kind == ConflictNodePortCollision {
			collision = &plan.Conflicts[i]
		}
	}
	if collision == nil {
		t.Fatalf("expected node-port-collision conflict, got %+v", plan.Conflicts)
	}
	if !reflect.DeepEqual(collision.Refs, []Ref{{Service: "a", Index: 0}, {Service: "b", Index: 0}}) {
		t.Errorf("collision refs = %+v", collision.Refs)
	}
}

func TestIngressPlan(t *testing.T) {
	svc := service("web", mustParseOne(t, "8080:80"))
	svc.ExposeService = "example.com/api, true"
	plan := Allocate([]ServiceSpec{svc})

	ingress := plan.Services[0].Ingress
	if ingress == nil {
		t.Fatal("expected an Ingress plan")
	}
	if ingress.Backend != "web" || ingress.ServicePort != 8080 || ingress.Conflicting {
		t.Errorf("unexpected ingress: %+v", ingress)
	}
	wantRoutes := []IngressRoute{
		{Host: "example.com", Path: "/api"},
		{Host: "", Path: "/"},
	}
	if !reflect.DeepEqual(ingress.Routes, wantRoutes) {
		t.Errorf("routes = %+v, want %+v", ingress.Routes, wantRoutes)
	}
}

func TestIngressUDPConflict(t *testing.T) {
	svc := service("dns", mustParseOne(t, "53:53/udp"))
	svc.ExposeService = "dns.example.com"
	plan := Allocate([]ServiceSpec{svc})

	if plan.Services[0].Ingress == nil || !plan.Services[0].Ingress.Conflicting {
		t.Fatal("expected conflicting ingress plan")
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != ConflictIngressUnsupported {
		t.Fatalf("expected ingress-unsupported conflict, got %+v", plan.Conflicts)
	}
}

func TestIngressRouteCollisionAcrossServices(t *testing.T) {
	a := service("a", mustParseOne(t, "80"))
	a.ExposeService = "example.com/web"
	b := service("b", mustParseOne(t, "80"))
	b.ExposeService = "example.com/web"
	c := service("c", mustParseOne(t, "80"))
	c.ExposeService = "example.com/api"
	plan := Allocate([]ServiceSpec{a, b, c})

	var collision *Conflict
	for i := range plan.Conflicts {
		if plan.Conflicts[i].Kind == ConflictIngressRoute {
			collision = &plan.Conflicts[i]
		}
	}
	if collision == nil {
		t.Fatalf("expected ingress route collision, got %+v", plan.Conflicts)
	}
	if !reflect.DeepEqual(collision.Refs, []Ref{{Service: "a", Index: 0}, {Service: "b", Index: 0}}) {
		t.Errorf("collision refs = %+v", collision.Refs)
	}
}

func TestHostPortCollisionAcrossServices(t *testing.T) {
	tests := []struct {
		name         string
		a            string
		b            string
		wantConflict bool
	}{
		{"wildcard overlaps specific", "80:80", "127.0.0.1:80:80", true},
		{"same specific address", "127.0.0.1:80:80", "127.0.0.1:80:80", true},
		{"distinct specific addresses", "127.0.0.1:80:80", "127.0.0.2:80:80", false},
		{"different protocols", "80:80/tcp", "80:80/udp", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := service("a", mustParseOne(t, tt.a))
			a.ExposeContainerToHost = true
			b := service("b", mustParseOne(t, tt.b))
			b.ExposeContainerToHost = true
			plan := Allocate([]ServiceSpec{a, b})

			found := false
			for _, c := range plan.Conflicts {
				if c.Kind == ConflictHostPortCollision {
					found = true
				}
			}
			if found != tt.wantConflict {
				t.Errorf("host-port conflict = %v, want %v (conflicts: %+v)", found, tt.wantConflict, plan.Conflicts)
			}
		})
	}
}

func TestInvalidMappings(t *testing.T) {
	svc := service("web",
		MappingSpec{Target: 80, Protocol: "sctp"},
		MappingSpec{Target: 70000},
		MappingSpec{Published: 80, Target: 80, HostIP: "not-an-ip"},
		mustParseOne(t, "81"),
	)
	plan := Allocate([]ServiceSpec{svc})

	kinds := conflictKinds(plan)
	wantKinds := []ConflictKind{
		ConflictInvalidMapping,
		ConflictInvalidMapping,
		ConflictInvalidMapping,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("conflict kinds = %v, want %v", kinds, wantKinds)
	}
	// Invalid declarations keep their declaration index; only the valid
	// one reaches the plan.
	if len(plan.Services[0].Ports) != 1 || plan.Services[0].Ports[0].ServicePort != 81 {
		t.Errorf("only the valid mapping should be planned, got %+v", plan.Services[0].Ports)
	}
}

func TestResultStableByServiceOrder(t *testing.T) {
	services := []ServiceSpec{
		service("zeta", mustParseOne(t, "80:80")),
		service("alpha", mustParseOne(t, "80:80")),
	}
	first := Allocate(services)
	second := Allocate(services)

	// Repeated allocations are identical (no map iteration leaks through).
	if !reflect.DeepEqual(first, second) {
		t.Error("allocation is not deterministic")
	}
	// Input service order is preserved rather than sorted.
	if first.Services[0].Name != "zeta" || first.Services[1].Name != "alpha" {
		t.Errorf("service order = %s, %s; want input order preserved", first.Services[0].Name, first.Services[1].Name)
	}
}

func TestDoesNotMutateInput(t *testing.T) {
	original := service("web", mustParseOne(t, "127.0.0.1:80:80"))
	snapshot := append([]MappingSpec(nil), original.Mappings...)
	_ = Allocate([]ServiceSpec{original})
	if !reflect.DeepEqual(original.Mappings, snapshot) {
		t.Error("Allocate mutated the input mappings")
	}
}
