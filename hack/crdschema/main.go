/*
Copyright 2026.

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

// Command crdschema writes a JSON schema for kubeconform from every
// version of every CustomResourceDefinition in the YAML files it is given:
//
//	crdschema OUTDIR FILE...
//
// Each schema goes to OUTDIR/<group>/<kind>_<version>.json, with the kind
// in lower case, which is the path kubeconform's schema location template
// {{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json names. It does
// what kubeconform's openapi2jsonschema.py does in strict mode: every
// object schema that lists its properties, and says nothing of others,
// refuses others, so that a misspelt field fails the check rather than
// being dropped. hack/capi-check.sh runs it on the Cluster API and CAPA
// releases' CRDs.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// crd is the part of a CustomResourceDefinition this command reads.
type crd struct {
	Kind string `json:"kind"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind string `json:"kind"`
		} `json:"names"`
		Versions []struct {
			Name   string `json:"name"`
			Schema struct {
				OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: crdschema OUTDIR FILE...")
		os.Exit(2)
	}
	n := 0
	for _, file := range os.Args[2:] {
		written, err := convert(os.Args[1], file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crdschema: %s: %v\n", file, err)
			os.Exit(1)
		}
		n += written
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "crdschema: no CustomResourceDefinition with a schema in the files given")
		os.Exit(1)
	}
	fmt.Printf("crdschema: wrote %d schemas to %s\n", n, os.Args[1])
}

// convert writes a schema for every version of every CRD in file to
// outDir, and returns how many it wrote.
func convert(outDir, file string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	n := 0
	r := utilyaml.NewYAMLReader(bufio.NewReader(f))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var c crd
		if err := yaml.Unmarshal(doc, &c); err != nil {
			return n, err
		}
		if c.Kind != "CustomResourceDefinition" {
			continue
		}
		for _, v := range c.Spec.Versions {
			if v.Schema.OpenAPIV3Schema == nil {
				continue
			}
			strict(v.Schema.OpenAPIV3Schema)
			out, err := json.MarshalIndent(v.Schema.OpenAPIV3Schema, "", "  ")
			if err != nil {
				return n, err
			}
			path := filepath.Join(outDir, c.Spec.Group,
				strings.ToLower(c.Spec.Names.Kind)+"_"+v.Name+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return n, err
			}
			if err := os.WriteFile(path, out, 0o644); err != nil {
				return n, err
			}
			n++
		}
	}
}

// strict makes every object schema under s that lists properties, and
// neither allows other properties nor preserves unknown fields, refuse
// properties it does not list.
func strict(s any) {
	switch v := s.(type) {
	case map[string]any:
		_, lists := v["properties"]
		_, allows := v["additionalProperties"]
		_, preserves := v["x-kubernetes-preserve-unknown-fields"]
		if lists && !allows && !preserves {
			v["additionalProperties"] = false
		}
		for _, child := range v {
			strict(child)
		}
	case []any:
		for _, child := range v {
			strict(child)
		}
	}
}
