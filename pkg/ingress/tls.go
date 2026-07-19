package ingress

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// TLSMaterial is the short-lived, validated input for CMP certificate upload.
// It must never be persisted in observed state, annotations, events, or logs.
type TLSMaterial struct {
	Certificate []byte
	PrivateKey  []byte
	CA          []byte
	Fingerprint string
}

// ReadTLSSecret validates the Kubernetes TLS Secret before a certificate
// manager can upload it. The CMP Swagger requires certificate and private-key
// form fields, and accepting malformed data would otherwise leave an HTTPS
// listener with an unusable frontend.
func ReadTLSSecret(secret *corev1.Secret) (TLSMaterial, error) {
	if secret == nil {
		return TLSMaterial{}, fmt.Errorf("TLS secret is required")
	}
	if secret.Type != corev1.SecretTypeTLS {
		return TLSMaterial{}, fmt.Errorf("secret %s/%s must have type kubernetes.io/tls", secret.Namespace, secret.Name)
	}
	cert, key := secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]
	if len(cert) == 0 || len(key) == 0 {
		return TLSMaterial{}, fmt.Errorf("TLS secret %s/%s requires tls.crt and tls.key", secret.Namespace, secret.Name)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("TLS secret %s/%s has an invalid certificate/key pair: %w", secret.Namespace, secret.Name, err)
	}
	if len(pair.Certificate) == 0 {
		return TLSMaterial{}, fmt.Errorf("TLS secret %s/%s contains no certificate", secret.Namespace, secret.Name)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("TLS secret %s/%s has an invalid leaf certificate: %w", secret.Namespace, secret.Name, err)
	}
	if strings.TrimSpace(leaf.Subject.CommonName) == "" && len(leaf.DNSNames) == 0 {
		return TLSMaterial{}, fmt.Errorf("TLS secret %s/%s certificate has no DNS identity", secret.Namespace, secret.Name)
	}
	sum := sha256.Sum256(append(append([]byte(nil), cert...), key...))
	return TLSMaterial{Certificate: cert, PrivateKey: key, CA: secret.Data["ca.crt"], Fingerprint: fmt.Sprintf("%x", sum[:])}, nil
}
