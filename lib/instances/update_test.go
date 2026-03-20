package instances

import (
	"context"
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateUpdateInstanceRequest(t *testing.T) {
	baseMeta := &metadata{
		StoredMetadata: StoredMetadata{
			NetworkEgress: &NetworkEgressPolicy{Enabled: true},
			Credentials: map[string]CredentialPolicy{
				"OUTBOUND_OPENAI_KEY": {
					Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
				},
			},
		},
	}

	t.Run("requires at least one env key", func(t *testing.T) {
		err := validateUpdateInstanceRequest(baseMeta, UpdateInstanceRequest{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "at least one credential source env var")
	})

	t.Run("rejects instances without credential backed envs", func(t *testing.T) {
		err := validateUpdateInstanceRequest(&metadata{}, UpdateInstanceRequest{
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "rotated"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "no credential-backed env vars")
	})

	t.Run("rejects unrelated env keys", func(t *testing.T) {
		err := validateUpdateInstanceRequest(baseMeta, UpdateInstanceRequest{
			Env: map[string]string{"UNRELATED_KEY": "value"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "UNRELATED_KEY")
		assert.Contains(t, err.Error(), "OUTBOUND_OPENAI_KEY")
	})

	t.Run("allows credential source env keys", func(t *testing.T) {
		err := validateUpdateInstanceRequest(baseMeta, UpdateInstanceRequest{
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "rotated"},
		})
		require.NoError(t, err)
	})
}

func TestUpdateInstanceRulesServiceOrNil(t *testing.T) {
	t.Parallel()

	var svc *egressproxy.Service
	assert.Nil(t, updateInstanceRulesServiceOrNil(svc))

	typedSvc := updateInstanceRulesService(&fakeUpdateInstanceRulesService{})
	require.NotNil(t, typedSvc)
}

type fakeUpdateInstanceRulesService struct {
	calls [][]egressproxy.HeaderInjectRuleConfig
	errs  []error
}

func (f *fakeUpdateInstanceRulesService) UpdateInstanceRules(_ context.Context, _ string, rules []egressproxy.HeaderInjectRuleConfig) error {
	copied := make([]egressproxy.HeaderInjectRuleConfig, len(rules))
	copy(copied, rules)
	f.calls = append(f.calls, copied)

	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func TestApplyUpdatedInstanceEnvWithoutProxyService(t *testing.T) {
	t.Parallel()

	meta := &metadata{
		StoredMetadata: StoredMetadata{
			Id:  "inst-no-proxy",
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "old"},
		},
	}
	prevEnv := cloneEnvMap(meta.Env)
	nextEnv := map[string]string{"OUTBOUND_OPENAI_KEY": "new"}

	saveCalls := 0
	err := applyUpdatedInstanceEnv(context.Background(), nil, meta.Id, meta, prevEnv, nextEnv, func(saved *metadata) error {
		saveCalls++
		assert.Equal(t, nextEnv, saved.Env)
		return nil
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, saveCalls)
	assert.Equal(t, nextEnv, meta.Env)
}

func TestApplyUpdatedInstanceEnvRollsBackRulesOnSaveFailure(t *testing.T) {
	t.Parallel()

	meta := &metadata{
		StoredMetadata: StoredMetadata{
			Id:            "inst-save-rollback",
			NetworkEgress: &NetworkEgressPolicy{Enabled: true},
			Credentials: map[string]CredentialPolicy{
				"OUTBOUND_OPENAI_KEY": {
					Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
					Inject: []CredentialInjectRule{{
						As: CredentialInjectAs{
							Header: "Authorization",
							Format: "Bearer ${value}",
						},
					}},
				},
			},
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "old"},
		},
	}
	prevEnv := cloneEnvMap(meta.Env)
	nextEnv := map[string]string{"OUTBOUND_OPENAI_KEY": "new"}
	svc := &fakeUpdateInstanceRulesService{}
	saveErr := errors.New("disk full")

	err := applyUpdatedInstanceEnv(context.Background(), nil, meta.Id, meta, prevEnv, nextEnv, func(*metadata) error {
		return saveErr
	}, svc)
	require.Error(t, err)
	assert.ErrorContains(t, err, "save metadata")
	assert.ErrorContains(t, err, saveErr.Error())
	assert.Equal(t, prevEnv, meta.Env)
	require.Len(t, svc.calls, 2)
	assert.Equal(t, buildEgressProxyInjectRules(meta.NetworkEgress, meta.Credentials, nextEnv), svc.calls[0])
	assert.Equal(t, buildEgressProxyInjectRules(meta.NetworkEgress, meta.Credentials, prevEnv), svc.calls[1])
}

func TestApplyUpdatedInstanceEnvReturnsRollbackFailure(t *testing.T) {
	t.Parallel()

	meta := &metadata{
		StoredMetadata: StoredMetadata{
			Id:            "inst-double-failure",
			NetworkEgress: &NetworkEgressPolicy{Enabled: true},
			Credentials: map[string]CredentialPolicy{
				"OUTBOUND_OPENAI_KEY": {
					Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
					Inject: []CredentialInjectRule{{
						As: CredentialInjectAs{
							Header: "Authorization",
							Format: "Bearer ${value}",
						},
					}},
				},
			},
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "old"},
		},
	}
	prevEnv := cloneEnvMap(meta.Env)
	nextEnv := map[string]string{"OUTBOUND_OPENAI_KEY": "new"}
	saveErr := errors.New("save failed")
	rollbackErr := errors.New("rollback failed")
	svc := &fakeUpdateInstanceRulesService{errs: []error{nil, rollbackErr}}

	err := applyUpdatedInstanceEnv(context.Background(), nil, meta.Id, meta, prevEnv, nextEnv, func(*metadata) error {
		return saveErr
	}, svc)
	require.Error(t, err)
	assert.ErrorContains(t, err, "save metadata")
	assert.ErrorContains(t, err, saveErr.Error())
	assert.ErrorContains(t, err, rollbackErr.Error())
	assert.Equal(t, prevEnv, meta.Env)
	require.Len(t, svc.calls, 2)
}
