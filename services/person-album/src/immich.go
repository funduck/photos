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

// Asset is the subset of Immich's AssetResponseDto used here.
type Asset struct {
	ID               string `json:"id"`
	OriginalFileName string `json:"originalFileName"`
}

type album struct {
	ID        string `json:"id"`
	AlbumName string `json:"albumName"`
}

type person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AddResult is one entry of Immich's BulkIdResponseDto. Error is one of
// duplicate, no_permission, not_found, unknown or validation.
type AddResult struct {
	ID           string `json:"id"`
	Success      bool   `json:"success"`
	Error        string `json:"error"`
	ErrorMessage string `json:"errorMessage"`
}

// Immich is a minimal client for the handful of endpoints used here. The API
// key needs person.read, asset.read, album.read and albumAsset.create.
type Immich struct {
	base string
	key  string
	http *http.Client
}

func NewImmich(base, key string, hc *http.Client) *Immich {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Immich{base: strings.TrimRight(base, "/"), key: key, http: hc}
}

// AlbumID resolves an album name or UUID to its ID. It runs every pass, so a
// renamed or deleted album fails loudly instead of silently adding nothing.
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

// People lists every person of the key's user, hidden ones included.
func (c *Immich) People(ctx context.Context) ([]person, error) {
	var out []person
	for page := 1; ; page++ {
		var resp struct {
			People      []person `json:"people"`
			HasNextPage bool     `json:"hasNextPage"`
		}
		path := "/api/people?withHidden=true&size=1000&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.People...)
		if !resp.HasNextPage || len(resp.People) == 0 {
			return out, nil
		}
	}
}

// PersonID resolves a person name or UUID against a People listing.
func PersonID(people []person, nameOrID string) (string, error) {
	var matches []string
	for _, p := range people {
		if p.ID == nameOrID {
			return p.ID, nil
		}
		if p.Name == nameOrID {
			matches = append(matches, p.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no person named %q is visible to this API key", nameOrID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d people are named %q: use the person ID instead (%s)",
			len(matches), nameOrID, strings.Join(matches, ", "))
	}
}

// PersonAssets lists the timeline assets that show every one of personIDs:
// Immich ANDs them. Archived, hidden (such as the video half of a live photo),
// locked and trashed assets are left out.
func (c *Immich) PersonAssets(ctx context.Context, personIDs []string) ([]Asset, error) {
	var out []Asset
	for page := 1; ; {
		req := map[string]any{
			"personIds":   personIDs,
			"visibility":  "timeline",
			"withDeleted": false,
			"page":        page,
			"size":        1000,
		}
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

// AddToAlbum adds assets to an album, reporting the outcome per asset.
func (c *Immich) AddToAlbum(ctx context.Context, albumID string, assetIDs []string) ([]AddResult, error) {
	var out []AddResult
	path := "/api/albums/" + url.PathEscape(albumID) + "/assets"
	err := c.sendJSON(ctx, http.MethodPut, path, map[string]any{"ids": assetIDs}, &out)
	return out, err
}

func (c *Immich) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
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
	return c.sendJSON(ctx, http.MethodPost, path, in, out)
}

func (c *Immich) sendJSON(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// do sends a request and turns any non-2xx answer into an error carrying the
// start of the response body, which is where Immich explains itself.
func (c *Immich) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
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
