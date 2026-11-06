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

// Package portalloc is a read-only planner for Compose service port
// mappings. Given the declared host/container ports, protocols and host
// address bindings of Compose services, it computes the Kubernetes
// Service/Ingress layout that a conversion would produce and reports:
//
//   - mappings that cannot coexist in the resulting layout (Conflicts),
//   - mappings that collapse onto a single Service or container port and
//     can therefore be safely merged (Merges),
//   - mappings whose protocol was omitted and defaulted to TCP
//     (DefaultedProtocol),
//   - mappings bound to an IPv6 address (IPv6Bindings),
//   - mappings declared more than once (DuplicateDeclarations).
//
// The planner never mutates its inputs, performs no I/O and does not
// contact the network or the Kubernetes API. Results are deterministic:
// services keep the order of the input slice and every other slice is
// ordered by service order followed by declaration index.
package portalloc

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// Protocol names used in the plan, matching the Kubernetes API conventions.
const (
	ProtocolTCP = "TCP"
	ProtocolUDP = "UDP"
)

// Kubernetes Service types recognized by the planner. An empty Type on a
// ServiceSpec is treated as ClusterIP, mirroring kompose defaults.
const (
	typeClusterIP    = "ClusterIP"
	typeNodePort     = "NodePort"
	typeLoadBalancer = "LoadBalancer"
	typeHeadless     = "Headless"
)

// MappingSpec is one Compose port mapping as declared, before any
// normalization. Use ParseMapping to build values from the Compose short
// syntax ("[host_ip:]host_port:container_port[/protocol]").
type MappingSpec struct {
	// HostIP is the bind address as declared. The empty string means that
	// no address was given (all interfaces). IPv6 literals may be passed
	// either with ("[::1]") or without ("::1") brackets.
	HostIP string
	// Published is the host (published) port. Zero means it was omitted;
	// the target Service port then defaults to the container port.
	Published int32
	// Target is the container port the traffic is forwarded to.
	Target int32
	// Protocol is the protocol as declared. The empty string means it was
	// omitted and is defaulted to TCP at allocation time.
	Protocol string
}

// ServiceSpec is the read-only input description of one Compose service.
type ServiceSpec struct {
	// Name is the Compose service name.
	Name string
	// Type is the target Kubernetes Service type (kompose.service.type).
	// Unknown values are kept verbatim in the plan but behave like
	// ClusterIP for conflict detection.
	Type string
	// NodePort is the fixed node port requested through
	// kompose.service.nodeport.port. Zero means Kubernetes allocates one.
	NodePort int32
	// ExposeContainerToHost mirrors kompose.controller.port.expose, which
	// publishes ports on the node through the container hostPort field.
	ExposeContainerToHost bool
	// ExposeService mirrors kompose.service.expose: a comma separated list
	// of host[/path] entries. Non-empty values produce an Ingress plan for
	// non-LoadBalancer services.
	ExposeService string
	// Mappings are the declared port mappings in declaration order.
	Mappings []MappingSpec
}

// Ref identifies a single MappingSpec inside the input: the service name
// plus the zero based declaration index within that service.
type Ref struct {
	Service string
	Index   int
}

// ConflictKind explains why a set of mappings cannot coexist.
type ConflictKind string

const (
	// ConflictInvalidMapping is reported for declarations that cannot be
	// planned at all (out of range ports, unknown protocol, malformed or
	// unparseable bind address).
	ConflictInvalidMapping ConflictKind = "invalid-mapping"
	// ConflictHostTargetMismatch is reported when the same Service port and
	// protocol are mapped to distinct container ports inside one service:
	// a Kubernetes Service port can only target a single container port.
	ConflictHostTargetMismatch ConflictKind = "host-target-mismatch"
	// ConflictIngressUnsupported is reported when an Ingress is requested
	// but the backing Service port speaks UDP (Ingress only carries
	// HTTP/TCP).
	ConflictIngressUnsupported ConflictKind = "ingress-unsupported-protocol"
	// ConflictNodePortReuse is reported when one fixed node port is applied
	// to several ports of the same NodePort Service (node ports are unique
	// within a Service).
	ConflictNodePortReuse ConflictKind = "node-port-reuse"
	// ConflictNodePortCollision is reported across services when two
	// NodePort Services request the same fixed node port.
	ConflictNodePortCollision ConflictKind = "node-port-collision"
	// ConflictHostPortCollision is reported across services that expose
	// container hostPorts and publish the same (bind address, port,
	// protocol) tuple, including wildcard bindings that overlap specific
	// addresses.
	ConflictHostPortCollision ConflictKind = "host-port-collision"
	// ConflictIngressRoute is reported across services whose Ingress rules
	// claim the same host and path for different backends.
	ConflictIngressRoute ConflictKind = "ingress-route-collision"
)

// MergeKind explains how several declarations collapse into one plan item.
type MergeKind string

const (
	// MergeExactDuplicate groups identical declarations repeated on a
	// service.
	MergeExactDuplicate MergeKind = "exact-duplicate"
	// MergeAddressBindingIgnored groups declarations that only differ by
	// their host bind address: a Kubernetes Service has no per-address
	// binding, so the address is dropped and the ports collapse.
	MergeAddressBindingIgnored MergeKind = "address-binding-ignored"
	// MergePublishedCoalesced groups an ephemeral/container-only
	// declaration with an explicit published declaration that resolve to
	// the same Service port.
	MergePublishedCoalesced MergeKind = "published-coalesced"
	// MergeContainerPortDedup groups declarations that reach the same
	// container port through distinct Service ports: the container only
	// declares the port once.
	MergeContainerPortDedup MergeKind = "container-port-dedup"
)

// Conflict describes mappings that cannot coexist in the computed plan.
// Service is empty for cluster-scoped conflicts spanning several services.
type Conflict struct {
	Kind    ConflictKind
	Service string
	Message string
	Refs    []Ref
}

// Merge describes declarations that collapse onto the same plan item. Kept
// is the representative declaration (always the first one declared); Refs
// lists every declaration that was merged, Kept included.
type Merge struct {
	Kind    MergeKind
	Service string
	Message string
	Kept    Ref
	Refs    []Ref
}

// PlannedPort is one port of the target Kubernetes Service.
type PlannedPort struct {
	// ServicePort is the Service port number (published port, or the
	// container port when nothing was published).
	ServicePort int32
	// TargetPort is the container port backing the Service port.
	TargetPort int32
	// Protocol is the normalized protocol (TCP/UDP).
	Protocol string
	// NodePort is the fixed node port, zero when Kubernetes allocates it.
	NodePort int32
	// Bindings lists the distinct normalized host bind addresses declared
	// for this port, in first-seen order. The empty string stands for all
	// interfaces. Bind addresses are not represented on Service objects.
	Bindings []string
	// Members references every declaration backing the port, in service
	// declaration order.
	Members []Ref
	// Conflicting marks a port whose declarations disagree (for example
	// several target ports); the port is kept in the plan for completeness.
	Conflicting bool
}

// IngressRoute is one host/path rule of an Ingress plan.
type IngressRoute struct {
	// Host is the rule host, empty when the rule matches every host.
	Host string
	// Path is the HTTP path, defaulting to "/".
	Path string
}

// IngressPlan is the Ingress object associated with a Service.
type IngressPlan struct {
	// Backend is the name of the backend Service.
	Backend string
	// ServicePort is the backend Service port the rule points at.
	ServicePort int32
	// Routes are the planned host/path rules in declaration order.
	Routes []IngressRoute
	// Conflicting marks an Ingress that cannot serve its backend (for
	// example a UDP port).
	Conflicting bool
}

// ServicePlan is the computed layout for one Compose service.
type ServicePlan struct {
	// Name is the Compose service name.
	Name string
	// Type is the normalized target Service type.
	Type string
	// Targets are the names of the Service objects that would be emitted.
	// LoadBalancer services are split per protocol into "<name>-tcp" and
	// "<name>-udp"; every other type yields at most "<name>". It is empty
	// for services without ports.
	Targets []string
	// Ports are the planned Service ports in declaration order.
	Ports []PlannedPort
	// Ingress is the optional Ingress plan. It is nil for services without
	// a kompose.service.expose label and for LoadBalancer services (kompose
	// only creates Ingress objects for regular Services).
	Ingress *IngressPlan
}

// Plan is the result of Allocate.
type Plan struct {
	// Services holds the per-service plans in input service order.
	Services []ServicePlan
	// Conflicts lists non-coexistent mappings. Per-service conflicts come
	// first (in service order), followed by deterministic cross-service
	// conflicts.
	Conflicts []Conflict
	// Merges lists declarations that can safely collapse, in service and
	// declaration order.
	Merges []Merge
	// DefaultedProtocol references declarations whose protocol was omitted
	// and normalized to TCP.
	DefaultedProtocol []Ref
	// IPv6Bindings references declarations bound to an IPv6 address.
	IPv6Bindings []Ref
	// DuplicateDeclarations references the later copies of declarations
	// already seen on the same service (the first declaration is never
	// listed).
	DuplicateDeclarations []Ref
}

// HasConflicts reports whether the plan contains any conflict.
func (p *Plan) HasConflicts() bool {
	return len(p.Conflicts) > 0
}

// nMapping is a normalized mapping with resolved protocol and address.
type nMapping struct {
	ref       Ref
	binding   string // canonical non-wildcard bind address
	wildcard  bool   // binding was "" / 0.0.0.0 / ::
	ipv6      bool
	published int32
	target    int32
	protocol  string
	defaulted bool
	invalid   string // non-empty reason when the declaration is invalid
}

type servicePortKey struct {
	port     int32
	protocol string
}

type binding struct {
	wildcard bool
	address  string
}

func (b binding) key() string {
	if b.wildcard {
		return "*"
	}
	return b.address
}

// Allocate computes the Service/Ingress plan for the given services.
// It is a pure, read-only transformation: it never mutates the input and
// performs no I/O. The order of services and of declarations is preserved
// in every output slice.
func Allocate(services []ServiceSpec) *Plan {
	plan := &Plan{}

	normalized := make([][]nMapping, len(services))
	for i, svc := range services {
		normalized[i] = normalizeService(svc, plan)
	}

	for i, svc := range services {
		sp, conflicts, merges := planService(svc, normalized[i])
		plan.Services = append(plan.Services, sp)
		plan.Conflicts = append(plan.Conflicts, conflicts...)
		plan.Merges = append(plan.Merges, merges...)
	}

	plan.Conflicts = append(plan.Conflicts, nodePortConflicts(services, plan)...)
	plan.Conflicts = append(plan.Conflicts, hostPortConflicts(services, normalized)...)
	plan.Conflicts = append(plan.Conflicts, ingressRouteConflicts(services, plan)...)

	return plan
}

// normalizeService normalizes every declaration of a service, records the
// protocol-default / IPv6 / duplicate classifications and returns the
// normalized declarations in declaration order. Invalid declarations are
// kept in the slice (with an invalid reason) so their index keeps matching
// the input declaration index.
func normalizeService(svc ServiceSpec, plan *Plan) []nMapping {
	out := make([]nMapping, 0, len(svc.Mappings))
	seen := map[string]bool{}

	for i, spec := range svc.Mappings {
		n := normalize(Ref{Service: svc.Name, Index: i}, spec)
		if n.defaulted {
			plan.DefaultedProtocol = append(plan.DefaultedProtocol, n.ref)
		}
		if n.ipv6 {
			plan.IPv6Bindings = append(plan.IPv6Bindings, n.ref)
		}
		if n.invalid != "" {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				Kind:    ConflictInvalidMapping,
				Service: svc.Name,
				Message: fmt.Sprintf("service %q port mapping #%d is invalid: %s", svc.Name, i, n.invalid),
				Refs:    []Ref{n.ref},
			})
			out = append(out, n)
			continue
		}

		identity := fmt.Sprintf("%s|%d|%d|%s", binding{wildcard: n.wildcard, address: n.binding}.key(), n.published, n.target, n.protocol)
		if seen[identity] {
			plan.DuplicateDeclarations = append(plan.DuplicateDeclarations, n.ref)
		}
		seen[identity] = true
		out = append(out, n)
	}
	return out
}

// normalize resolves the protocol default, validates ranges and canonical
// bind address of a single declaration.
func normalize(ref Ref, spec MappingSpec) nMapping {
	n := nMapping{
		ref:       ref,
		published: spec.Published,
		target:    spec.Target,
	}

	switch strings.ToUpper(strings.TrimSpace(spec.Protocol)) {
	case "":
		n.protocol = ProtocolTCP
		n.defaulted = true
	case ProtocolTCP, ProtocolUDP:
		n.protocol = strings.ToUpper(strings.TrimSpace(spec.Protocol))
	default:
		n.invalid = fmt.Sprintf("unsupported protocol %q, only tcp and udp are supported", spec.Protocol)
		return n
	}

	if spec.Target < 1 || spec.Target > 65535 {
		n.invalid = fmt.Sprintf("container port %d is out of range (1-65535)", spec.Target)
		return n
	}
	if spec.Published < 0 || spec.Published > 65535 {
		n.invalid = fmt.Sprintf("host port %d is out of range (1-65535)", spec.Published)
		return n
	}

	host := strings.TrimSpace(spec.HostIP)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		n.wildcard = true
		return n
	}
	ip := net.ParseIP(host)
	if ip == nil {
		n.invalid = fmt.Sprintf("bind address %q is not a valid IP address", spec.HostIP)
		return n
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else {
		n.ipv6 = true
	}
	if ip.IsUnspecified() {
		n.wildcard = true
		return n
	}
	n.binding = ip.String()
	return n
}

// servicePlanBuilder accumulates the plan of a single service.
type servicePlanBuilder struct {
	svc    ServiceSpec
	plan   ServicePlan
	groups map[servicePortKey][]nMapping
	order  []servicePortKey
	conf   []Conflict
}

func planService(svc ServiceSpec, mappings []nMapping) (ServicePlan, []Conflict, []Merge) {
	b := &servicePlanBuilder{
		svc:    svc,
		groups: map[servicePortKey][]nMapping{},
	}
	b.plan.Name = svc.Name
	b.plan.Type = normalizeServiceType(svc.Type)

	for _, n := range mappings {
		if n.invalid != "" {
			continue
		}
		key := servicePortKey{port: effectivePort(n), protocol: n.protocol}
		if _, ok := b.groups[key]; !ok {
			b.order = append(b.order, key)
		}
		b.groups[key] = append(b.groups[key], n)
	}

	for _, key := range b.order {
		members := b.groups[key]
		port := PlannedPort{
			ServicePort: key.port,
			TargetPort:  members[0].target,
			Protocol:    key.protocol,
		}
		if b.plan.Type == typeNodePort && svc.NodePort > 0 {
			port.NodePort = svc.NodePort
		}

		distinctTargets := map[int32]struct{}{}
		bindings := map[string]struct{}{}
		for _, m := range members {
			distinctTargets[m.target] = struct{}{}
			port.Members = append(port.Members, m.ref)
			bk := binding{wildcard: m.wildcard, address: m.binding}.key()
			if _, ok := bindings[bk]; !ok {
				bindings[bk] = struct{}{}
				if m.wildcard {
					port.Bindings = append(port.Bindings, "")
				} else {
					port.Bindings = append(port.Bindings, m.binding)
				}
			}
		}

		if len(distinctTargets) > 1 {
			port.Conflicting = true
			targets := make([]int32, 0, len(distinctTargets))
			for t := range distinctTargets {
				targets = append(targets, t)
			}
			sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
			b.conf = append(b.conf, Conflict{
				Kind:    ConflictHostTargetMismatch,
				Service: svc.Name,
				Message: fmt.Sprintf("service %q maps Service port %d/%s to multiple container ports %v; one Service port can only target a single container port", svc.Name, key.port, strings.ToLower(key.protocol), targets),
				Refs:    append([]Ref(nil), port.Members...),
			})
		}

		b.plan.Ports = append(b.plan.Ports, port)
	}

	if b.plan.Type == typeNodePort && svc.NodePort > 0 && len(b.plan.Ports) > 1 {
		refs := make([]Ref, 0, len(b.plan.Ports))
		for _, p := range b.plan.Ports {
			if len(p.Members) > 0 {
				refs = append(refs, p.Members[0])
			}
		}
		b.conf = append(b.conf, Conflict{
			Kind:    ConflictNodePortReuse,
			Service: svc.Name,
			Message: fmt.Sprintf("service %q assigns fixed node port %d to %d Service ports; node ports must be unique within a Service", svc.Name, svc.NodePort, len(b.plan.Ports)),
			Refs:    refs,
		})
	}

	b.plan.Targets = plannedTargets(b.plan.Name, b.plan.Type, b.plan.Ports)
	b.plan.Ingress = b.planIngress()
	return b.plan, b.conf, b.merges()
}

func (b *servicePlanBuilder) planIngress() *IngressPlan {
	if b.plan.Type == typeLoadBalancer || b.svc.ExposeService == "" || len(b.plan.Ports) == 0 {
		return nil
	}
	first := b.plan.Ports[0]
	ingress := &IngressPlan{
		Backend:     b.svc.Name,
		ServicePort: first.ServicePort,
		Routes:      parseIngressRoutes(b.svc.ExposeService),
	}
	if first.Protocol == ProtocolUDP {
		ingress.Conflicting = true
		b.conf = append(b.conf, Conflict{
			Kind:    ConflictIngressUnsupported,
			Service: b.svc.Name,
			Message: fmt.Sprintf("service %q requests an Ingress but its first Service port %d uses UDP; Ingress only supports HTTP over TCP", b.svc.Name, first.ServicePort),
			Refs:    []Ref{first.Members[0]},
		})
	}
	return ingress
}

func (b *servicePlanBuilder) merges() []Merge {
	var merges []Merge

	// Merges between declarations collapsing onto the same Service port.
	for _, key := range b.order {
		members := b.groups[key]
		if len(members) < 2 {
			continue
		}
		kind := MergeExactDuplicate
		publishZero, publishSet := false, map[int32]bool{}
		bindings := map[string]bool{}
		for _, m := range members {
			if m.published == 0 {
				publishZero = true
			} else {
				publishSet[m.published] = true
			}
			bindings[binding{wildcard: m.wildcard, address: m.binding}.key()] = true
		}
		switch {
		case publishZero && len(publishSet) > 0:
			kind = MergePublishedCoalesced
		case len(bindings) > 1:
			kind = MergeAddressBindingIgnored
		}

		refs := make([]Ref, 0, len(members))
		for _, m := range members {
			refs = append(refs, m.ref)
		}
		merges = append(merges, Merge{
			Kind:    kind,
			Service: b.svc.Name,
			Kept:    refs[0],
			Refs:    refs,
			Message: mergeMessage(b.svc.Name, kind, key),
		})
	}

	// Merges of container ports reached from distinct Service ports. One
	// representative per Service port is recorded so the merge does not
	// overlap with the within-group duplicates above.
	byTarget := map[servicePortKey][]Ref{}
	targetOrder := []servicePortKey{}
	seenTarget := map[servicePortKey]bool{}
	for _, key := range b.order {
		m := b.groups[key][0]
		tk := servicePortKey{port: m.target, protocol: m.protocol}
		byTarget[tk] = append(byTarget[tk], m.ref)
		if !seenTarget[tk] {
			seenTarget[tk] = true
			targetOrder = append(targetOrder, tk)
		}
	}
	for _, tk := range targetOrder {
		refs := byTarget[tk]
		if len(refs) < 2 {
			continue
		}
		merges = append(merges, Merge{
			Kind:    MergeContainerPortDedup,
			Service: b.svc.Name,
			Kept:    refs[0],
			Refs:    refs,
			Message: fmt.Sprintf("service %q exposes container port %d/%s through several Service ports; the container port is declared once", b.svc.Name, tk.port, strings.ToLower(tk.protocol)),
		})
	}

	sort.SliceStable(merges, func(i, j int) bool {
		if merges[i].Kept.Index != merges[j].Kept.Index {
			return merges[i].Kept.Index < merges[j].Kept.Index
		}
		return merges[i].Kind < merges[j].Kind
	})
	return merges
}

func mergeMessage(service string, kind MergeKind, key servicePortKey) string {
	switch kind {
	case MergeExactDuplicate:
		return fmt.Sprintf("service %q declares Service port %d/%s more than once; duplicates are dropped", service, key.port, strings.ToLower(key.protocol))
	case MergeAddressBindingIgnored:
		return fmt.Sprintf("service %q declares Service port %d/%s with different bind addresses; Services do not preserve bind addresses so the declarations collapse", service, key.port, strings.ToLower(key.protocol))
	case MergePublishedCoalesced:
		return fmt.Sprintf("service %q declares port %d/%s both with and without a host port; both resolve to Service port %d and collapse", service, key.port, strings.ToLower(key.protocol), key.port)
	default:
		return fmt.Sprintf("service %q merges declarations for Service port %d/%s", service, key.port, strings.ToLower(key.protocol))
	}
}

// effectivePort returns the Service port number: the published host port,
// defaulting to the container port when no host port was declared.
func effectivePort(n nMapping) int32 {
	if n.published != 0 {
		return n.published
	}
	return n.target
}

// plannedTargets returns the Service object names emitted for a service.
func plannedTargets(name, serviceType string, ports []PlannedPort) []string {
	if len(ports) == 0 {
		return nil
	}
	if serviceType != typeLoadBalancer {
		return []string{name}
	}
	var targets []string
	hasProto := map[string]bool{}
	for _, p := range ports {
		hasProto[p.Protocol] = true
	}
	if hasProto[ProtocolTCP] {
		targets = append(targets, name+"-tcp")
	}
	if hasProto[ProtocolUDP] {
		targets = append(targets, name+"-udp")
	}
	return targets
}

func normalizeServiceType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "clusterip":
		return typeClusterIP
	case "nodeport":
		return typeNodePort
	case "loadbalancer":
		return typeLoadBalancer
	case "headless":
		return typeHeadless
	default:
		return t
	}
}

var ingressEntrySplit = regexp.MustCompile(`[ ,]*,[ ,]*`)

// parseIngressRoutes mirrors the host[/path] parsing performed by kompose
// when creating Ingress objects.
func parseIngressRoutes(expose string) []IngressRoute {
	entries := ingressEntrySplit.Split(expose, -1)
	routes := make([]IngressRoute, 0, len(entries))
	for _, entry := range entries {
		host, path := entry, "/"
		if i := strings.Index(entry, "/"); i >= 0 {
			host, path = entry[:i], entry[i:]
		}
		if host == "true" {
			host = ""
		}
		routes = append(routes, IngressRoute{Host: host, Path: path})
	}
	return routes
}

// nodePortConflicts detects fixed node ports shared by distinct NodePort
// services.
func nodePortConflicts(services []ServiceSpec, plan *Plan) []Conflict {
	byNodePort := map[int32][]Ref{}
	for i, svc := range services {
		if normalizeServiceType(svc.Type) != typeNodePort || svc.NodePort <= 0 {
			continue
		}
		if len(plan.Services[i].Ports) == 0 {
			continue
		}
		first := plan.Services[i].Ports[0]
		if len(first.Members) == 0 {
			continue
		}
		byNodePort[svc.NodePort] = append(byNodePort[svc.NodePort], first.Members[0])
	}
	ports := make([]int32, 0, len(byNodePort))
	for p := range byNodePort {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	var conflicts []Conflict
	for _, p := range ports {
		refs := byNodePort[p]
		if len(refs) < 2 {
			continue
		}
		names := make([]string, 0, len(refs))
		for _, r := range refs {
			names = append(names, r.Service)
		}
		conflicts = append(conflicts, Conflict{
			Kind:    ConflictNodePortCollision,
			Message: fmt.Sprintf("fixed node port %d is requested by services %v; node ports are cluster unique", p, names),
			Refs:    append([]Ref(nil), refs...),
		})
	}
	return conflicts
}

// hostPortConflicts detects overlapping node-level bindings for services
// that expose container ports through hostPort.
func hostPortConflicts(services []ServiceSpec, mappings [][]nMapping) []Conflict {
	type hostKey struct {
		port     int32
		protocol string
	}
	type svcBindings struct {
		service int
		binds   map[string]struct{}
		refs    []Ref
	}
	groups := map[hostKey][]svcBindings{}
	order := []hostKey{}
	seen := map[hostKey]bool{}

	for i, svc := range services {
		if !svc.ExposeContainerToHost {
			continue
		}
		perGroup := map[hostKey]*svcBindings{}
		for _, m := range mappings[i] {
			if m.invalid != "" || m.published == 0 {
				continue
			}
			key := hostKey{port: m.published, protocol: m.protocol}
			if !seen[key] {
				seen[key] = true
				order = append(order, key)
			}
			if perGroup[key] == nil {
				perGroup[key] = &svcBindings{service: i, binds: map[string]struct{}{}}
			}
			perGroup[key].binds[binding{wildcard: m.wildcard, address: m.binding}.key()] = struct{}{}
			perGroup[key].refs = append(perGroup[key].refs, m.ref)
		}
		for key, sb := range perGroup {
			groups[key] = append(groups[key], *sb)
		}
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].port != order[j].port {
			return order[i].port < order[j].port
		}
		return order[i].protocol < order[j].protocol
	})

	var conflicts []Conflict
	for _, key := range order {
		sbs := groups[key]
		involved := map[int]bool{}
		for a := 0; a < len(sbs); a++ {
			for b := a + 1; b < len(sbs); b++ {
				if bindingsOverlap(sbs[a].binds, sbs[b].binds) {
					involved[sbs[a].service] = true
					involved[sbs[b].service] = true
				}
			}
		}
		if len(involved) == 0 {
			continue
		}
		var refs []Ref
		for si := range services {
			if !involved[si] {
				continue
			}
			for _, sb := range sbs {
				if sb.service == si {
					refs = append(refs, sb.refs...)
				}
			}
		}
		conflicts = append(conflicts, Conflict{
			Kind:    ConflictHostPortCollision,
			Message: fmt.Sprintf("host port %d/%s is published by services with overlapping bind addresses; two processes cannot bind the same node port", key.port, strings.ToLower(key.protocol)),
			Refs:    refs,
		})
	}
	return conflicts
}

// bindingsOverlap reports whether two bind sets collide on a node: a
// wildcard binding overlaps every address and equal specific addresses
// overlap each other.
func bindingsOverlap(a, b map[string]struct{}) bool {
	if _, ok := a["*"]; ok {
		return true
	}
	if _, ok := b["*"]; ok {
		return true
	}
	for addr := range a {
		if _, ok := b[addr]; ok {
			return true
		}
	}
	return false
}

// ingressRouteConflicts detects identical host/path rules claimed by
// distinct services.
func ingressRouteConflicts(services []ServiceSpec, plan *Plan) []Conflict {
	type routeKey struct {
		host string
		path string
	}
	byRoute := map[routeKey][]Ref{}
	for i, svc := range services {
		ingress := plan.Services[i].Ingress
		if ingress == nil {
			continue
		}
		var ref Ref
		if len(plan.Services[i].Ports) > 0 && len(plan.Services[i].Ports[0].Members) > 0 {
			ref = plan.Services[i].Ports[0].Members[0]
		} else {
			ref = Ref{Service: svc.Name}
		}
		for _, route := range ingress.Routes {
			key := routeKey{host: route.Host, path: route.Path}
			byRoute[key] = append(byRoute[key], ref)
		}
	}
	keys := make([]routeKey, 0, len(byRoute))
	for k := range byRoute {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].host != keys[j].host {
			return keys[i].host < keys[j].host
		}
		return keys[i].path < keys[j].path
	})
	var conflicts []Conflict
	for _, key := range keys {
		refs := byRoute[key]
		distinct := map[string]bool{}
		for _, r := range refs {
			distinct[r.Service] = true
		}
		if len(distinct) < 2 {
			continue
		}
		host := key.host
		if host == "" {
			host = "*"
		}
		conflicts = append(conflicts, Conflict{
			Kind:    ConflictIngressRoute,
			Message: fmt.Sprintf("ingress route %s%s is claimed by several services with distinct backends", host, key.path),
			Refs:    append([]Ref(nil), refs...),
		})
	}
	return conflicts
}
