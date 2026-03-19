package egressproxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func loadOrCreateCA(dataDir string) (*x509.Certificate, *rsa.PrivateKey, string, error) {
	dir := filepath.Join(dataDir, "egressproxy")
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parseCA(certPEM, keyPEM)
		if err == nil {
			return cert, key, string(certPEM), nil
		}
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, "", fmt.Errorf("create egress proxy cert dir: %w", err)
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate egress proxy CA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate CA serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "hypeman-egress-proxy-ca",
			Organization: []string{"hypeman"},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create egress proxy CA cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, nil, "", fmt.Errorf("write egress proxy CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, nil, "", fmt.Errorf("write egress proxy CA key: %w", err)
	}

	cert, key, err := parseCA(certPEM, keyPEM)
	if err != nil {
		return nil, nil, "", err
	}

	return cert, key, string(certPEM), nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("parse egress proxy CA cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse egress proxy CA cert: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("parse egress proxy CA key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse egress proxy CA key: %w", err)
	}
	return cert, key, nil
}

func signHostCertificate(caCert *x509.Certificate, caKey *rsa.PrivateKey, host string) (*tls.Certificate, error) {
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign leaf cert: %w", err)
	}

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})

	tlsCert, err := tls.X509KeyPair(append(leafPEM, caPEM...), keyPEM)
	if err != nil {
		return nil, fmt.Errorf("build leaf keypair: %w", err)
	}
	if len(tlsCert.Certificate) > 0 {
		if leaf, parseErr := x509.ParseCertificate(tlsCert.Certificate[0]); parseErr == nil {
			tlsCert.Leaf = leaf
		}
	}
	return &tlsCert, nil
}
