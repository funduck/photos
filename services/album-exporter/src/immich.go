package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Asset is the subset of Immich's AssetResponseDto the exporter needs.
type Asset struct {
	ID               string    `json:"id"`
	OriginalFileName string    `json:"originalFileName"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	LivePhotoVideoID string    `json:"livePhotoVideoId"`
}

type album struct {
	ID        string `json:"id"`
	AlbumName string `json:"albumName"`
}

// Immich is a minimal client for the handful of endpoints used here. The API
// key needs album.read, asset.read and asset.download.
type Immich struct {
	base string
	key  string
	http *http.Client
}

func NewImmich(base, key string, hc *http.Client) *Immich {
	if hc == nil {
		// No overall timeout: a long video is a long download, and the context
		// already bounds it. Only a server that never answers is cut short.
		hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: time.Minute,
		}}
	}
	return &Immich{base: strings.TrimRight(base, "/"), key: key, http: hc}
}

// AlbumID resolves an album name or UUID to its ID. It runs every pass, so a
// renamed or deleted album fails loudly instead of silently exporting nothing.
func (c *Immich) AlbumID(ctx context.Context, nameOrID string) (string, error) {
	var albums []album
	// With no "shared" filter Immich returns albums owned by and shared with
	// the key's user alike.
	if err := c.getJSON(ctx, "/api/albums", &albums); err != nil {
		return "", err
	}
	var matches []string
	for _, a := range albums {
		if a.ID == nameOrID {
			return a.ID, nil
		}
		if a.AlbumName == nameOrID {
			matches = append(matches, a.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no album named %q is visible to this API key", nameOrID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d albums are named %q: use the album ID instead (%s)",
			len(matches), nameOrID, strings.Join(matches, ", "))
	}
}

// AlbumAssets lists every asset in an album. It pages through metadata search
// rather than reading the album itself: search is paginated, and does not
// depend on whether the album endpoint still embeds its assets.
func (c *Immich) AlbumAssets(ctx context.Context, albumID string) ([]Asset, error) {
	var out []Asset
	for page := 1; ; {
		req := map[string]any{"albumIds": []string{albumID}, "page": page, "size": 1000}
		var resp struct {
			Assets struct {
				Items    []Asset `json:"items"`
				NextPage *string `json:"nextPage"`
			} `json:"assets"`
		}
		if err := c.postJSON(ctx, "/api/search/metadata", req, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Assets.Items...)
		if resp.Assets.NextPage == nil || *resp.Assets.NextPage == "" {
			return out, nil
		}
		next, err := strconv.Atoi(*resp.Assets.NextPage)
		if err != nil || next <= page {
			return nil, fmt.Errorf("unexpected nextPage %q after page %d", *resp.Assets.NextPage, page)
		}
		page = next
	}
}

// Asset fetches one asset. Live photo videos are hidden from search, so this
// is how their file names are found.
func (c *Immich) Asset(ctx context.Context, id string) (Asset, error) {
	var a Asset
	err := c.getJSON(ctx, "/api/assets/"+url.PathEscape(id), &a)
	return a, err
}

// Original streams an asset's original file. The caller closes the body.
func (c *Immich) Original(ctx context.Context, id string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/assets/"+url.PathEscape(id)+"/original", nil, "*/*")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Immich) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

func (c *Immich) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("POST %s: decode: %w", path, err)
	}
	return nil
}

// do sends a request and turns any non-2xx answer into an error carrying the
// start of the response body, which is where Immich explains itself.
func (c *Immich) do(ctx context.Context, method, path string, body []byte, accept string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}
