package lbaas

import (
	"context"
	"fmt"
	"strings"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

// CertificateClient is the minimum CMP capability required to upload and bind
// certificates for HTTPS virtual servers. The implementation is intentionally
// thin because the repository's CMP client exposes a generic raw HTTP surface.
// The manager accepts a callback-based adapter so tests can verify behavior
// without requiring a full generated client.
type CertificateClient interface {
	ListCertificates(ctx context.Context, lbServiceID string) ([]CertificateResource, error)
	UploadCertificate(ctx context.Context, lbServiceID string, spec CertificateSpec) (CertificateResource, error)
	DeleteCertificate(ctx context.Context, lbServiceID, certificateID string) error
	BindCertificate(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error
	UnbindCertificate(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error
}

type CertificateResource struct {
	ID           string
	Name         string
	Fingerprint  string
	SecretName   string
	Certificate  string
	PrivateKey   string
	CA           string
	HostNames    []string
}

type CertificateSpec struct {
	Name        string
	Fingerprint string
	SecretName  string
	Certificate string
	PrivateKey  string
	CA          string
	HostNames   []string
}

type CertificateManager struct{ client CertificateClient }

func NewCertificateManager(client CertificateClient) *CertificateManager {
	return &CertificateManager{client: client}
}

func (m *CertificateManager) Ensure(ctx context.Context, lbServiceID, virtualServerID string, desired []model.Certificate, observed map[string]model.ObservedResource) ([]model.ObservedResource, bool, error) {
	if strings.TrimSpace(lbServiceID) == "" {
		return nil, false, fmt.Errorf("lb service id is required for certificate reconciliation")
	}
	if m.client == nil {
		return nil, false, fmt.Errorf("certificate client is required")
	}
	resources, err := m.client.ListCertificates(ctx, lbServiceID)
	if err != nil {
		return nil, false, fmt.Errorf("listing certificates for lb service %s: %w", lbServiceID, err)
	}
	byFingerprint := map[string]CertificateResource{}
	byName := map[string]CertificateResource{}
	for _, resource := range resources {
		if strings.TrimSpace(resource.ID) == "" {
			continue
		}
		if fingerprint := strings.ToLower(strings.TrimSpace(resource.Fingerprint)); fingerprint != "" {
			byFingerprint[fingerprint] = resource
		}
		if name := strings.ToLower(strings.TrimSpace(resource.Name)); name != "" {
			byName[name] = resource
		}
	}
	out := make([]model.ObservedResource, 0, len(desired))
	changed := false
	for _, cert := range desired {
		spec := CertificateSpec{
			Name:        cert.Name,
			SecretName:  cert.SecretName,
			HostNames:   append([]string(nil), cert.Hosts...),
			Fingerprint: strings.TrimSpace(cert.Fingerprint),
			Certificate: cert.Certificate,
			PrivateKey:  cert.PrivateKey,
			CA:          cert.CA,
		}
		if strings.TrimSpace(spec.Name) == "" {
			spec.Name = cert.SecretName
		}
		nameKey := strings.ToLower(strings.TrimSpace(spec.Name))
		if nameKey == "" {
			continue
		}

		var (
			resource CertificateResource
			exists   bool
		)
		if fingerprint := strings.ToLower(strings.TrimSpace(spec.Fingerprint)); fingerprint != "" {
			resource, exists = byFingerprint[fingerprint]
		}
		if !exists {
			resource, exists = byName[nameKey]
		}

		if exists && strings.TrimSpace(resource.ID) != "" {
			out = append(out, model.ObservedResource{LogicalID: cert.Name, ExternalID: resource.ID, Name: resource.Name, Ownership: cert.Ownership})
			if err := m.bindToVirtualServer(ctx, lbServiceID, virtualServerID, resource.ID); err != nil {
				return nil, changed, fmt.Errorf("binding certificate %s to virtual server %s: %w", cert.Name, virtualServerID, err)
			}
			continue
		}
		created, err := m.client.UploadCertificate(ctx, lbServiceID, spec)
		if err != nil {
			return nil, changed, fmt.Errorf("uploading certificate %s: %w", cert.Name, err)
		}
		if strings.TrimSpace(created.ID) == "" {
			return nil, changed, fmt.Errorf("CMP created certificate %s without a provider id", cert.Name)
		}
		if fingerprint := strings.ToLower(strings.TrimSpace(created.Fingerprint)); fingerprint != "" {
			byFingerprint[fingerprint] = created
		}
		byName[nameKey] = created
		out = append(out, model.ObservedResource{LogicalID: cert.Name, ExternalID: created.ID, Name: created.Name, Ownership: cert.Ownership})
		if err := m.bindToVirtualServer(ctx, lbServiceID, virtualServerID, created.ID); err != nil {
			return nil, changed, fmt.Errorf("binding certificate %s to virtual server %s: %w", cert.Name, virtualServerID, err)
		}
		changed = true
	}
	for key, resource := range observed {
		if strings.TrimSpace(resource.ExternalID) == "" {
			continue
		}
		var keep bool
		for _, cert := range desired {
			if key == cert.Name {
				keep = true
				break
			}
		}
		if keep {
			continue
		}
		if err := m.unbindFromVirtualServer(ctx, lbServiceID, virtualServerID, resource.ExternalID); err != nil {
			return nil, changed, fmt.Errorf("unbinding certificate %s from virtual server %s: %w", resource.ExternalID, virtualServerID, err)
		}
		if err := m.client.DeleteCertificate(ctx, lbServiceID, resource.ExternalID); err != nil {
			return nil, changed, fmt.Errorf("deleting unreferenced certificate %s: %w", resource.ExternalID, err)
		}
		changed = true
	}
	return out, changed, nil
}

func (m *CertificateManager) bindToVirtualServer(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error {
	if m.client == nil || strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" || strings.TrimSpace(certificateID) == "" {
		return nil
	}
	return m.client.BindCertificate(ctx, lbServiceID, virtualServerID, certificateID)
}

func (m *CertificateManager) unbindFromVirtualServer(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error {
	if m.client == nil || strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" || strings.TrimSpace(certificateID) == "" {
		return nil
	}
	return m.client.UnbindCertificate(ctx, lbServiceID, virtualServerID, certificateID)
}
