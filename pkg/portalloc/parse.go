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
	"fmt"
	"strconv"
	"strings"
)

// ParseMapping parses one Compose short syntax port declaration into one
// or more MappingSpec values. The supported forms mirror the Compose
// specification:
//
//	container
//	host:container
//	ip:host:container           (IPv4 address)
//	[ipv6]:host:container       (bracketed IPv6 address)
//
// with an optional "/tcp" or "/udp" protocol suffix and port ranges such
// as "3000-3002:5000-5002". Ranges expand in ascending order and both
// sides of a range must have the same length.
//
// Parsing is purely structural: omitted protocols stay empty (so the
// allocator can flag the TCP default), and address/port semantics are
// validated by Allocate.
func ParseMapping(spec string) ([]MappingSpec, error) {
	body := strings.TrimSpace(spec)
	if body == "" {
		return nil, fmt.Errorf("empty port mapping")
	}

	protocol := ""
	if i := strings.LastIndex(body, "/"); i >= 0 {
		protocol = body[i+1:]
		body = body[:i]
		if protocol == "" || strings.ContainsAny(protocol, ":/[]") {
			return nil, fmt.Errorf("invalid protocol suffix in port mapping %q", spec)
		}
	}

	ip := ""
	rest := body
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return nil, fmt.Errorf("missing closing bracket in port mapping %q", spec)
		}
		ip = rest[1:end]
		rest = rest[end+1:]
		if rest == "" || rest[0] != ':' {
			return nil, fmt.Errorf("missing host and container ports after IPv6 address in port mapping %q", spec)
		}
		rest = rest[1:]
		// An address binding requires both host and container ports, as
		// enforced by the Docker port specification ("[ip]:host:container"
		// or "[ip]::container").
		if !strings.Contains(rest, ":") {
			return nil, fmt.Errorf("bind address requires both host and container ports in port mapping %q", spec)
		}
	} else {
		switch strings.Count(rest, ":") {
		case 0:
		case 1:
		case 2:
			segments := strings.SplitN(rest, ":", 3)
			ip, rest = segments[0], segments[1]+":"+segments[2]
		default:
			return nil, fmt.Errorf("IPv6 addresses must be enclosed in square brackets in port mapping %q", spec)
		}
	}

	var hostPart, containerPart string
	switch parts := strings.Split(rest, ":"); len(parts) {
	case 1:
		containerPart = parts[0]
	case 2:
		hostPart, containerPart = parts[0], parts[1]
	default:
		return nil, fmt.Errorf("too many port segments in port mapping %q", spec)
	}
	if containerPart == "" {
		return nil, fmt.Errorf("missing container port in port mapping %q", spec)
	}

	hostPorts, err := parsePortPart("host", hostPart)
	if err != nil {
		return nil, formatPartError(spec, err)
	}
	containerPorts, err := parsePortPart("container", containerPart)
	if err != nil {
		return nil, formatPartError(spec, err)
	}
	if len(hostPorts) != 0 && len(hostPorts) != len(containerPorts) {
		return nil, fmt.Errorf("host and container port ranges have different lengths in port mapping %q", spec)
	}

	mappings := make([]MappingSpec, len(containerPorts))
	for i, container := range containerPorts {
		m := MappingSpec{
			HostIP:   ip,
			Target:   container,
			Protocol: protocol,
		}
		if len(hostPorts) > 0 {
			m.Published = hostPorts[i]
		}
		mappings[i] = m
	}
	return mappings, nil
}

// parsePortPart parses a single "port" or "start-end" part. An empty part
// expands to nothing (omitted host port). Values must stay within the
// 0-65535 range and ranges must be ascending.
func parsePortPart(kind, part string) ([]int32, error) {
	if part == "" {
		return nil, nil
	}
	bounds := strings.Split(part, "-")
	if len(bounds) > 2 {
		return nil, fmt.Errorf("invalid %s port range %q", kind, part)
	}
	start, err := parsePortNumber(kind, bounds[0])
	if err != nil {
		return nil, err
	}
	if len(bounds) == 1 {
		return []int32{start}, nil
	}
	end, err := parsePortNumber(kind, bounds[1])
	if err != nil {
		return nil, err
	}
	if end < start {
		return nil, fmt.Errorf("invalid %s port range %q: start is greater than end", kind, part)
	}
	ports := make([]int32, 0, int(end-start)+1)
	for p := start; p <= end; p++ {
		ports = append(ports, p)
	}
	return ports, nil
}

func parsePortNumber(kind, value string) (int32, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > 65535 {
		return 0, fmt.Errorf("invalid %s port %q", kind, value)
	}
	return int32(n), nil
}

func formatPartError(spec string, err error) error {
	return fmt.Errorf("%v in port mapping %q", err, spec)
}
