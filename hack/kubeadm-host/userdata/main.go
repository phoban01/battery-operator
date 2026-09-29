//go:build kubeadmhost

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

// Command userdata renders a Host's cloud-init user-data from a Host pool's
// KubeadmConfigTemplate, the way Cluster API's kubeadm bootstrap provider
// (CABPK) renders it for a worker Machine: the template's files and users,
// the join configuration as a kubeadm v1beta4 JoinConfiguration with the
// bootstrap token discovery CABPK fills in, and `kubeadm join` in runcmd.
// The kubeadm Host proof (hack/kubeadm-host/README.md) boots the Host Image
// with it.
//
// It reads `kustomize build` output of a pool on stdin and writes the
// user-data on stdout. It covers the fields the templates in config/capi
// use, and fails on any other, so that nothing in a template is dropped
// without notice. The build tag keeps it out of the module's build, tests
// and lint.
//
//	go run -tags kubeadmhost ./hack/kubeadm-host/userdata -api-server 192.168.64.2:6443 \
//	    -token abcdef.0123456789abcdef -ca-cert-hash sha256:... <pool.yaml
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// joinConfigPath and the success file are CABPK's.
const (
	joinConfigPath = "/run/kubeadm/kubeadm-join-config.yaml"
	successFile    = "/run/cluster-api/bootstrap-success.complete"
)

func main() {
	apiServer := flag.String("api-server", "", "the control plane endpoint, host:port")
	token := flag.String("token", "", "a kubeadm bootstrap token")
	caCertHash := flag.String("ca-cert-hash", "", "the cluster CA's public key hash, sha256:<hex>")
	flag.Parse()
	if *apiServer == "" || *token == "" || *caCertHash == "" {
		fmt.Fprintln(os.Stderr, "userdata: -api-server, -token and -ca-cert-hash are required")
		os.Exit(2)
	}
	spec, err := readTemplate(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "userdata: %v\n", err)
		os.Exit(1)
	}
	out, err := render(spec, *apiServer, *token, *caCertHash)
	if err != nil {
		fmt.Fprintf(os.Stderr, "userdata: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintf(os.Stderr, "userdata: %v\n", err)
		os.Exit(1)
	}
}

// readTemplate returns spec.template.spec of the one KubeadmConfigTemplate
// in a stream of YAML documents.
func readTemplate(r io.Reader) (map[string]any, error) {
	docs, err := splitDocuments(r)
	if err != nil {
		return nil, err
	}
	var found map[string]any
	for _, d := range docs {
		var obj map[string]any
		if err := yaml.Unmarshal(d, &obj); err != nil {
			return nil, fmt.Errorf("parsing a document: %w", err)
		}
		if obj["kind"] != "KubeadmConfigTemplate" {
			continue
		}
		if found != nil {
			return nil, errors.New("more than one KubeadmConfigTemplate; build one pool")
		}
		spec, ok := dig(obj, "spec", "template", "spec")
		if !ok {
			return nil, errors.New("the KubeadmConfigTemplate has no spec.template.spec")
		}
		found = spec
	}
	if found == nil {
		return nil, errors.New("no KubeadmConfigTemplate in the input")
	}
	return found, nil
}

func splitDocuments(r io.Reader) ([][]byte, error) {
	var docs [][]byte
	var cur bytes.Buffer
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for s.Scan() {
		line := s.Text()
		if strings.TrimRight(line, " ") == "---" {
			if strings.TrimSpace(cur.String()) != "" {
				docs = append(docs, bytes.Clone(cur.Bytes()))
			}
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cur.String()) != "" {
		docs = append(docs, cur.Bytes())
	}
	return docs, nil
}

func dig(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	return m, true
}

// handled are the KubeadmConfigSpec fields this renderer maps. Any other
// field in a template is an error.
var handled = map[string]bool{
	"format": true, "files": true, "users": true, "joinConfiguration": true,
	"preKubeadmCommands": true, "postKubeadmCommands": true,
}

func render(spec map[string]any, apiServer, token, caCertHash string) ([]byte, error) {
	var unknown []string
	for k := range spec {
		if !handled[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("the template sets fields this renderer does not map: %s", strings.Join(unknown, ", "))
	}
	if f, ok := spec["format"]; ok && f != "cloud-config" {
		return nil, fmt.Errorf("format %v: only cloud-config is supported", f)
	}

	join, err := joinConfiguration(spec, apiServer, token, caCertHash)
	if err != nil {
		return nil, err
	}

	writeFiles, err := files(spec)
	if err != nil {
		return nil, err
	}
	// CABPK writes the join configuration after the template's files, and a
	// placeholder that makes /run/cluster-api.
	writeFiles = append(writeFiles,
		map[string]any{"path": joinConfigPath, "owner": "root:root", "permissions": "0640", "content": join},
		map[string]any{
			"path": "/run/cluster-api/placeholder", "owner": "root:root", "permissions": "0640",
			"content": "This placeholder file is used to create the /run/cluster-api sub directory",
		},
	)

	cfg := map[string]any{"write_files": writeFiles}
	if u, err := users(spec); err != nil {
		return nil, err
	} else if len(u) > 0 {
		cfg["users"] = u
	}
	var runcmd []any
	pre, err := commands(spec, "preKubeadmCommands")
	if err != nil {
		return nil, err
	}
	post, err := commands(spec, "postKubeadmCommands")
	if err != nil {
		return nil, err
	}
	runcmd = append(runcmd, pre...)
	runcmd = append(runcmd, fmt.Sprintf("kubeadm join --config %s  && echo success > %s", joinConfigPath, successFile))
	runcmd = append(runcmd, post...)
	cfg["runcmd"] = runcmd

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	// CABPK's header: cloud-init renders the user-data as a Jinja template,
	// which fills in {{ ds.meta_data.local_hostname }} and the like.
	return append([]byte("## template: jinja\n#cloud-config\n\n"), body...), nil
}

// joinConfiguration turns the template's joinConfiguration into kubeadm's
// v1beta4 JoinConfiguration, as CABPK does for Kubernetes 1.31 and later:
// the node registration is the same shape in both, and the discovery is
// the bootstrap token CABPK fills in.
func joinConfiguration(spec map[string]any, apiServer, token, caCertHash string) (string, error) {
	jc, ok := spec["joinConfiguration"].(map[string]any)
	if !ok {
		return "", errors.New("the template has no joinConfiguration")
	}
	out := map[string]any{
		"apiVersion": "kubeadm.k8s.io/v1beta4",
		"kind":       "JoinConfiguration",
		"discovery": map[string]any{
			"bootstrapToken": map[string]any{
				"apiServerEndpoint": apiServer,
				"token":             token,
				"caCertHashes":      []any{caCertHash},
			},
		},
	}
	for k, v := range jc {
		switch k {
		case "nodeRegistration", "skipPhases":
			out[k] = v
		case "discovery":
			return "", errors.New("the template sets joinConfiguration.discovery, which CABPK fills in")
		default:
			return "", fmt.Errorf("joinConfiguration.%s: this renderer does not map it", k)
		}
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		return "", err
	}
	return "---\n" + string(b), nil
}

// files maps the template's files to cloud-init write_files, as CABPK
// does. contentFrom needs a Secret, and the templates use none.
func files(spec map[string]any) ([]any, error) {
	raw, _ := spec["files"].([]any)
	var out []any
	for i, f := range raw {
		m, ok := f.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("files[%d] is not an object", i)
		}
		wf := map[string]any{}
		for k, v := range m {
			switch k {
			case "path", "owner", "permissions", "content", "append", "encoding":
				wf[k] = v
			default:
				return nil, fmt.Errorf("files[%d].%s: this renderer does not map it", i, k)
			}
		}
		out = append(out, wf)
	}
	return out, nil
}

// users maps the template's users to cloud-init users, with CABPK's names.
func users(spec map[string]any) ([]any, error) {
	raw, _ := spec["users"].([]any)
	names := map[string]string{
		"name": "name", "gecos": "gecos", "groups": "groups", "homeDir": "homedir",
		"inactive": "inactive", "shell": "shell", "primaryGroup": "primary_group",
		"lockPassword": "lock_passwd", "sudo": "sudo", "sshAuthorizedKeys": "ssh_authorized_keys",
	}
	var out []any
	for i, u := range raw {
		m, ok := u.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("users[%d] is not an object", i)
		}
		cu := map[string]any{}
		for k, v := range m {
			n, ok := names[k]
			if !ok {
				return nil, fmt.Errorf("users[%d].%s: this renderer does not map it", i, k)
			}
			cu[n] = v
		}
		out = append(out, cu)
	}
	return out, nil
}

func commands(spec map[string]any, key string) ([]any, error) {
	raw, ok := spec[key]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a list", key)
	}
	return list, nil
}
