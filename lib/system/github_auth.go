package system

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type githubRelease struct {
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

func doDownloadRequest(ctx context.Context, client *http.Client, rawURL string) (*http.Response, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}

	repo, tag, asset, ok := githubReleaseAsset(rawURL)
	if !ok {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}

	metadataURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, url.PathEscape(tag))
	metadataReq, err := githubAPIRequest(ctx, metadataURL, token, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	metadataResp, err := client.Do(metadataReq)
	if err != nil {
		return nil, fmt.Errorf("get release metadata: %w", err)
	}
	defer metadataResp.Body.Close()
	if metadataResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get release metadata: status %d", metadataResp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(metadataResp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode release metadata: %w", err)
	}
	for _, candidate := range release.Assets {
		if candidate.Name != asset {
			continue
		}
		assetReq, err := githubAPIRequest(ctx, candidate.URL, token, "application/octet-stream")
		if err != nil {
			return nil, err
		}
		return client.Do(assetReq)
	}
	return nil, fmt.Errorf("release asset %q not found", asset)
}

func githubAPIRequest(ctx context.Context, rawURL, token, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

func githubReleaseAsset(rawURL string) (repo, tag, asset string, ok bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() != "github.com" {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 6 || parts[2] != "releases" || parts[3] != "download" {
		return "", "", "", false
	}
	tag, err = url.PathUnescape(parts[4])
	if err != nil {
		return "", "", "", false
	}
	asset, err = url.PathUnescape(parts[5])
	if err != nil {
		return "", "", "", false
	}
	return parts[0] + "/" + parts[1], tag, asset, true
}
