package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/builds/registryauth"
)

func fetchRegistryCredentials(encoder *json.Encoder, decoder *json.Decoder, localRegistry string) (map[string]registryauth.Credential, error) {
	encoderLock.Lock()
	err := encoder.Encode(VsockMessage{Type: "get_registry_credentials"})
	encoderLock.Unlock()
	if err != nil {
		return nil, fmt.Errorf("request registry credentials")
	}
	var response VsockMessage
	if err := decoder.Decode(&response); err != nil || response.Type != "registry_credentials" || len(response.RegistryCredentials) == 0 {
		return nil, fmt.Errorf("receive registry credentials")
	}
	return registryauth.Normalize(response.RegistryCredentials, localRegistry, time.Now())
}

func registryAuthEntries(host, token string, credentials map[string]registryauth.Credential) map[string]map[string]string {
	auths := map[string]map[string]string{
		host: {"auth": base64.StdEncoding.EncodeToString([]byte(token + ":")), "identitytoken": token},
	}
	for host, credential := range credentials {
		auths[registryauth.DockerKey(host)] = map[string]string{"auth": credential.Auth()}
	}
	return auths
}

func writeRegistryConfig(directory string, data []byte) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(directory, "config.json"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	_, err = file.Write(data)
	return err
}

func cleanupRegistryAuth(directories ...string) {
	for _, directory := range directories {
		path := filepath.Join(directory, "config.json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("Failed to clean up registry auth file: %s", path)
		}
	}
}
