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
	snapshottest "github.com/kernel/hypeman/lib/snapshot/testsupport"
	"github.com/stretchr/testify/require"
)

func TestEgressProxyRewritesHTTPSHeaders(t *testing.T) {
	t.Parallel()
	acquireHeavyIO(t)

	requireKVMAccess(t)

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	probeTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "proxy-ok")
	}))
	defer probeTarget.Close()

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
	snapshottest.EnsureImageReady(t, ctx, manager.paths, manager.imageManager, imageRef)

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
			_ = deleteTestInstanceNow(context.Background(), manager, inst.Id)
		}
	})

	_, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, integrationTestTimeout(5*time.Second))
	if err != nil {
		logs, logErr := collectLogs(ctx, manager, inst.Id, 200)
		if logErr != nil {
			t.Logf("failed to collect logs after Running timeout: %v", logErr)
		} else {
			t.Logf("app logs after Running timeout:\n%s", logs)
		}
		current, getErr := manager.GetInstance(ctx, inst.Id)
		if getErr != nil {
			t.Logf("failed to get instance after Running timeout: %v", getErr)
		} else {
			t.Logf("instance after Running timeout: state=%s program_started_at=%v guest_agent_ready_at=%v boot_markers_hydrated=%v", current.State, current.ProgramStartedAt, current.GuestAgentReadyAt, current.BootMarkersHydrated)
		}
	}
	require.NoError(t, err)

	envOutput, envExitCode, err := execCommand(ctx, inst, "sh", "-lc", "printf '%s\\n%s\\n%s' \"$OUTBOUND_OPENAI_KEY\" \"$HTTP_PROXY\" \"$HTTPS_PROXY\"")
	require.NoError(t, err)
	require.Equal(t, 0, envExitCode)
	envLines := strings.Split(strings.TrimSpace(envOutput), "\n")
	require.Len(t, envLines, 3, "unexpected env output: %q", envOutput)
	require.Equal(t, "mock-OUTBOUND_OPENAI_KEY", envLines[0])

	alloc, err := manager.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	proxyURL := fmt.Sprintf("http://%s:%d", alloc.Gateway, egressproxy.DefaultListenPort)
	require.Equal(t, proxyURL, envLines[1])
	require.Equal(t, proxyURL, envLines[2])

	probeCmd := fmt.Sprintf(
		"NO_PROXY= no_proxy= curl -sS --proxy %s %s",
		proxyURL,
		probeTarget.URL,
	)
	probeOutput, probeExitCode, err := execCommand(ctx, inst, "sh", "-lc", probeCmd)
	require.NoError(t, err)
	require.Equal(t, 0, probeExitCode, "curl output: %s", probeOutput)
	require.Equal(t, "proxy-ok", strings.TrimSpace(probeOutput))

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

	// === Key rotation: update credential env and verify new value is used ===
	t.Log("Updating egress proxy credential env for key rotation...")
	updated, err := manager.UpdateInstance(ctx, inst.Id, UpdateInstanceRequest{
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "rotated-key-456",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "rotated-key-456", updated.Env["OUTBOUND_OPENAI_KEY"])

	// Guest-visible env should still show the mock value (secrets never reach guest)
	envAfterUpdate, envExitCode2, err := execCommand(ctx, inst, "sh", "-lc", "printf '%s' \"$OUTBOUND_OPENAI_KEY\"")
	require.NoError(t, err)
	require.Equal(t, 0, envExitCode2)
	require.Equal(t, "mock-OUTBOUND_OPENAI_KEY", envAfterUpdate)

	// Egress proxy should now inject the rotated key
	rotatedCmd := fmt.Sprintf(
		"NO_PROXY= no_proxy= curl -k -sS https://%s:%s",
		targetHost, targetPort,
	)
	rotatedOutput, rotatedExitCode, err := execCommand(ctx, inst, "sh", "-lc", rotatedCmd)
	require.NoError(t, err)
	require.Equal(t, 0, rotatedExitCode, "curl output: %s", rotatedOutput)
	require.Contains(t, rotatedOutput, "Bearer rotated-key-456")
	require.NotContains(t, rotatedOutput, "real-openai-key-123")

	require.NoError(t, deleteTestInstanceNow(ctx, manager, inst.Id))
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
