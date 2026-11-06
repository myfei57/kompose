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
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write %s: %v", path, err)
	}
}

// TestScanDocumentsMultipleFilesAndDuplicates covers the override scenario:
// the same service may be declared in several Compose files (and YAML
// streams may contain several documents), and every declaration must be
// retained. Values are never inspected, so secrets/environment cannot leak.
func TestScanDocumentsMultipleFilesAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	override := filepath.Join(dir, "override.yaml")
	stream := filepath.Join(dir, "stream.yaml")

	// base.yaml defines web and a top level secret.
	writeFile(t, base, `
services:
  web:
    image: nginx
  Web_Alias:
    image: busybox
secrets:
  cert:
    file: ./cert.pem
`)

	// override.yaml declares web a second time (Compose override merge).
	writeFile(t, override, `
services:
  web:
    replicas: 2
secrets: {}
`)

	// stream.yaml contains two YAML documents, the second one declaring db.
	writeFile(t, stream, "services:\n  web:\n    image: nginx\n---\nservices:\n  db:\n    image: postgres\n")

	index := ScanDocuments([]string{base, override, stream})

	web := index.ServiceDocuments("web")
	if len(web) != 3 {
		t.Fatalf("expected web declared in base.yaml, override.yaml and the first stream document, got %d: %+v", len(web), web)
	}

	// Normalization must mirror the compose loader: Web_Alias -> web-alias.
	alias := index.ServiceDocuments("web-alias")
	if len(alias) != 1 || alias[0].File != base {
		t.Fatalf("normalized service name was not resolved: %+v", alias)
	}

	db := index.ServiceDocuments("db")
	if len(db) != 1 || db[0].Document != 1 {
		t.Fatalf("expected db in document index 1 of the stream, got %+v", db)
	}

	cert, ok := index.Secrets["cert"]
	if !ok || len(cert) != 1 {
		t.Fatalf("expected top level secret cert indexed once, got %+v", index.Secrets)
	}

	if len(index.Files) != 3 {
		t.Fatalf("expected 3 indexed files, got %v", index.Files)
	}
}

func TestScanDocumentsMissingAndStdin(t *testing.T) {
	index := ScanDocuments([]string{"-", "does-not-exist.yaml"})
	if len(index.Warnings) != 2 {
		t.Fatalf("expected a warning for stdin and one for the missing file, got %v", index.Warnings)
	}
	if len(index.Files) != 0 {
		t.Fatalf("no file should be indexed, got %v", index.Files)
	}
}

func TestNormalizeServiceName(t *testing.T) {
	cases := map[string]string{
		"My.Service": "my-service",
		"web_app":    "web-app",
		"db":         "db",
	}
	for in, want := range cases {
		if got := NormalizeServiceName(in); got != want {
			t.Errorf("NormalizeServiceName(%q) = %q, want %q", in, got, want)
		}
	}
}
