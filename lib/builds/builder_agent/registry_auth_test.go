package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/builds/registryauth"
	"github.com/stretchr/testify/require"
)

func TestRegistryCredentialHandoffAndConfig(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	credentials := map[string]registryauth.Credential{"registry.example": {Username: "AWS", Password: "synthetic-token"}}
	done := make(chan error, 1)
	go func() {
		var request VsockMessage
		if err := json.NewDecoder(host).Decode(&request); err != nil {
			done <- err
			return
		}
		if request.Type != "get_registry_credentials" {
			done <- registryauth.ErrInvalid
			return
		}
		done <- json.NewEncoder(host).Encode(VsockMessage{Type: "registry_credentials", RegistryCredentials: credentials})
	}()
	got, err := fetchRegistryCredentials(json.NewEncoder(guest), json.NewDecoder(guest), "local.example:4973")
	require.NoError(t, err)
	require.NoError(t, <-done)
	auths := registryAuthEntries("local.example:4973", "local-token", got)
	require.Equal(t, "local-token", auths["local.example:4973"]["identitytoken"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("AWS:synthetic-token")), auths["registry.example"]["auth"])
	require.Len(t, auths, 2)
	data, err := json.Marshal(map[string]any{"auths": auths})
	require.NoError(t, err)
	directory := filepath.Join(t.TempDir(), ".docker")
	require.NoError(t, writeRegistryConfig(directory, data))
	info, err := os.Stat(filepath.Join(directory, "config.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	info, err = os.Stat(directory)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	// Rewriting config must not retain a previous build's external credentials.
	data, err = json.Marshal(map[string]any{"auths": registryAuthEntries("local.example:4973", "next-local-token", nil)})
	require.NoError(t, err)
	require.NoError(t, writeRegistryConfig(directory, data))
	written, err := os.ReadFile(filepath.Join(directory, "config.json"))
	require.NoError(t, err)
	require.NotContains(t, string(written), got["registry.example"].Auth())
	cleanupRegistryAuth(directory)
	require.NoFileExists(t, filepath.Join(directory, "config.json"))
}

func TestRegistryCredentialLogRedactionAcrossWrites(t *testing.T) {
	oldStdout := os.Stdout
	read, write, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = write
	defer func() { os.Stdout = oldStdout; read.Close(); write.Close() }()
	oldChan := logChan
	logChan = make(chan string, 100)
	defer func() { logChan = oldChan }()
	credential := registryauth.Credential{Username: "AWS", Password: `synthetic-"secret"/+=`}
	writer := newStreamingLogWriter()
	writer.setRedactor(registryauth.Redactor(map[string]registryauth.Credential{"registry.example": credential}))
	quoted, err := json.Marshal(credential.Password)
	require.NoError(t, err)
	input := "pull " + credential.Password + "\nconfig " + credential.Auth() + "\nquoted " + string(quoted)
	for _, b := range []byte(input) {
		_, err = writer.Write([]byte{b})
		require.NoError(t, err)
	}
	writer.markClosed()
	require.NoError(t, write.Close())
	stdout, err := io.ReadAll(read)
	require.NoError(t, err)
	streamed := ""
	for len(logChan) > 0 {
		streamed += <-logChan
	}
	for _, output := range []string{string(stdout), streamed, writer.String(), writer.redactText(input)} {
		require.NotContains(t, output, credential.Password)
		require.NotContains(t, output, credential.Auth())
		require.NotContains(t, output, string(quoted))
		require.Equal(t, 3, strings.Count(output, "[REDACTED]"))
	}
}
