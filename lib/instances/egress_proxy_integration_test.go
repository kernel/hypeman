package instances

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestEgressProxyRewritesHTTPSHeaders(t *testing.T) {
	requireKVMAccess(t)

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	caPEM, cert := mustGenerateTLSChain(t, []string{"localhost"})
	manager.egressProxyServiceOptions = egressproxy.ServiceOptions{
		AdditionalRootCAPEM: []string{caPEM},
	}

	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, r.Header.Get("Authorization"))
	}))
	target.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	target.StartTLS()
	defer target.Close()
	targetHostPort := strings.TrimPrefix(target.URL, "https://")
	targetHost, targetPort, err := net.SplitHostPort(targetHostPort)
	require.NoError(t, err)

	imageRef := integrationTestImageRef(t, "docker.io/library/nginx:alpine")
	t.Logf("Pulling %s image...", imageRef)
	created, err := manager.imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageRef})
	require.NoError(t, err)

	for i := 0; i < 120; i++ {
		img, err := manager.imageManager.GetImage(ctx, created.Name)
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}
	img, err := manager.imageManager.GetImage(ctx, created.Name)
	require.NoError(t, err)
	require.Equal(t, images.StatusReady, img.Status)

	require.NoError(t, manager.systemManager.EnsureSystemFiles(ctx))
	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-egress-proxy",
		Image:          imageRef,
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		NetworkEgress: &NetworkEgressPolicy{
			Enabled: true,
		},
		Credentials: map[string]CredentialPolicy{
			"OUTBOUND_OPENAI_KEY": {
				Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
				Inject: []CredentialInjectRule{
					{
						Hosts: []string{"127.0.0.1"},
						As: CredentialInjectAs{
							Header: "Authorization",
							Format: "Bearer ${value}",
						},
					},
				},
			},
		},
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "real-openai-key-123",
		},
		Entrypoint: []string{"/bin/sh", "-lc"},
		Cmd:        []string{"sleep 3600"},
	})
	require.NoError(t, err)

	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = manager.DeleteInstance(context.Background(), inst.Id)
		}
	})

	require.NoError(t, waitForVMReady(ctx, inst.SocketPath, 10*time.Second))
	require.NoError(t, waitForLogMessage(ctx, manager, inst.Id, "[guest-agent] listening", 45*time.Second))

	envOutput, envExitCode, err := execCommand(ctx, inst, "sh", "-lc", "printf '%s' \"$OUTBOUND_OPENAI_KEY\"")
	require.NoError(t, err)
	require.Equal(t, 0, envExitCode)
	require.Equal(t, "mock-OUTBOUND_OPENAI_KEY", envOutput)

	allowedCmd := fmt.Sprintf(
		"NO_PROXY= no_proxy= curl -k -sS https://%s:%s",
		targetHost, targetPort,
	)
	output, exitCode, err := execCommand(ctx, inst, "sh", "-lc", allowedCmd)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "curl output: %s", output)
	require.Contains(t, output, "Bearer real-openai-key-123")

	blockedCmd := fmt.Sprintf(
		"NO_PROXY= no_proxy= curl -k -sS https://localhost:%s",
		targetPort,
	)
	blockedOutput, blockedExitCode, err := execCommand(ctx, inst, "sh", "-lc", blockedCmd)
	require.NoError(t, err)
	require.Equal(t, 0, blockedExitCode, "curl output: %s", blockedOutput)
	require.Equal(t, "", blockedOutput)

	// --- Secret rotation: update credential env and verify proxy injects new value ---
	rotatedInst, err := manager.UpdateInstanceEnv(ctx, inst.Id, UpdateInstanceEnvRequest{
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "rotated-openai-key-456",
		},
	})
	require.NoError(t, err)
	require.Equal(t, StateRunning, rotatedInst.State)

	// Verify the proxy now injects the rotated credential
	rotatedOutput, rotatedExitCode, err := execCommand(ctx, inst, "sh", "-lc", allowedCmd)
	require.NoError(t, err)
	require.Equal(t, 0, rotatedExitCode, "curl output: %s", rotatedOutput)
	require.Contains(t, rotatedOutput, "Bearer rotated-openai-key-456")
	require.NotContains(t, rotatedOutput, "real-openai-key-123")

	require.NoError(t, manager.DeleteInstance(ctx, inst.Id))
	deleted = true
}

func mustGenerateTLSChain(t *testing.T, dnsNames []string) (string, tls.Certificate) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	caSerial, err := rand.Int(rand.Reader, serialLimit)
	require.NoError(t, err)

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName: "egress-proxy-test-ca",
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serverSerial, err := rand.Int(rand.Reader, serialLimit)
	require.NoError(t, err)

	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject: pkix.Name{
			CommonName: dnsNames[0],
		},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		SubjectKeyId: []byte{1, 2, 3, 4},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	cert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	require.NoError(t, err)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return string(caPEM), cert
}
