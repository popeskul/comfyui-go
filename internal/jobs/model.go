package jobs

import (
	"errors"
	"time"
)

var (
	ErrConflict = errors.New("conflict")
	ErrCapacity = errors.New("capacity reached")
	ErrNotFound = errors.New("not found")
)

type Image struct {
	Data []byte
	Type string
}

type Job struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	PromptID string    `json:"-"`
	Workflow []byte    `json:"-"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created_at"`
	Updated  time.Time `json:"updated_at"`
	Images   int       `json:"image_count"`
}
