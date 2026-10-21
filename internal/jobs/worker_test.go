package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerHappyPath(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "completed", Images: []Ref{{Filename: "one.png"}}}, nil
		},
		FetchFunc: func(context.Context, Ref) (Image, error) {
			return Image{Data: []byte("png"), Type: "image/png"}, nil
		},
	}

	w := Worker{s, b, time.Hour}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "succeeded" || len(b.SubmitCalls()) != 1 {
		t.Fatalf("%+v posts=%d", got, len(b.SubmitCalls()))
	}
}

func TestWorkerAmbiguousSubmitNeverRetries(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return errors.New("response lost")
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "completed", Images: []Ref{{Filename: "one.png"}}}, nil
		},
		FetchFunc: func(context.Context, Ref) (Image, error) {
			return Image{Data: []byte("png"), Type: "image/png"}, nil
		},
	}
	w := Worker{s, b, time.Hour}
	_ = w.Step(context.Background())

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "uncertain" {
		t.Fatal(got.State)
	}

	_ = w.Step(context.Background())

	if len(b.SubmitCalls()) != 1 {
		t.Fatal("ambiguous request resubmitted")
	}

	if err := w.Reconcile(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}

	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ = s.Get(context.Background(), j.ID)
	if got.State != "succeeded" || len(b.SubmitCalls()) != 1 {
		t.Fatalf("recovery: %+v posts=%d", got, len(b.SubmitCalls()))
	}
}

func TestWorkerKnownRejection(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	w := Worker{s, &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return &Rejected{"invalid workflow"}
		},
	}, time.Hour}
	_ = w.Step(context.Background())

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "failed" {
		t.Fatal(got.State)
	}
}

func TestWorkerResumesRunningWithoutSubmit(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	_ = s.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")
	_ = s.Transition(context.Background(), j.ID, "submitting", "running", j.ID, "")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "completed", Images: []Ref{{Filename: "one.png"}}}, nil
		},
		FetchFunc: func(context.Context, Ref) (Image, error) {
			return Image{Data: []byte("png"), Type: "image/png"}, nil
		},
	}

	w := Worker{s, b, time.Hour}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(b.SubmitCalls()) != 0 {
		t.Fatal("running job resubmitted")
	}
}

func TestWorkerFetchFailureDoesNotPublishPartialSuccess(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "completed", Images: []Ref{{Filename: "one.png"}}}, nil
		},
		FetchFunc: func(context.Context, Ref) (Image, error) {
			return Image{Data: []byte("png"), Type: "image/png"}, errors.New("connection dropped")
		},
	}

	w := Worker{s, b, time.Hour}
	if err := w.Step(context.Background()); err == nil {
		t.Fatal("expected error")
	}

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "running" || got.Images != 0 {
		t.Fatal(got)
	}

	b.FetchFunc = func(context.Context, Ref) (Image, error) { return Image{Data: []byte("png"), Type: "image/png"}, nil }

	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(b.SubmitCalls()) != 1 {
		t.Fatal("retried POST")
	}
}

func TestWorkerExecutionFailure(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	w := Worker{s, &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "failed"}, nil
		},
	}, time.Hour}
	_ = w.Step(context.Background())

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "failed" {
		t.Fatal(got)
	}
}

func TestWorkerMissingAndDeadline(t *testing.T) {
	for _, state := range []string{"missing", "running"} {
		t.Run(state, func(t *testing.T) {
			s := openTest(t, 4)
			j := createTest(t, s, "one")
			w := Worker{s, &BackendMock{
				ReadyFunc: func(context.Context) error { return nil },
				SubmitFunc: func(context.Context, string, []byte) error {
					return nil
				},
				InspectFunc: func(context.Context, string) (Remote, error) {
					return Remote{State: state}, nil
				},
			}, time.Nanosecond}
			_ = w.Step(context.Background())

			got, _ := s.Get(context.Background(), j.ID)
			if got.State != "uncertain" && got.State != "failed" {
				t.Fatal(got)
			}
		})
	}
}

func TestWorkerUnresolvedReconcile(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	_ = s.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")
	_ = s.Recover(context.Background())

	w := Worker{s, &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "missing"}, nil
		},
	}, time.Hour}
	if !errors.Is(w.Reconcile(context.Background(), j.ID), ErrConflict) {
		t.Fatal("missing task must remain unresolved")
	}
}

func TestWorkerCancelledNeverSubmitted(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	_ = s.Cancel(context.Background(), j.ID)
	b := &BackendMock{}

	w := Worker{s, b, time.Hour}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(b.SubmitCalls()) != 0 {
		t.Fatal("cancelled task submitted")
	}
}

func TestWorkerExpiredQueuedJobIsNotSubmitted(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	_, _ = s.db.Exec(`UPDATE jobs SET created=? WHERE id=?`, time.Now().Add(-2*time.Hour).UnixNano(), j.ID)
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "running"}, nil
		},
	}

	w := Worker{s, b, time.Hour}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "failed" || len(b.SubmitCalls()) != 0 {
		t.Fatalf("expired queue: %+v posts=%d", got, len(b.SubmitCalls()))
	}
}

func TestWorkerCompletedResultCanBeRecoveredAfterDeadline(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	_ = s.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")
	_ = s.Transition(context.Background(), j.ID, "submitting", "running", j.ID, "")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "completed", Images: []Ref{{Filename: "one.png"}}}, nil
		},
		FetchFunc: func(context.Context, Ref) (Image, error) {
			return Image{Data: []byte("png"), Type: "image/png"}, nil
		},
	}

	w := Worker{s, b, time.Nanosecond}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "succeeded" {
		t.Fatalf("completed result lost at deadline: %+v", got)
	}
}

func TestWorkerOfflineBackendKeepsJobQueued(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return errors.New("offline") },
	}
	w := Worker{s, b, time.Hour}
	_ = w.Step(context.Background())

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "queued" || len(b.SubmitCalls()) != 0 {
		t.Fatalf("offline backend consumed job: %+v posts=%d", got, len(b.SubmitCalls()))
	}
}

func TestWorkerFailedAcceptanceWriteNeverResubmits(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "running"}, nil
		},
	}
	w := Worker{s, b, time.Hour}

	_, err := s.db.Exec(`
CREATE TRIGGER block_running BEFORE UPDATE ON jobs
WHEN NEW.state='running' AND OLD.state='submitting'
BEGIN
    SELECT RAISE(ABORT,'simulated write failure');
END
`)
	if err != nil {
		t.Fatal(err)
	}

	if err = w.Step(context.Background()); err == nil {
		t.Fatal("expected failed acceptance write")
	}

	_, _ = s.db.Exec(`DROP TRIGGER block_running`)

	if err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "uncertain" || len(b.SubmitCalls()) != 1 {
		t.Fatalf("lost acceptance marker: %+v posts=%d", got, len(b.SubmitCalls()))
	}
}

func TestWorkerDownloadFailureEventuallyReleasesWorker(t *testing.T) {
	s := openTest(t, 4)
	j := createTest(t, s, "one")
	_ = s.Transition(context.Background(), j.ID, "queued", "submitting", j.ID, "")
	_ = s.Transition(context.Background(), j.ID, "submitting", "running", j.ID, "")
	b := &BackendMock{
		ReadyFunc: func(context.Context) error { return nil },
		SubmitFunc: func(context.Context, string, []byte) error {
			return nil
		},
		InspectFunc: func(context.Context, string) (Remote, error) {
			return Remote{State: "completed", Images: []Ref{{Filename: "one.png"}}}, nil
		},
		FetchFunc: func(context.Context, Ref) (Image, error) {
			return Image{Data: []byte("png"), Type: "image/png"}, errors.New("download offline")
		},
	}
	w := Worker{s, b, time.Nanosecond}
	_ = w.Step(context.Background())

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "failed" {
		t.Fatalf("expired download blocks queue: %+v", got)
	}
}
