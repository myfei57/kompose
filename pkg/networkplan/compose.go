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

	"github.com/compose-spec/compose-go/v2/types"
)

// defaultNetworkKey is the implicit Compose network attached to services
// that do not declare any network.
const defaultNetworkKey = "default"

// BuildFromComposeProject builds the plan directly from a compose-go
// project. It is a convenience wrapper around InputFromComposeProject and
// Build: the project is only read and copied into the neutral Input model,
// so it is never mutated, no file is written and no DNS lookup occurs.
func BuildFromComposeProject(project *types.Project) Plan {
	return Build(InputFromComposeProject(project))
}

// InputFromComposeProject copies the naming relevant fields of a compose-go
// project into the neutral Input model. Slices are copied on purpose so
// that later analysis can never mutate the project.
func InputFromComposeProject(project *types.Project) Input {
	input := Input{
		Networks: make(map[string]Network, len(project.Networks)),
		Services: make(map[string]Service, len(project.Services)),
	}

	for key, network := range project.Networks {
		input.Networks[key] = Network{
			Key:  key,
			Name: network.Name,
		}
	}

	for key, service := range project.Services {
		planned := Service{
			Name:        key,
			Hostname:    service.Hostname,
			DomainName:  service.DomainName,
			NetworkMode: service.NetworkMode,
			Links:       append([]string(nil), service.Links...),
			Attachments: make(map[string]Attachment, len(service.Networks)),
		}

		keys := make([]string, 0, len(service.Networks))
		for networkKey := range service.Networks {
			keys = append(keys, networkKey)
		}
		sort.Strings(keys)

		// Compose attaches services without explicit networks to the
		// implicit "default" network, mirroring the compose loader.
		if len(keys) == 0 {
			if _, ok := project.Networks[defaultNetworkKey]; ok {
				keys = []string{defaultNetworkKey}
			}
		}

		for _, networkKey := range keys {
			attachment := Attachment{}
			if configured := service.Networks[networkKey]; configured != nil {
				attachment.Aliases = append([]string(nil), configured.Aliases...)
			}
			planned.Attachments[networkKey] = attachment
		}

		input.Services[key] = planned
	}

	return input
}
