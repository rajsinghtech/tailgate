package webhook

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureCerts generates a self-signed CA + serving cert for the webhook Service, writes the
// serving cert to certDir (tls.crt/tls.key) for the controller-runtime webhook server, and
// patches the CA bundle into the named MutatingWebhookConfiguration. No cert-manager
// dependency (platform-agnostic). The operator runs a single replica, so regenerating on each
// start is fine — the webhook config's caBundle is repatched to match the fresh cert.
func EnsureCerts(ctx context.Context, c client.Client, certDir, service, namespace, webhookCfg string) error {
	dnsNames := []string{
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
	notBefore := time.Now().Add(-time.Hour)
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour)

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tailgate-webhook-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(certDir, "tls.crt"), "CERTIFICATE", leafDER); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(certDir, "tls.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(leafKey)); err != nil {
		return err
	}

	var cfg admissionv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: webhookCfg}, &cfg); err != nil {
		return fmt.Errorf("get webhook config %q: %w", webhookCfg, err)
	}
	for i := range cfg.Webhooks {
		cfg.Webhooks[i].ClientConfig.CABundle = caPEM
	}
	if err := c.Update(ctx, &cfg); err != nil {
		return fmt.Errorf("patch webhook config %q caBundle: %w", webhookCfg, err)
	}
	return nil
}

func writePEM(path, typ string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
}
