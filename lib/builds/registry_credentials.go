package builds

import (
	"errors"
	"maps"
	"time"

	"github.com/kernel/hypeman/lib/builds/registryauth"
	"github.com/kernel/hypeman/lib/images"
)

var errRegistryCredentialsUnavailable = errors.New("build registry credentials expired or unavailable; submit a new build with fresh credentials")

type borrowedBuildCredentials struct {
	credentials map[string]registryauth.Credential
	expiresAt   time.Time
	timer       *time.Timer
}

func (m *manager) holdRegistryCredentials(id string, credentials map[string]registryauth.Credential) {
	expires := time.Now().Add(images.DefaultBorrowedCredentialsTimeout)
	for _, c := range credentials {
		if c.ExpiresAt != nil && c.ExpiresAt.Before(expires) {
			expires = *c.ExpiresAt
		}
	}
	m.credentialsMu.Lock()
	defer m.credentialsMu.Unlock()
	if m.registryCredentials == nil {
		m.registryCredentials = make(map[string]*borrowedBuildCredentials)
	}
	entry := &borrowedBuildCredentials{credentials: credentials, expiresAt: expires}
	m.registryCredentials[id] = entry
	entry.timer = time.AfterFunc(time.Until(expires), func() { m.releaseRegistryCredentials(id) })
}

func (m *manager) borrowRegistryCredentials(id string) (map[string]registryauth.Credential, time.Time, error) {
	m.credentialsMu.Lock()
	defer m.credentialsMu.Unlock()
	entry := m.registryCredentials[id]
	if entry == nil || !time.Now().Before(entry.expiresAt) {
		return nil, time.Time{}, errRegistryCredentialsUnavailable
	}
	credentials := maps.Clone(entry.credentials)
	for host, credential := range credentials {
		expires := entry.expiresAt
		credential.ExpiresAt = &expires
		credentials[host] = credential
	}
	return credentials, entry.expiresAt, nil
}

func (m *manager) releaseRegistryCredentials(id string) {
	m.credentialsMu.Lock()
	defer m.credentialsMu.Unlock()
	if entry := m.registryCredentials[id]; entry != nil {
		entry.timer.Stop()
		entry.credentials = nil
		delete(m.registryCredentials, id)
	}
}
