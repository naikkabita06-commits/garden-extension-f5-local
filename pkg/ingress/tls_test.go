package ingress

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReadTLSSecretValidatesPairAndFingerprint(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.test"}, DNSNames: []string{"example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.test"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	material, err := ReadTLSSecret(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "tls"}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: priv}})
	if err != nil || material.Fingerprint == "" {
		t.Fatalf("material=%#v err=%v", material, err)
	}
}

func TestReadTLSSecretRejectsInvalidSecret(t *testing.T) {
	_, err := ReadTLSSecret(&corev1.Secret{Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: []byte("bad"), corev1.TLSPrivateKeyKey: []byte("bad")}})
	if err == nil {
		t.Fatal("expected invalid TLS material to fail")
	}
}
