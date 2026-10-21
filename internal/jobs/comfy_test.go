package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func pngBytes() []byte {
	var b bytes.Buffer

	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))

	return b.Bytes()
}

func newFakeComfy(t *testing.T, handler http.HandlerFunc) *Comfy {
	t.Helper()

	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)

	c, err := NewComfy(s.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestComfySubmitCarriesDurableID(t *testing.T) {
	c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any

		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["prompt_id"] != "durable-id" {
			t.Error("missing durable ID")
		}

		_, _ = w.Write([]byte(`{"prompt_id":"durable-id"}`))
	})
	if err := c.Submit(context.Background(), "durable-id", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestComfySubmissionErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		rejected bool
	}{
		{
			name:     "validation",
			status:   http.StatusBadRequest,
			body:     `{}`,
			rejected: true,
		},
		{
			name:     "server",
			status:   http.StatusInternalServerError,
			body:     `{}`,
			rejected: false,
		},
		{
			name:     "malformed",
			status:   http.StatusOK,
			body:     `broken`,
			rejected: false,
		},
		{
			name:     "mismatch",
			status:   http.StatusOK,
			body:     `{"prompt_id":"other"}`,
			rejected: false,
		},
		{
			name:     "redirect",
			status:   http.StatusFound,
			body:     `{}`,
			rejected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) {
				calls++

				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			})
			err := c.Submit(context.Background(), "id", []byte(`{}`))

			var rejected *Rejected

			if err == nil || errors.As(err, &rejected) != test.rejected || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestComfyInspect(t *testing.T) {
	tests := []struct {
		name    string
		history string
		queue   string
		want    string
	}{
		{
			name: "done",
			history: `{
  "id": {
    "status": {
      "completed": true,
      "status_str": "success"
    },
    "outputs": {
      "9": {
        "images": [
          {
            "filename": "one.png",
            "type": "output"
          }
        ]
      }
    }
  }
}`,
			queue: `{}`,
			want:  "completed",
		},
		{
			name:    "failed",
			history: `{"id":{"status":{"status_str":"error"}}}`,
			queue:   `{}`,
			want:    "failed",
		},
		{
			name:    "queued",
			history: `{}`,
			queue:   `{"queue_pending":[[0,"id",{},{}]],"queue_running":[]}`,
			want:    "pending",
		},
		{
			name:    "running",
			history: `{}`,
			queue:   `{"queue_pending":[],"queue_running":[[0,"id",{},{}]]}`,
			want:    "running",
		},
		{
			name:    "missing",
			history: `{}`,
			queue:   `{"queue_pending":[],"queue_running":[]}`,
			want:    "missing",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/queue" {
					_, _ = w.Write([]byte(test.queue))
				} else {
					_, _ = w.Write([]byte(test.history))
				}
			})

			got, err := c.Inspect(context.Background(), "id")
			if err != nil || got.State != test.want {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestComfyFetchRejectsUnsafeReferencesAndInvalidImages(t *testing.T) {
	c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not a PNG")) })
	refs := []Ref{
		{Filename: "../secret", Type: "output"},
		{Filename: "file.png", Subfolder: "../secret", Type: "output"},
		{Filename: "file.png", Type: "input"},
		{Filename: "file.png", Type: "output"},
	}

	for _, ref := range refs {
		_, err := c.Fetch(context.Background(), ref)

		var rejected *Rejected

		if !errors.As(err, &rejected) {
			t.Fatalf("ref=%+v err=%v", ref, err)
		}
	}
}

func TestComfyFetchPNG(t *testing.T) {
	c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(pngBytes()) })

	got, err := c.Fetch(context.Background(), Ref{Filename: "one.png", Type: "output"})
	if err != nil || got.Type != "image/png" || len(got.Data) == 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestComfyRejectsBadURL(t *testing.T) {
	urls := []string{
		"file:///tmp",
		"http://user:password@localhost",
		"http://localhost?q=x",
		"http://localhost/path",
	}

	for _, base := range urls {
		if _, err := NewComfy(base, time.Second); err == nil {
			t.Fatal(base)
		}
	}
}

func TestComfyAcceptedThenConnectionLost(t *testing.T) {
	var calls atomic.Int32

	c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}

		connection.Close()
	})
	err := c.Submit(context.Background(), "id", []byte(`{}`))

	var rejected *Rejected

	if err == nil || errors.As(err, &rejected) || calls.Load() != 1 {
		t.Fatalf("lost response: %v calls=%d", err, calls.Load())
	}
}

func TestComfyCompletionBetweenHistoryAndQueue(t *testing.T) {
	histories := 0
	c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/queue" {
			_, _ = w.Write([]byte(`{"queue_pending":[],"queue_running":[]}`))
			return
		}

		histories++
		if histories == 1 {
			_, _ = w.Write([]byte(`{}`))
			return
		}

		_, _ = w.Write([]byte(`{"id":{"status":{"completed":true,"status_str":"success"},"outputs":{}}}`))
	})

	result, err := c.Inspect(context.Background(), "id")
	if err != nil || result.State != "completed" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestComfyRejectsImageDecompressionBomb(t *testing.T) {
	var b bytes.Buffer

	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2048, 2048)))
	c := newFakeComfy(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b.Bytes()) })
	_, err := c.Fetch(context.Background(), Ref{Filename: "image.png", Type: "output"})

	var rejected *Rejected

	if !errors.As(err, &rejected) {
		t.Fatalf("oversized pixels accepted: %v", err)
	}
}

func TestComfySubmissionTimeoutIsUncertain(t *testing.T) {
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		time.Sleep(50 * time.Millisecond)

		_, _ = w.Write([]byte(`{"prompt_id":"id"}`))
	}))
	defer server.Close()

	c, err := NewComfy(server.URL, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	err = c.Submit(context.Background(), "id", []byte(`{}`))

	var rejected *Rejected

	if err == nil || errors.As(err, &rejected) {
		t.Fatalf("timeout: %v", err)
	}

	server.Close()

	if calls != 1 {
		t.Fatal("POST retried")
	}
}
