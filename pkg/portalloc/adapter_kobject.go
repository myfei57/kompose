//go:build go1.24

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
	"sort"

	"github.com/kubernetes/kompose/pkg/kobject"
)

// FromKomposeObject adapts a loaded KomposeObject to the read-only
// allocator input. Services are returned in sorted name order, which is
// the order used by the Kubernetes transformer, so plans stay stable
// across runs. The input object is never modified.
//
// Note that the Compose loader already uppercases protocols and defaults
// omitted protocols to TCP, so mappings built this way cannot be flagged
// in DefaultedProtocol. Build ServiceSpec values directly (optionally via
// ParseMapping) to preserve that information.
func FromKomposeObject(ko *kobject.KomposeObject) []ServiceSpec {
	names := make([]string, 0, len(ko.ServiceConfigs))
	for name := range ko.ServiceConfigs {
		names = append(names, name)
	}
	sort.Strings(names)

	services := make([]ServiceSpec, 0, len(names))
	for _, name := range names {
		cfg := ko.ServiceConfigs[name]
		services = append(services, FromServiceConfig(name, cfg))
	}
	return services
}

// FromServiceConfig adapts a single loaded service configuration. Port
// declaration order is preserved.
func FromServiceConfig(name string, cfg kobject.ServiceConfig) ServiceSpec {
	svc := ServiceSpec{
		Name:                  name,
		Type:                  cfg.ServiceType,
		NodePort:              cfg.NodePortPort,
		ExposeContainerToHost: cfg.ExposeContainerToHost,
		ExposeService:         cfg.ExposeService,
		Mappings:              make([]MappingSpec, 0, len(cfg.Port)),
	}
	for _, port := range cfg.Port {
		svc.Mappings = append(svc.Mappings, MappingSpec{
			HostIP:    port.HostIP,
			Published: port.HostPort,
			Target:    port.ContainerPort,
			Protocol:  port.Protocol,
		})
	}
	return svc
}
