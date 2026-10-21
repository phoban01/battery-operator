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
	"fmt"
	"os"
)

// CertificateMount reads battery's client certificate as the volume that
// battery reads it from holds it now. The kubelet updates a Secret volume
// some time after the Secret changes, so Reload waits until the mount
// holds what the Secret holds before it makes battery read the files.
type CertificateMount interface {
	// ClientCertificate returns the mounted certificate. An error that
	// wraps os.ErrNotExist means the volume is being swapped, and is
	// waited out.
	ClientCertificate() ([]byte, error)
}

// FileMount is the Operator's own mount of the Secret holding battery's
// client certificate: the volume battery's container mounts too.
type FileMount struct {
	// Path is the certificate's file in the Operator's container.
	Path string
}

var _ CertificateMount = FileMount{}

// ClientCertificate implements CertificateMount.
func (m FileMount) ClientCertificate() ([]byte, error) {
	b, err := os.ReadFile(m.Path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", m.Path, err)
	}
	return b, nil
}
