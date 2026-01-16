// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/internal/testutil"
	"github.com/onkernel/hypeman-go/option"
)

func TestInstanceNewWithOptionalParams(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.New(context.TODO(), hypeman.InstanceNewParams{
		Image:     "docker.io/library/alpine:latest",
		Name:      "my-workload-1",
		Devices:   []string{"l4-gpu"},
		DiskIoBps: hypeman.String("100MB/s"),
		Env: map[string]string{
			"PORT":     "3000",
			"NODE_ENV": "production",
		},
		GPU: hypeman.InstanceNewParamsGPU{
			Profile: hypeman.String("L40S-1Q"),
		},
		HotplugSize: hypeman.String("2GB"),
		Hypervisor:  hypeman.InstanceNewParamsHypervisorCloudHypervisor,
		Network: hypeman.InstanceNewParamsNetwork{
			BandwidthDownload: hypeman.String("1Gbps"),
			BandwidthUpload:   hypeman.String("1Gbps"),
			Enabled:           hypeman.Bool(true),
		},
		OverlaySize: hypeman.String("20GB"),
		Size:        hypeman.String("2GB"),
		Vcpus:       hypeman.Int(2),
		Volumes: []hypeman.VolumeMountParam{{
			MountPath:   "/mnt/data",
			VolumeID:    "vol-abc123",
			Overlay:     hypeman.Bool(true),
			OverlaySize: hypeman.String("1GB"),
			Readonly:    hypeman.Bool(true),
		}},
	})
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceList(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.List(context.TODO())
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceDelete(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	err := client.Instances.Delete(context.TODO(), "id")
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceGet(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.Get(context.TODO(), "id")
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceRestore(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.Restore(context.TODO(), "id")
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceStandby(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.Standby(context.TODO(), "id")
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceStart(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.Start(context.TODO(), "id")
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceStatWithOptionalParams(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.Stat(
		context.TODO(),
		"id",
		hypeman.InstanceStatParams{
			Path:        "path",
			FollowLinks: hypeman.Bool(true),
		},
	)
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}

func TestInstanceStop(t *testing.T) {
	t.Skip("Prism tests are disabled")
	baseURL := "http://localhost:4010"
	if envURL, ok := os.LookupEnv("TEST_API_BASE_URL"); ok {
		baseURL = envURL
	}
	if !testutil.CheckTestServer(t, baseURL) {
		return
	}
	client := hypeman.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("My API Key"),
	)
	_, err := client.Instances.Stop(context.TODO(), "id")
	if err != nil {
		var apierr *hypeman.Error
		if errors.As(err, &apierr) {
			t.Log(string(apierr.DumpRequest(true)))
		}
		t.Fatalf("err should be nil: %s", err.Error())
	}
}
