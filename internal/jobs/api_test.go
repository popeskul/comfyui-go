package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-with-at-least-32-characters"

type apiFixture struct {
	*API
	store  *Store
	worker *Worker
}

func apiTest(t *testing.T, capacity int) *apiFixture {
	t.Helper()

	template, err := os.ReadFile("../../workflow.json")
	if err != nil {
		t.Fatal(err)
	}

	s := openTest(t, capacity)

	worker := &Worker{s, &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
	}, time.Hour}

	api, err := NewAPI(s, worker, template, testToken)
	if err != nil {
		t.Fatal(err)
	}

	return &apiFixture{API: api, store: s, worker: worker}
}

func request(api http.Handler, method, path, body, key, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}

	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}

	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)

	return w
}

func TestAPIAuth(t *testing.T) {
	api := apiTest(t, 4)

	for _, token := range []string{"", "bad"} {
		w := request(api, "POST", "/v1/jobs", `{"prompt":"cup"}`, "request-01", token)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}

	if w := request(api, "GET", "/healthz", "", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestAPIIdempotencyAndConflict(t *testing.T) {
	api := apiTest(t, 1)

	a := request(api, "POST", "/v1/jobs", `{"prompt":"cup"}`, "request-01", testToken)
	if a.Code != 202 {
		t.Fatalf("%d %s", a.Code, a.Body)
	}

	var job Job

	_ = json.Unmarshal(a.Body.Bytes(), &job)

	b := request(api, "POST", "/v1/jobs", `{"prompt":"cup"}`, "request-01", testToken)
	if b.Code != 200 || !strings.Contains(b.Body.String(), job.ID) {
		t.Fatalf("%d %s", b.Code, b.Body)
	}

	c := request(api, "POST", "/v1/jobs", `{"prompt":"other"}`, "request-01", testToken)
	if c.Code != 409 {
		t.Fatal(c.Code)
	}

	d := request(api, "POST", "/v1/jobs", `{"prompt":"cup"}`, "request-02", testToken)
	if d.Code != 429 {
		t.Fatal(d.Code)
	}

	if strings.Contains(a.Body.String(), "workflow") || strings.Contains(a.Body.String(), "cup") {
		t.Fatal("private prompt leaked")
	}
}

func TestAPIRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name, body, key string
		code            int
	}{{
		name: "missing-prompt",
		body: `{}`,
		key:  "request-01",
		code: 400,
	}, {
		name: "unknown",
		body: `{"prompt":"cup","url":"http://internal"}`,
		key:  "request-01",
		code: 400,
	}, {
		name: "trailing",
		body: `{"prompt":"cup"}{}`,
		key:  "request-01",
		code: 400,
	}, {
		name: "bad-size",
		body: `{"prompt":"cup","width":513}`,
		key:  "request-01",
		code: 400,
	}, {
		name: "too-many-steps",
		body: `{"prompt":"cup","steps":999}`,
		key:  "request-01",
		code: 400,
	}, {
		name: "bad-saturation",
		body: `{"prompt":"cup","saturation":3}`,
		key:  "request-01",
		code: 400,
	}, {
		name: "no-key",
		body: `{"prompt":"cup"}`,
		key:  "",
		code: 400,
	}, {
		name: "huge",
		body: `{"prompt":"` + strings.Repeat("x", 40000) + `"}`,
		key:  "request-01",
		code: 413,
	}, {
		name: "bad-key",
		body: `{"prompt":"cup"}`,
		key:  "bad key!",
		code: 400,
	}, {
		name: "empty-body",
		body: ``,
		key:  "request-01",
		code: 400,
	}} {
		t.Run(test.name, func(t *testing.T) {
			w := request(apiTest(t, 4), "POST", "/v1/jobs", test.body, test.key, testToken)
			if w.Code != test.code {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestAPIGetCancelAndMissing(t *testing.T) {
	api := apiTest(t, 4)
	a := request(api, "POST", "/v1/jobs", `{"prompt":"cup"}`, "request-01", testToken)

	var j Job

	_ = json.Unmarshal(a.Body.Bytes(), &j)
	for _, method := range []string{"GET", "DELETE", "DELETE"} {
		w := request(api, method, "/v1/jobs/"+j.ID, "", "", testToken)
		if w.Code != 200 {
			t.Fatalf("%s %d %s", method, w.Code, w.Body)
		}
	}

	w := request(api, "GET", "/v1/jobs/00000000-0000-4000-8000-000000000000", "", "", testToken)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestAPIImagesProtected(t *testing.T) {
	api := apiTest(t, 4)
	j := createTest(t, api.store, "one")

	_ = api.store.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")

	_ = api.store.Transition(context.Background(), j.ID, "submitting", "running", j.ID, "")

	_ = api.store.Finish(context.Background(), j.ID, []Image{{Data: pngBytes(), Type: "image/png"}})

	path := "/v1/jobs/" + j.ID + "/images/0"
	if w := request(api, "GET", path, "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}

	w := request(api, "GET", path, "", "", testToken)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

func TestAPIRateLimit(t *testing.T) {
	api := apiTest(t, 100)
	limited := false

	for i := 0; i < 30; i++ {
		w := request(api, "POST", "/v1/jobs", `{"prompt":"cup"}`, "request-01", testToken)
		if w.Code == 429 {
			limited = true
		}
	}

	if !limited {
		t.Fatal("unbounded admission rate")
	}
}

func TestAPIBadConfiguration(t *testing.T) {
	s := openTest(t, 4)

	for _, token := range []string{"", "short"} {
		if _, err := NewAPI(s, Worker{}, []byte(`{}`), token); err == nil {
			t.Fatal("insecure token accepted")
		}
	}

	if _, err := NewAPI(s, Worker{}, []byte(`{}`), testToken); err == nil {
		t.Fatal("invalid template accepted")
	}
}

func TestAPIWrongMediaAndMethods(t *testing.T) {
	api := apiTest(t, 4)

	r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(`{"prompt":"cup"}`))

	r.Header.Set("Authorization", "Bearer "+testToken)

	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)

	if w.Code != 415 {
		t.Fatal(w.Code)
	}

	w = request(api, "PUT", "/v1/jobs", "", "", testToken)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestAPIAbandonOnlyUncertainJobs(t *testing.T) {
	api := apiTest(t, 4)
	j := createTest(t, api.store, "one")

	path := "/v1/jobs/" + j.ID + "/abandon"
	if w := request(api, "POST", path, "", "", testToken); w.Code != 409 {
		t.Fatalf("queued abandon: %d", w.Code)
	}

	_ = api.store.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")

	_ = api.store.Recover(context.Background())

	w := request(api, "POST", path, "", "", testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"failed"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAPIConcurrentReplay(t *testing.T) {
	api := apiTest(t, 32)
	ids := make(chan string, 20)

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			w := request(api, "POST", "/v1/jobs", `{"prompt":"concurrent cup"}`, "concurrent-key", testToken)
			if w.Code != 200 && w.Code != 202 {
				t.Errorf("%d %s", w.Code, w.Body)
				return
			}

			var j Job

			if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
				t.Error(err)
				return
			}

			ids <- j.ID
		}()
	}

	wg.Wait()
	close(ids)

	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}

		if first != id {
			t.Fatal("multiple HTTP jobs created")
		}
	}
}

func TestAPIReadinessDependencyDown(t *testing.T) {
	api := apiTest(t, 4)

	api.worker.Backend = &BackendMock{
		ReadyFunc: func(context.Context) error { return errors.New("offline") },
	}

	w := request(api, "GET", "/readyz", "", "", "")
	if w.Code != 503 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAPIReconcileDoesNotSubmit(t *testing.T) {
	api := apiTest(t, 4)

	j := createTest(t, api.store, "one")

	_ = api.store.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")

	_ = api.store.Recover(context.Background())

	backend := &BackendMock{
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "running"}, nil
		},
	}

	api.worker.Backend = backend

	w := request(api, "POST", "/v1/jobs/"+j.ID+"/reconcile", "", "", testToken)
	if w.Code != 200 || len(backend.SubmitCalls()) != 0 {
		t.Fatalf("%d posts=%d", w.Code, len(backend.SubmitCalls()))
	}
}

func TestAPIDrainStopsNewWorkButAllowsReads(t *testing.T) {
	api := apiTest(t, 4)
	j := createTest(t, api.store, "one")
	api.Drain()

	for _, path := range []string{"/readyz", "/v1/jobs"} {
		method, body, key := "GET", "", ""
		if path == "/v1/jobs" {
			method, body, key = "POST", `{"prompt":"cup"}`, "request-01"
		}

		w := request(api, method, path, body, key, testToken)
		if w.Code != 503 {
			t.Fatalf("draining %s: %d", path, w.Code)
		}
	}

	if w := request(api, "GET", "/v1/jobs/"+j.ID, "", "", testToken); w.Code != 200 {
		t.Fatal("read unavailable during drain", w.Code)
	}
}

func TestAPIBoundsConcurrentHandlers(t *testing.T) {
	api := apiTest(t, 4)

	entered := make(chan struct{}, 32)
	release := make(chan struct{})
	b := &BackendMock{
		ReadyFunc: func(ctx context.Context) error {
			entered <- struct{}{}

			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	api.worker.Backend = b

	var wg sync.WaitGroup

	defer func() { close(release); wg.Wait() }()

	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); request(api, "GET", "/readyz", "", "", "") }()
	}

	for i := 0; i < 32; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("handlers did not start")
		}
	}

	w := request(api, "GET", "/v1/jobs/00000000-0000-4000-8000-000000000000", "", "", testToken)
	if w.Code != 503 {
		t.Fatalf("unbounded concurrent handlers: %d", w.Code)
	}
}

func TestAPICatalogFormat(t *testing.T) {
	api := apiTest(t, 4)

	response := request(api, "POST", "/v1/jobs", `{"prompt":"cup","output_format":"portrait"}`, "catalog-001", testToken)
	if response.Code != 202 {
		t.Fatalf("catalog request: %d %s", response.Code, response.Body)
	}

	var job Job

	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}

	stored, err := api.store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}

	var graph map[string]struct {
		Inputs map[string]any `json:"inputs"`
	}

	if err := json.Unmarshal(stored.Workflow, &graph); err != nil {
		t.Fatal(err)
	}

	if graph["12"].Inputs["format"] != "portrait" {
		t.Fatal("catalog format was not persisted")
	}

	replay := request(api, "POST", "/v1/jobs", `{"prompt":"cup","output_format":"portrait"}`, "catalog-001", testToken)
	if replay.Code != 200 {
		t.Fatal("replay failed", replay.Code)
	}

	conflict := request(api, "POST", "/v1/jobs", `{"prompt":"cup","output_format":"square"}`, "catalog-001", testToken)
	if conflict.Code != 409 {
		t.Fatal("format change must conflict", conflict.Code)
	}

	invalid := request(api, "POST", "/v1/jobs", `{"prompt":"cup","output_format":"unknown"}`, "catalog-002", testToken)
	if invalid.Code != 400 {
		t.Fatal("invalid format accepted", invalid.Code)
	}
}
