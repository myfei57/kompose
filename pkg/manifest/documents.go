/*
Copyright 2024 The Kubernetes Authors All rights reserved.

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

package manifest

import (
	"io"
	"os"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// DocumentIndex records where each top level Compose declaration was seen
// across the input files. A single service or secret may be declared in
// several files (Compose override semantics) or in several YAML documents of
// the same stream, so every name maps to a list of references.
//
// Only declaration names are read: field values (including Secret contents
// or environment variables) are never inspected here.
type DocumentIndex struct {
	// Files lists the files that were successfully scanned, in input order.
	Files []string
	// Services maps a normalized service name to its declarations.
	Services map[string][]DocumentRef
	// Secrets maps a secret name to its declarations.
	Secrets map[string][]DocumentRef
	// Warnings lists non-fatal scanning problems (unreadable files, ...).
	Warnings []string
}

// ServiceDocuments returns the declarations of a normalized service name.
func (d DocumentIndex) ServiceDocuments(name string) []DocumentRef {
	if d.Services == nil {
		return nil
	}
	return d.Services[name]
}

var serviceNameNormalizer = regexp.MustCompile(`[._]`)

// NormalizeServiceName mirrors the compose loader service name normalization
// (lower case, '.' and '_' replaced with '-') so that manifest records can be
// matched against the scanned declarations.
func NormalizeServiceName(name string) string {
	return strings.ToLower(serviceNameNormalizer.ReplaceAllString(name, "-"))
}

// ScanDocuments scans the given Compose input files. Reading stdin ("-") is
// not supported and produces a warning; unreadable files are skipped with a
// warning as well. The scan is best effort and never fatal.
func ScanDocuments(files []string) DocumentIndex {
	index := DocumentIndex{
		Services: map[string][]DocumentRef{},
		Secrets:  map[string][]DocumentRef{},
	}
	for _, file := range files {
		if file == "-" || file == "" {
			index.Warnings = append(index.Warnings, "cannot index Compose declarations read from stdin; document references will be incomplete")
			continue
		}
		declarations, found, err := scanOneFile(file)
		if err != nil {
			log.Warnf("Unable to index Compose file %q for the manifest: %v", file, err)
			index.Warnings = append(index.Warnings, "unable to index Compose file "+file+": "+err.Error())
			continue
		}
		if !found {
			continue
		}
		index.Files = append(index.Files, file)
		for _, decl := range declarations {
			ref := DocumentRef{File: file, Document: decl.document}
			switch decl.kind {
			case "services":
				name := NormalizeServiceName(decl.name)
				index.Services[name] = append(index.Services[name], ref)
			case "secrets":
				index.Secrets[decl.name] = append(index.Secrets[decl.name], ref)
			}
		}
	}
	return index
}

type declaration struct {
	kind     string
	name     string
	document int
}

// scanOneFile returns all top level service/secret declarations of one file.
// A file may contain several YAML documents separated by "---".
func scanOneFile(file string) ([]declaration, bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	var declarations []declaration
	decoder := yaml.NewDecoder(f)
	docIndex := -1
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		docIndex++
		// An empty document ("---") decodes to a node without content.
		if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
			continue
		}
		root := node.Content[0]
		if root.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(root.Content); i += 2 {
			key := root.Content[i]
			value := root.Content[i+1]
			if key.Kind != yaml.ScalarNode || value.Kind != yaml.MappingNode {
				continue
			}
			switch key.Value {
			case "services", "secrets":
				for j := 0; j+1 < len(value.Content); j += 2 {
					nameNode := value.Content[j]
					if nameNode.Kind != yaml.ScalarNode || nameNode.Value == "" {
						continue
					}
					declarations = append(declarations, declaration{
						kind:     key.Value,
						name:     nameNode.Value,
						document: docIndex,
					})
				}
			}
		}
	}
	return declarations, len(declarations) > 0, nil
}
