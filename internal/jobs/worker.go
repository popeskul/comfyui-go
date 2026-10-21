package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Ref struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type Remote struct {
	State  string
	Images []Ref
}

type Rejected struct{ Reason string }

func (e *Rejected) Error() string { return e.Reason }

// Backend is the upstream generation contract, including its readiness probe.
type Backend interface {
	Ready(context.Context) error
	Submit(context.Context, string, []byte) error
	Inspect(context.Context, string) (Remote, error)
	Fetch(context.Context, Ref) (Image, error)
}

// WorkerStore contains only the persistence operations needed to execute jobs.
type WorkerStore interface {
	Next(context.Context) (Job, error)
	Get(context.Context, string) (Job, error)
	Transition(context.Context, string, string, string, string, string) error
	Finish(context.Context, string, []Image) error
}

type Worker struct {
	Store   WorkerStore
	Backend Backend
	MaxAge  time.Duration
}

// Step is called by exactly one worker. SQLite's process lock enforces one owner.
func (w Worker) Step(ctx context.Context) error {
	j, err := w.Store.Next(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	if j.State == "submitting" {
		return w.Store.Transition(ctx, j.ID, "submitting", "uncertain", "", "submission marker requires reconciliation; never retry POST")
	}

	if j.State == "queued" {
		if time.Since(j.Created) > w.MaxAge {
			return w.Store.Transition(ctx, j.ID, "queued", "failed", "", "job expired before submission")
		}

		// A down backend must leave work queued; probing with GET is safe.
		if err := w.Backend.Ready(ctx); err != nil {
			return err
		}

		// This durable marker MUST commit before the non-idempotent upstream POST.
		if err = w.Store.Transition(ctx, j.ID, "queued", "submitting", j.ID, ""); err != nil {
			return err
		}

		err = w.Backend.Submit(ctx, j.ID, j.Workflow)
		if err != nil {
			// Even shutdown/transport failure may happen after upstream accepted the job.
			state, message := "uncertain", "upstream acceptance unknown; reconcile this job"

			var rejected *Rejected

			if errors.As(err, &rejected) {
				state, message = "failed", "upstream rejected the workflow"
			}

			// A cancelled process context must not prevent best-effort durable recording.
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()

			return w.Store.Transition(saveCtx, j.ID, "submitting", state, j.ID, message)
		}

		if err = w.Store.Transition(ctx, j.ID, "submitting", "running", j.ID, ""); err != nil {
			return err
		}

		j.State = "running"
		j.PromptID = j.ID
	}

	remote, err := w.Backend.Inspect(ctx, j.PromptID)
	if err != nil {
		if ctx.Err() == nil && time.Since(j.Created) > w.MaxAge {
			return w.Store.Transition(ctx, j.ID, "running", "uncertain", "", "job deadline reached; upstream status unavailable")
		}

		return err
	}

	// GET is safe to retry on the next tick.
	switch remote.State {
	case "pending", "running":
		if time.Since(j.Created) > w.MaxAge {
			return w.Store.Transition(ctx, j.ID, "running", "uncertain", "", "job deadline reached; upstream may still be running")
		}

		return nil
	case "missing":
		return w.Store.Transition(ctx, j.ID, "running", "uncertain", "", "upstream has no queue/history entry; never resubmit automatically")
	case "failed":
		return w.Store.Transition(ctx, j.ID, "running", "failed", "", "upstream execution failed")
	case "completed":
		if len(remote.Images) < 1 || len(remote.Images) > 4 {
			return w.Store.Transition(ctx, j.ID, "running", "failed", "", "upstream returned an invalid image count")
		}

		images := make([]Image, 0, len(remote.Images))
		for _, ref := range remote.Images {
			img, err := w.Backend.Fetch(ctx, ref)
			if err != nil {
				var rejected *Rejected

				if errors.As(err, &rejected) {
					return w.Store.Transition(ctx, j.ID, "running", "failed", "", "upstream returned an invalid image")
				}

				if ctx.Err() == nil && time.Since(j.Created) > w.MaxAge {
					return w.Store.Transition(ctx, j.ID, "running", "failed", "", "completed result could not be downloaded before deadline")
				}

				return err
			}

			images = append(images, img)
		}

		err = w.Store.Finish(ctx, j.ID, images)
		if errors.Is(err, ErrCapacity) {
			return w.Store.Transition(ctx, j.ID, "running", "failed", "", "result storage limit reached")
		}

		return err
	default:
		return fmt.Errorf("unrecognized upstream state %q", remote.State)
	}
}

// Reconcile only reads upstream state. It never calls Submit.
func (w Worker) Reconcile(ctx context.Context, id string) error {
	j, err := w.Store.Get(ctx, id)
	if err != nil {
		return err
	}

	if j.State != "uncertain" {
		return ErrConflict
	}

	remote, err := w.Backend.Inspect(ctx, j.PromptID)
	if err != nil {
		return err
	}

	switch remote.State {
	case "pending", "running", "completed":
		return w.Store.Transition(ctx, id, "uncertain", "running", "", "")
	case "failed":
		return w.Store.Transition(ctx, id, "uncertain", "failed", "", "upstream execution failed")
	default:
		return ErrConflict
	}
}

// Ready probes the upstream without exposing it to the HTTP handler.
func (w Worker) Ready(ctx context.Context) error { return w.Backend.Ready(ctx) }

// Abandon releases an uncertain job without retrying or cancelling upstream work.
func (w Worker) Abandon(ctx context.Context, id string) error {
	return w.Store.Transition(ctx, id, "uncertain", "failed", "", "operator abandoned this task; upstream may still finish")
}
