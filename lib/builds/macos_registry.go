package builds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// machineRegistryRequest sends one authenticated registry request. The caller
// closes the response body.
func machineRegistryRequest(ctx context.Context, client *http.Client, token, method, target, media string, body io.Reader, size int64) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	request.ContentLength = size
	request.Header.Set("Authorization", "Bearer "+token)
	if media != "" {
		request.Header.Set("Content-Type", media)
	}
	request.Header.Set("Accept", string(types.OCIManifestSchema1))
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("machine registry request failed")
	}
	return response, nil
}

func machineHash(data []byte) v1.Hash {
	sum := sha256.Sum256(data)
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

// machineFetch GETs a registry object and checks it against digest. A size of 0
// marks metadata: the body is buffered up to maxMachineMetadataBytes and returned.
// Any other size must match exactly and is only hashed, never kept, so layers of
// tens of gigabytes are not held in memory.
func machineFetch(ctx context.Context, client *http.Client, token, target, digest string, size int64) ([]byte, error) {
	response, err := machineRegistryRequest(ctx, client, token, http.MethodGet, target, "", nil, 0)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned status %d", response.StatusCode)
	}
	metadata := size == 0
	limit := size + 1
	if metadata {
		limit = maxMachineMetadataBytes + 1
	}
	sum := sha256.New()
	var kept bytes.Buffer
	out := io.Writer(sum)
	if metadata {
		out = io.MultiWriter(sum, &kept)
	}
	n, err := io.Copy(out, io.LimitReader(response.Body, limit))
	if err != nil {
		return nil, err
	}
	if n >= limit || (!metadata && n != size) {
		return nil, fmt.Errorf("object does not match digest %s", digest)
	}
	if (v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(sum.Sum(nil))}).String() != digest {
		return nil, fmt.Errorf("object does not match digest %s", digest)
	}
	if !metadata {
		return nil, nil
	}
	return kept.Bytes(), nil
}

// machineBlobStored reports whether the registry already holds a blob of the
// descriptor's digest and size. The store is shared by digest across repositories.
func machineBlobStored(ctx context.Context, client *http.Client, token, base string, descriptor v1.Descriptor) (bool, error) {
	head, err := machineRegistryRequest(ctx, client, token, http.MethodHead, base+"/blobs/"+descriptor.Digest.String(), "", nil, 0)
	if err != nil {
		return false, err
	}
	head.Body.Close()
	switch head.StatusCode {
	case http.StatusOK:
		if head.ContentLength != descriptor.Size {
			return false, fmt.Errorf("stored blob %s has size %d, want %d", descriptor.Digest, head.ContentLength, descriptor.Size)
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("blob check returned status %d", head.StatusCode)
}
