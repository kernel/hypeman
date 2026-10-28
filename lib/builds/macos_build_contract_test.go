package builds

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestMachineRecipeContract(t *testing.T) {
	valid := `{"version":1,"steps":[{"command":["/bin/sh","build.sh"],"env":{"MODE":"qa"}}]}`
	cases := map[string]string{
		"valid":                valid,
		"case variant version": `{"Version":1,"steps":[{"command":["/bin/true"]}]}`,
		"case variant command": `{"version":1,"steps":[{"Command":["/bin/true"]}]}`,
		"duplicate version":    `{"version":2,"version":1,"steps":[{"command":["/bin/true"]}]}`,
		"duplicate command":    `{"version":1,"steps":[{"command":["/bin/false"],"command":["/bin/true"]}]}`,
		"duplicate env":        `{"version":1,"steps":[{"command":["/bin/true"],"env":{"KEY":"one","KEY":"two"}}]}`,
		"null argument":        `{"version":1,"steps":[{"command":["/bin/echo",null]}]}`,
		"null env":             `{"version":1,"steps":[{"command":["/bin/true"],"env":{"KEY":null}}]}`,
		"empty steps":          `{"version":1,"steps":[]}`,
		"relative executable":  `{"version":1,"steps":[{"command":["sh"]}]}`,
		"unknown step field":   `{"version":1,"steps":[{"command":["/bin/true"],"cwd":"/tmp"}]}`,
		"unknown top field":    `{"version":1,"steps":[{"command":["/bin/true"]}],"secret":"no"}`,
		"invalid env name":     `{"version":1,"steps":[{"command":["/bin/true"],"env":{"A=B":"x"}}]}`,
		"nul argument":         `{"version":1,"steps":[{"command":["/bin/echo","\u0000"]}]}`,
		"invalid UTF-8":        strings.Replace(valid, "qa", "\xff", 1),
		"trailing object":      valid + `{}`,
		"oversize":             strings.Replace(valid, "qa", strings.Repeat("x", 64<<10), 1),
		"oversize argument":    strings.Replace(valid, "build.sh", strings.Repeat("x", 8193), 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			source := writeMachineSource(t, []*tar.Header{{Name: machineRecipeName, Typeflag: tar.TypeReg, Size: int64(len(body))}}, []string{body})
			root := filepath.Join(t.TempDir(), "source")
			recipe, err := extractMachineRecipe(context.Background(), source, root)
			if name != "valid" {
				require.Error(t, err)
				require.NoDirExists(t, root)
				return
			}
			require.NoError(t, err)
			digest := sha256.Sum256([]byte(body))
			require.Equal(t, hex.EncodeToString(digest[:]), recipe.hash)
		})
	}
	for _, count := range []int{32, 33} {
		body := `{"version":1,"steps":[` + strings.TrimSuffix(strings.Repeat(`{"command":["/bin/true"]},`, count), ",") + `]}`
		source := writeMachineSource(t, []*tar.Header{{Name: machineRecipeName, Typeflag: tar.TypeReg, Size: int64(len(body))}}, []string{body})
		_, err := extractMachineRecipe(context.Background(), source, filepath.Join(t.TempDir(), "source"))
		require.Equal(t, count > 32, err != nil)
	}
}

func TestMachineRecipeProgressDoesNotLogCommandsOrErrors(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var lines []string
			ctx := context.WithValue(context.Background(), machineProgressKey{}, func(p machineProgress) { lines = append(lines, p.line()) })
			recipe := &machineRecipe{Steps: []machineRecipeStep{
				{Command: []string{"/bin/echo", "secret-command"}, Env: map[string]string{"TOKEN": "secret-env"}},
				{Command: []string{"/bin/false"}},
			}}
			err := runMachineRecipe(ctx, recipe, func(context.Context, machineRecipeStep) error {
				if fail {
					return errors.New("secret-guest-error")
				}
				return nil
			})
			if fail {
				require.Error(t, err)
				require.Equal(t, []string{"machine build: recipe step 1/2 starting"}, lines)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"machine build: recipe step 1/2 starting", "machine build: recipe step 1/2 completed", "machine build: recipe step 2/2 starting", "machine build: recipe step 2/2 completed"}, lines)
			}
			require.NotContains(t, strings.Join(lines, "\n"), "secret")
		})
	}
	for _, p := range []machineProgress{{phase: -1}, {phase: machineStepStarting}, {phase: machineStepStarting, step: 1, total: 33}, {phase: machineStepCompleted, step: 2, total: 1}} {
		require.Empty(t, p.line())
	}
	err := runMachineRecipe(context.Background(), &machineRecipe{Steps: []machineRecipeStep{{}, {}}}, func(context.Context, machineRecipeStep) error { return &machineExitError{code: 7} })
	require.EqualError(t, err, "recipe step 1 exited with code 7")
}

func TestMachineBuildProgressUsesSharedLogsAndSSE(t *testing.T) {
	for _, failure := range []string{"", "provision"} {
		t.Run(failure, func(t *testing.T) {
			f := newMachineFixture()
			f.fail = failure
			m := newMachineTestManager(t, f, &machineJobPublisher{fixture: f}, nil, false)
			build, err := m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("source"))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				b, err := m.GetBuild(context.Background(), build.ID)
				return err == nil && (b.Status == StatusReady || b.Status == StatusFailed)
			}, 5*time.Second, 10*time.Millisecond)
			data, err := m.GetBuildLogs(context.Background(), build.ID)
			require.NoError(t, err)
			logs := string(data)
			require.Contains(t, logs, "machine build: starting builder\n")
			require.NotContains(t, logs, "sensitive-command-argument")
			if failure == "" {
				require.Contains(t, logs, "machine build: publication verified\n")
			} else {
				require.Contains(t, logs, "machine build: failed\n")
				require.NotContains(t, logs, "machine build: publishing")
			}
			events, err := m.StreamBuildEvents(context.Background(), build.ID, false)
			require.NoError(t, err)
			var replay strings.Builder
			for event := range events {
				if event.Type == EventTypeLog {
					replay.WriteString(event.Content + "\n")
				}
			}
			require.Equal(t, logs, replay.String())
		})
	}
}

type machineEvidenceFixture struct {
	*machineFixture
}

func (f *machineEvidenceFixture) machineRecipeHash() string { return strings.Repeat("a", 64) }
func (f *machineEvidenceFixture) Start(ctx context.Context, name string, base *images.Image, policy BuildPolicy) (MachineBuildSession, error) {
	_, err := f.machineFixture.Start(ctx, name, base, policy)
	return f, err
}

func TestMachineRecipeProvenanceRoundTrip(t *testing.T) {
	f := &machineEvidenceFixture{machineFixture: newMachineFixture()}
	backend := MachineBuildBackend{Driver: f, Publisher: f, Storage: testMachineStorage(t)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "source.tar.gz")
	hash, err := stageBuildSource(ctx, strings.NewReader("source"), source, maxBuildSourceBytes)
	require.NoError(t, err)
	req := machineTestRequest()
	req.SourceHash = hash
	result, err := backend.runPrepared(ctx, req, f.image, source, root)
	require.NoError(t, err)
	require.Equal(t, f.machineRecipeHash(), result.Provenance.RecipeHash)
	require.Equal(t, machineTestDigest, result.Provenance.BaseImageDigest)
	require.Empty(t, result.Provenance.BuildkitVersion)
	require.Empty(t, result.Provenance.LockfileHashes)

	// Recipe bytes are hashed before execution; no command output or inferred
	// toolchain version belongs in provenance.
	session := &guestMachineSession{recipeHash: strings.Repeat("a", 64)}
	provenance := BuildProvenance{RecipeHash: session.machineRecipeHash()}
	encoded, err := json.Marshal(provenance)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"recipe_hash":"`+session.recipeHash+`"`)
	require.NotContains(t, string(encoded), "buildkit_version")
	var decoded BuildProvenance
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, provenance, decoded)
}
