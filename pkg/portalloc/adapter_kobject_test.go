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
	"reflect"
	"testing"

	"github.com/kubernetes/kompose/pkg/kobject"
)

func TestFromKomposeObjectStableOrder(t *testing.T) {
	ko := &kobject.KomposeObject{
		ServiceConfigs: map[string]kobject.ServiceConfig{
			"zeta": {
				Name: "zeta",
				Port: []kobject.Ports{
					{HostPort: 80, ContainerPort: 80, HostIP: "", Protocol: "TCP"},
				},
				ServiceType: "LoadBalancer",
			},
			"alpha": {
				Name: "alpha",
				Port: []kobject.Ports{
					{HostPort: 53, ContainerPort: 53, Protocol: "UDP"},
				},
			},
		},
	}

	services := FromKomposeObject(ko)
	if got := []string{services[0].Name, services[1].Name}; !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Errorf("services = %v, want sorted name order", got)
	}

	plan := Allocate(services)
	if plan.HasConflicts() {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}
	if plan.Services[1].Type != typeLoadBalancer {
		t.Errorf("expected LoadBalancer type, got %q", plan.Services[1].Type)
	}
	if !reflect.DeepEqual(plan.Services[1].Targets, []string{"zeta-tcp"}) {
		t.Errorf("targets = %v", plan.Services[1].Targets)
	}

	// The source object must not be modified.
	if ko.ServiceConfigs["zeta"].Port[0].Protocol != "TCP" {
		t.Error("FromKomposeObject mutated the source KomposeObject")
	}
}

func TestFromServiceConfigMapping(t *testing.T) {
	cfg := kobject.ServiceConfig{
		Name:          "web",
		ServiceType:   "NodePort",
		NodePortPort:  30000,
		ExposeService: "example.com",
		Port: []kobject.Ports{
			{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "TCP"},
		},
	}
	svc := FromServiceConfig("web", cfg)
	want := ServiceSpec{
		Name:          "web",
		Type:          "NodePort",
		NodePort:      30000,
		ExposeService: "example.com",
		Mappings: []MappingSpec{
			{HostIP: "127.0.0.1", Published: 8080, Target: 80, Protocol: "TCP"},
		},
	}
	if !reflect.DeepEqual(svc, want) {
		t.Errorf("FromServiceConfig = %+v, want %+v", svc, want)
	}
}
