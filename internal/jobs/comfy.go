package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

type Comfy struct {
	base string
	http *http.Client
}

func NewComfy(base string, timeout time.Duration) (*Comfy, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}

	if (u.Scheme != "http" && u.Scheme != "https") ||
		u.Host == "" ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") ||
		timeout <= 0 {
		return nil, fmt.Errorf("ComfyUI URL must be an HTTP(S) origin without credentials, path or query")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil

	return &Comfy{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Comfy) call(ctx context.Context, method, route string, payload, result any) error {
	var reader io.Reader

	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+route, reader)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Never include upstream bodies: they can contain prompts or internal paths.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 {
			return &Rejected{fmt.Sprintf("upstream HTTP %d", resp.StatusCode)}
		}

		return fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return err
	}

	if len(data) > 4<<20 {
		return fmt.Errorf("upstream response too large")
	}

	return json.Unmarshal(data, result)
}

func (c *Comfy) Submit(ctx context.Context, id string, workflow []byte) error {
	var response struct {
		ID string `json:"prompt_id"`
	}

	err := c.call(ctx, "POST", "/prompt", map[string]any{"prompt_id": id, "prompt": json.RawMessage(workflow)}, &response)
	if err != nil {
		return err
	}

	if response.ID != id {
		return fmt.Errorf("upstream did not echo durable prompt ID")
	}

	return nil
}

type historyEntry struct {
	Status struct {
		Completed bool   `json:"completed"`
		State     string `json:"status_str"`
	} `json:"status"`
	Outputs map[string]struct {
		Images []Ref `json:"images"`
	} `json:"outputs"`
}

func (entry historyEntry) remote() Remote {
	if entry.Status.State == "error" {
		return Remote{State: "failed"}
	}

	if !entry.Status.Completed || entry.Status.State != "success" {
		return Remote{State: "running"}
	}

	result := Remote{State: "completed"}

	keys := make([]string, 0, len(entry.Outputs))
	for key := range entry.Outputs {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		result.Images = append(result.Images, entry.Outputs[key].Images...)
	}

	return result
}

func (c *Comfy) Inspect(ctx context.Context, id string) (Remote, error) {
	var history map[string]historyEntry

	if err := c.call(ctx, "GET", "/history/"+url.PathEscape(id), nil, &history); err != nil {
		return Remote{}, err
	}

	if entry, ok := history[id]; ok {
		return entry.remote(), nil
	}

	var queue struct {
		Running [][]json.RawMessage `json:"queue_running"`
		Pending [][]json.RawMessage `json:"queue_pending"`
	}

	if err := c.call(ctx, "GET", "/queue", nil, &queue); err != nil {
		return Remote{}, err
	}

	for _, group := range []struct {
		items [][]json.RawMessage
		state string
	}{{queue.Running, "running"}, {queue.Pending, "pending"}} {
		for _, item := range group.items {
			if len(item) < 2 {
				return Remote{}, fmt.Errorf("invalid upstream queue entry")
			}

			var found string

			if err := json.Unmarshal(item[1], &found); err != nil {
				return Remote{}, err
			}

			if found == id {
				return Remote{State: group.state}, nil
			}
		}
	}

	// Completion can occur between the history and queue reads. Recheck history
	// once to avoid treating that ordinary transition as a lost task.
	if err := c.call(ctx, "GET", "/history/"+url.PathEscape(id), nil, &history); err != nil {
		return Remote{}, err
	}

	if entry, ok := history[id]; ok {
		return entry.remote(), nil
	}

	return Remote{State: "missing"}, nil
}

func (c *Comfy) Fetch(ctx context.Context, ref Ref) (Image, error) {
	if ref.Filename == "" ||
		path.Base(ref.Filename) != ref.Filename ||
		strings.Contains(ref.Filename, "\\") ||
		ref.Type != "output" ||
		path.IsAbs(ref.Subfolder) ||
		strings.Contains(ref.Subfolder, "\\") {
		return Image{}, &Rejected{"unsafe image reference"}
	}

	for _, part := range strings.Split(ref.Subfolder, "/") {
		if part == ".." {
			return Image{}, &Rejected{"unsafe image subfolder"}
		}
	}

	q := url.Values{"filename": {ref.Filename}, "subfolder": {ref.Subfolder}, "type": {"output"}}

	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/view?"+q.Encode(), nil)
	if err != nil {
		return Image{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Image{}, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if resp.StatusCode == 404 {
			return Image{}, &Rejected{"image no longer available"}
		}

		return Image{}, fmt.Errorf("image HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, (20<<20)+1))
	if err != nil {
		return Image{}, err
	}

	if len(data) > 20<<20 {
		return Image{}, &Rejected{"image exceeds 20 MiB"}
	}

	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 1024*1024 {
		return Image{}, &Rejected{"invalid or oversized PNG"}
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, &Rejected{"corrupt PNG"}
	}

	// Re-encode pixels to strip upstream metadata containing prompts/workflows.
	var clean bytes.Buffer

	if err = png.Encode(&clean, img); err != nil {
		return Image{}, err
	}

	return Image{Data: clean.Bytes(), Type: "image/png"}, nil
}

func (c *Comfy) Ready(ctx context.Context) error {
	var stats map[string]any

	return c.call(ctx, "GET", "/system_stats", nil, &stats)
}
