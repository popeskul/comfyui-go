package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestStoreCountsReflectTransitions(t *testing.T) {
	s := openTest(t, 4)
	ctx := context.Background()

	counts, err := s.Counts(ctx)
	if err != nil || len(counts) != 0 {
		t.Fatalf("empty counts: %v %v", counts, err)
	}

	first := createTest(t, s, "first")
	createTest(t, s, "second")

	if err := s.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	counts, err = s.Counts(ctx)
	if err != nil || counts["queued"] != 1 || counts["cancelled"] != 1 {
		t.Fatalf("counts: %v %v", counts, err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := s.Counts(cancelCtx); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestWorkerDoesNotSubmitWithoutDurableMarker(t *testing.T) {
	failure := errors.New("disk unavailable")
	backend := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
	}

	worker := Worker{
		Store: &WorkerStoreMock{
			NextFunc: func(context.Context) (Job, error) {
				return Job{ID: "job", State: "queued", Created: time.Now()}, nil
			},
			TransitionFunc: func(context.Context, string, string, string, string, string) error {
				return failure
			},
		},
		Backend: backend,
		MaxAge:  time.Hour,
	}
	if err := worker.Step(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("lost storage error: %v", err)
	}

	if len(backend.SubmitCalls()) != 0 {
		t.Fatal("submitted without durable marker")
	}
}

func TestAPIMetricsStorageFailure(t *testing.T) {
	template, err := os.ReadFile("../../workflow.json")
	if err != nil {
		t.Fatal(err)
	}

	api, err := NewAPI(&APIStoreMock{
		CountsFunc: func(context.Context) (map[string]int, error) {
			return nil, errors.New("database unavailable")
		},
	}, &JobControlMock{}, template, testToken)
	if err != nil {
		t.Fatal(err)
	}

	response := request(api, "GET", "/metrics", "", "", testToken)
	if response.Code != 503 {
		t.Fatalf("metrics failure: %d", response.Code)
	}
}

func TestWorkflowBuildIsolatedAndPreservesTemplate(t *testing.T) {
	template, err := os.ReadFile("../../workflow.json")
	if err != nil {
		t.Fatal(err)
	}

	original := append([]byte(nil), template...)
	first := defaults()
	first.Prompt = "first prompt"
	second := defaults()
	second.Prompt = "second prompt"

	second.Seed = 99
	for _, input := range []Request{first, second, first} {
		data, err := buildWorkflow(template, input)
		if err != nil {
			t.Fatal(err)
		}

		var graph map[string]struct {
			Inputs map[string]any `json:"inputs"`
		}

		if err := json.Unmarshal(data, &graph); err != nil {
			t.Fatal(err)
		}

		if graph["6"].Inputs["text"] != input.Prompt || graph["3"].Inputs["seed"] != float64(input.Seed) {
			t.Fatal("workflow leaked another request")
		}

		if graph["5"].Inputs["batch_size"] != float64(1) {
			t.Fatal("unbounded batch size")
		}
	}

	if !bytes.Equal(template, original) {
		t.Fatal("template was mutated")
	}

	if _, err := buildWorkflow([]byte(`{"3":{"inputs":null}}`), first); err == nil {
		t.Fatal("invalid graph accepted")
	}
}
