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

package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// RestartPendingAnnotation is set to "true" on battery's ConfigMap between
// writing a new configuration and battery answering again with it, so
// that a restart the Operator did not finish (it crashed, or the restart
// failed) is finished on the next reconcile.
const RestartPendingAnnotation = "battery.liquidmetal-x.dev/restart-pending"

// ClientCertificateAnnotation records on battery's ConfigMap the SHA-256
// digest, in hex, of the client certificate battery last restarted with,
// so that the Inventory Controller can tell when cert-manager has renewed
// it (DP-007).
const ClientCertificateAnnotation = "battery.liquidmetal-x.dev/client-certificate-sha256"

// Config is battery's configuration as stored.
type Config struct {
	// Hosts are the configuration's Hosts, without the placeholder an empty
	// inventory renders.
	Hosts Hosts
	// Raw is the configuration file.
	Raw []byte
	// Pending is true when Raw was written and battery may not yet have
	// restarted with it.
	Pending bool
	// ClientCertificate is the Digest of the client certificate battery last
	// restarted with, "" if none is recorded.
	ClientCertificate string
}

// Digest is the SHA-256 digest of a client certificate, in hex: what
// ClientCertificateAnnotation holds.
func Digest(cert []byte) string {
	sum := sha256.Sum256(cert)
	return hex.EncodeToString(sum[:])
}

// Store reads and writes battery's configuration.
type Store interface {
	// Load reads the configuration.
	Load(ctx context.Context) (Config, error)
	// Save writes c's Raw, Pending and ClientCertificate; Hosts are Raw's.
	Save(ctx context.Context, c Config) error
}

// ConfigMapStore keeps battery's configuration in its ConfigMap, under
// batterysidecar.ConfigKey, which the Manifests ship and mount into
// battery's container (DP-004).
type ConfigMapStore struct {
	// Reader reads the ConfigMap. The Operator may read that one ConfigMap
	// only, so this is the manager's API reader, not its cache.
	Reader client.Reader
	// Writer updates it.
	Writer client.Writer
	// Key names it.
	Key client.ObjectKey
}

var _ Store = ConfigMapStore{}

// Load implements Store.
func (c ConfigMapStore) Load(ctx context.Context) (Config, error) {
	cm, err := c.get(ctx)
	if err != nil {
		return Config{}, err
	}
	raw := []byte(cm.Data[batterysidecar.ConfigKey])
	f, err := batterysidecar.Parse(raw)
	if err != nil {
		return Config{}, fmt.Errorf("reading ConfigMap %s: %w", c.Key, err)
	}
	hosts := Hosts{}
	for _, h := range f.Hosts {
		if h.Name != batterysidecar.PlaceholderHostName {
			hosts[h.Name] = h.Address
		}
	}
	return Config{
		Hosts:             hosts,
		Raw:               raw,
		Pending:           cm.Annotations[RestartPendingAnnotation] == annotationTrue,
		ClientCertificate: cm.Annotations[ClientCertificateAnnotation],
	}, nil
}

// Save implements Store.
func (c ConfigMapStore) Save(ctx context.Context, config Config) error {
	cm, err := c.get(ctx)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[batterysidecar.ConfigKey] = string(config.Raw)
	setAnnotation(cm, RestartPendingAnnotation, config.Pending, annotationTrue)
	setAnnotation(cm, ClientCertificateAnnotation, config.ClientCertificate != "", config.ClientCertificate)
	if err := c.Writer.Update(ctx, cm); err != nil {
		return fmt.Errorf("updating ConfigMap %s: %w", c.Key, err)
	}
	return nil
}

// setAnnotation sets key to value on cm if set, and removes it otherwise.
func setAnnotation(cm *corev1.ConfigMap, key string, set bool, value string) {
	if !set {
		delete(cm.Annotations, key)
		return
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[key] = value
}

func (c ConfigMapStore) get(ctx context.Context) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Reader.Get(ctx, c.Key, cm); err != nil {
		return nil, fmt.Errorf("reading ConfigMap %s: %w", c.Key, err)
	}
	return cm, nil
}
