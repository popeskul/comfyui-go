package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// APIStore is the HTTP handler's persistence contract. SQL stays in Store.
type APIStore interface {
	Create(context.Context, string, string, []byte) (Job, bool, error)
	Get(context.Context, string) (Job, error)
	Cancel(context.Context, string) error
	Image(context.Context, string, int) ([]byte, string, error)
	Ping(context.Context) error
	Counts(context.Context) (map[string]int, error)
}

// JobControl exposes operator actions without coupling HTTP to worker internals.
type JobControl interface {
	Abandon(context.Context, string) error
	Ready(context.Context) error
	Reconcile(context.Context, string) error
}

type API struct {
	inflight chan struct{}
	draining atomic.Bool
	store    APIStore
	control  JobControl
	template []byte
	token    [32]byte
	rateMu   sync.Mutex
	tokens   float64
	last     time.Time
}

var (
	keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	idPattern  = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
)

func NewAPI(store APIStore, control JobControl, template []byte, token string) (*API, error) {
	if len(token) < 32 || len(token) > 256 || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("API_TOKEN must contain 32 to 256 characters")
	}

	if err := validateWorkflow(template); err != nil {
		return nil, err
	}

	return &API{
		inflight: make(chan struct{}, 32),
		store:    store,
		control:  control,
		template: append([]byte(nil), template...),
		token:    sha256.Sum256([]byte(token)),
		tokens:   20,
		last:     time.Now(),
	}, nil
}

func (a *API) allow() bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()

	now := time.Now()
	a.tokens = math.Min(20, a.tokens+now.Sub(a.last).Seconds()*5)

	a.last = now
	if a.tokens < 1 {
		return false
	}

	a.tokens--

	return true
}

func jsonReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, code string) {
	jsonReply(w, status, map[string]string{"error": code})
}

func apiError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		problem(w, 404, "not_found")
	case errors.Is(err, ErrConflict):
		problem(w, 409, "state_or_idempotency_conflict")
	case errors.Is(err, ErrCapacity):
		w.Header().Set("Retry-After", "1")
		problem(w, 429, "capacity_reached")
	default:
		problem(w, 503, "temporarily_unavailable")
	}
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.draining.Load() && (r.URL.Path == "/readyz" || r.Method == "POST") {
		problem(w, 503, "service_draining")
		return
	}

	if r.URL.Path != "/healthz" {
		select {
		case a.inflight <- struct{}{}:
			defer func() { <-a.inflight }()
		default:
			problem(w, 503, "server_busy")
			return
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method == "GET" && r.URL.Path == "/healthz" {
		jsonReply(w, 200, map[string]string{"status": "ok"})
		return
	}

	if r.Method == "GET" && r.URL.Path == "/readyz" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := a.store.Ping(ctx); err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}

		if err := a.control.Ready(ctx); err != nil {
			problem(w, 503, "comfy_unavailable")
			return
		}

		jsonReply(w, 200, map[string]string{"status": "ready"})

		return
	}

	header := r.Header.Get("Authorization")

	given := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare(given[:], a.token[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem(w, 401, "unauthorized")

		return
	}

	if r.URL.Path == "/v1/jobs" {
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			problem(w, 405, "method_not_allowed")

			return
		}

		a.create(w, r)

		return
	}

	if r.URL.Path == "/metrics" && r.Method == "GET" {
		a.metrics(w, r)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "jobs" || !idPattern.MatchString(parts[2]) {
		problem(w, 404, "not_found")
		return
	}

	id := parts[2]
	if len(parts) == 3 {
		switch r.Method {
		case "GET":
			a.replyJob(w, r, id)
		case "DELETE":
			if err := a.store.Cancel(r.Context(), id); err != nil {
				apiError(w, err)
				return
			}

			a.replyJob(w, r, id)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			problem(w, 405, "method_not_allowed")
		}

		return
	}

	if len(parts) == 4 && parts[3] == "abandon" && r.Method == "POST" {
		if err := a.control.Abandon(r.Context(), id); err != nil {
			apiError(w, err)
			return
		}

		a.replyJob(w, r, id)

		return
	}

	if len(parts) == 4 && parts[3] == "reconcile" && r.Method == "POST" {
		if !a.allow() {
			w.Header().Set("Retry-After", "1")
			problem(w, 429, "rate_limited")

			return
		}

		if err := a.control.Reconcile(r.Context(), id); err != nil {
			apiError(w, err)
			return
		}

		a.replyJob(w, r, id)

		return
	}

	if len(parts) == 5 && parts[3] == "images" && r.Method == "GET" {
		index, err := strconv.Atoi(parts[4])
		if err != nil || index < 0 || index > 3 {
			problem(w, 404, "not_found")
			return
		}

		data, media, err := a.store.Image(r.Context(), id, index)
		if err != nil {
			apiError(w, err)
			return
		}

		w.Header().Set("Content-Type", media)
		w.Header().Set("Content-Disposition", `attachment; filename="image.png"`)
		w.WriteHeader(200)
		_, _ = w.Write(data)

		return
	}

	problem(w, 404, "not_found")
}

func (a *API) replyJob(w http.ResponseWriter, r *http.Request, id string) {
	job, err := a.store.Get(r.Context(), id)
	if err != nil {
		apiError(w, err)
		return
	}

	jsonReply(w, http.StatusOK, job)
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	if !a.allow() {
		w.Header().Set("Retry-After", "1")
		problem(w, 429, "rate_limited")

		return
	}

	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		problem(w, 415, "expected_application_json")
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if !keyPattern.MatchString(key) {
		problem(w, 400, "invalid_idempotency_key")
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32768))
	if err != nil {
		var large *http.MaxBytesError

		if errors.As(err, &large) {
			problem(w, 413, "body_too_large")
		} else {
			problem(w, 400, "invalid_body")
		}

		return
	}

	if !utf8.Valid(data) {
		problem(w, 400, "invalid_utf8")
		return
	}

	input := defaults()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err = decoder.Decode(&input); err != nil {
		problem(w, 400, "invalid_request")
		return
	}

	var extra any

	if decoder.Decode(&extra) != io.EOF {
		problem(w, 400, "trailing_json")
		return
	}

	input.Prompt = strings.TrimSpace(input.Prompt)

	input.Negative = strings.TrimSpace(input.Negative)
	if !input.valid() {
		problem(w, 400, "invalid_generation_parameters")
		return
	}

	canonical, _ := json.Marshal(input)
	hash := sha256.Sum256(canonical)

	workflow, err := buildWorkflow(a.template, input)
	if err != nil {
		problem(w, 503, "invalid_template")
		return
	}

	job, created, err := a.store.Create(r.Context(), key, hex.EncodeToString(hash[:]), workflow)
	if err != nil {
		apiError(w, err)
		return
	}

	w.Header().Set("Location", "/v1/jobs/"+job.ID)

	status := 200
	if created {
		status = 202
	}

	jsonReply(w, status, job)
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	counts, err := a.store.Counts(r.Context())
	if err != nil {
		apiError(w, err)
		return
	}

	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}

	sort.Strings(states)

	var out strings.Builder

	out.WriteString("# TYPE image_jobs gauge\n")

	for _, state := range states {
		fmt.Fprintf(&out, "image_jobs{state=%q} %d\n", state, counts[state])
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = io.WriteString(w, out.String())
}

func (a *API) Drain() { a.draining.Store(true) }
