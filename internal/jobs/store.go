package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db       *sql.DB
	lock     *os.File
	capacity int
	maxBytes int64
	maxJobs  int
}

func Open(path string, capacity int, maxBytes int64, maxJobs int) (*Store, error) {
	if capacity < 1 || maxBytes < 1 || maxJobs < 1 {
		return nil, fmt.Errorf("invalid store limits")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	if err = os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}

	lock, err := os.OpenFile(abs+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another service owns this database: %w", err)
	}

	s := &Store{lock: lock, capacity: capacity, maxBytes: maxBytes, maxJobs: maxJobs}

	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		s.Close()
		return nil, err
	}

	file.Close()

	uri := (&url.URL{Scheme: "file", Path: abs}).String() + "?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_foreign_keys=on&_txlock=immediate"

	s.db, err = sql.Open("sqlite3", uri)
	if err != nil {
		s.Close()
		return nil, err
	}

	s.db.SetMaxOpenConns(1)

	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS jobs(
 id TEXT PRIMARY KEY, idem_key TEXT NOT NULL UNIQUE, request_hash TEXT NOT NULL,
 workflow BLOB NOT NULL, state TEXT NOT NULL CHECK(state IN ('queued','submitting','running','uncertain','succeeded','failed','cancelled')),
 prompt_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, updated INTEGER NOT NULL);

 CREATE INDEX IF NOT EXISTS jobs_state_created ON jobs(state,created);

 CREATE TABLE IF NOT EXISTS images(job_id TEXT NOT NULL REFERENCES jobs(id),idx INTEGER NOT NULL,data BLOB NOT NULL,media_type TEXT NOT NULL,PRIMARY KEY(job_id,idx));`)
	if err != nil {
		s.Close()
		return nil, err
	}

	return s, nil
}

func (s *Store) Close() error {
	var err error

	if s.db != nil {
		err = s.db.Close()
	}

	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
	}

	return err
}

func newID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

const columns = `id,state,prompt_id,workflow,error,created,updated,(SELECT count(*) FROM images WHERE job_id=jobs.id)`

func scan(row interface{ Scan(...any) error }) (Job, error) {
	var (
		j                Job
		created, updated int64
	)

	err := row.Scan(&j.ID, &j.State, &j.PromptID, &j.Workflow, &j.Error, &created, &updated, &j.Images)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}

	j.Created = time.Unix(0, created).UTC()
	j.Updated = time.Unix(0, updated).UTC()

	return j, err
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=?`, id))
}

func (s *Store) Create(ctx context.Context, key, hash string, workflow []byte) (Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}

	defer tx.Rollback()

	var id, oldHash string

	err = tx.QueryRowContext(ctx, `SELECT id,request_hash FROM jobs WHERE idem_key=?`, key).Scan(&id, &oldHash)
	if err == nil {
		if oldHash != hash {
			return Job{}, false, ErrConflict
		}

		j, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=?`, id))

		return j, false, err
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, err
	}

	var (
		active, total int
		used          int64
	)

	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(CASE WHEN state IN ('queued','submitting','running','uncertain') THEN 1 ELSE 0 END),0)
 FROM jobs`).Scan(&total, &active); err != nil {
		return Job{}, false, err
	}

	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(length(data)),0) FROM images`).Scan(&used); err != nil {
		return Job{}, false, err
	}

	if active >= s.capacity || total >= s.maxJobs || used >= s.maxBytes {
		return Job{}, false, ErrCapacity
	}

	id, err = newID()
	if err != nil {
		return Job{}, false, err
	}

	now := time.Now().UTC()

	if workflow == nil {
		workflow = []byte(`{}`)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,idem_key,request_hash,workflow,state,created,updated) VALUES(?,?,?,?,'queued',?,?)`, id, key, hash, workflow, now.UnixNano(), now.UnixNano())
	if err != nil {
		return Job{}, false, err
	}

	if err = tx.Commit(); err != nil {
		return Job{}, false, err
	}

	return Job{ID: id, State: "queued", Workflow: workflow, Created: now, Updated: now}, true, nil
}

func (s *Store) Transition(ctx context.Context, id, from, to, promptID, message string) error {
	allowed := map[string]string{
		"queued:submitting":    "",
		"queued:failed":        "",
		"queued:cancelled":     "",
		"submitting:running":   "",
		"submitting:uncertain": "",
		"submitting:failed":    "",
		"running:uncertain":    "",
		"running:failed":       "",
		"uncertain:running":    "",
		"uncertain:failed":     "",
	}
	if _, ok := allowed[from+":"+to]; !ok {
		return fmt.Errorf("invalid state transition %s to %s", from, to)
	}

	if to == "submitting" && promptID == "" {
		promptID = id
	}

	result, err := s.db.ExecContext(ctx, `UPDATE jobs
 SET state=?,prompt_id=CASE WHEN ?='' THEN prompt_id ELSE ? END,error=?,updated=?
 WHERE id=? AND state=?`, to, promptID, promptID, message, time.Now().UnixNano(), id, from)
	if err != nil {
		return err
	}

	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}

	return err
}

func (s *Store) Cancel(ctx context.Context, id string) error {
	j, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	if j.State == "cancelled" {
		return nil
	}

	if j.State != "queued" {
		return ErrConflict
	}

	return s.Transition(ctx, id, "queued", "cancelled", "", "")
}

func (s *Store) Recover(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs
 SET state='uncertain',error='service restarted during submission; reconcile before any further action',updated=?
 WHERE state='submitting'`, time.Now().UnixNano())

	return err
}

func (s *Store) Next(ctx context.Context) (Job, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE state IN ('queued','submitting','running') ORDER BY CASE state WHEN 'submitting' THEN 0 WHEN 'running' THEN 1 ELSE 2 END,created,id LIMIT 1`))
}

func (s *Store) Finish(ctx context.Context, id string, images []Image) error {
	if len(images) < 1 || len(images) > 4 {
		return fmt.Errorf("expected 1 to 4 images")
	}

	var added int64

	for _, img := range images {
		if len(img.Data) == 0 {
			return fmt.Errorf("empty image")
		}

		added += int64(len(img.Data))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	var used int64

	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(length(data)),0) FROM images`).Scan(&used); err != nil {
		return err
	}

	if used+added > s.maxBytes {
		return ErrCapacity
	}

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state='succeeded',error='',updated=? WHERE id=? AND state='running'`, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}

	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrConflict
	}

	for i, img := range images {
		if _, err = tx.ExecContext(ctx, `INSERT INTO images(job_id,idx,data,media_type) VALUES(?,?,?,?)`, id, i, img.Data, img.Type); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) Image(ctx context.Context, id string, index int) ([]byte, string, error) {
	var (
		data  []byte
		media string
	)

	err := s.db.QueryRowContext(ctx, `SELECT data,media_type
 FROM images JOIN jobs ON jobs.id=images.job_id
 WHERE job_id=? AND idx=? AND state='succeeded'`, id, index).Scan(&data, &media)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}

	return data, media, err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Prune(ctx context.Context, before time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	filter := `SELECT id FROM jobs WHERE state IN ('succeeded','failed','cancelled') AND updated < ?`
	if _, err = tx.ExecContext(ctx, `DELETE FROM images WHERE job_id IN (`+filter+`)`, before.UnixNano()); err != nil {
		return err
	}

	if _, err = tx.ExecContext(ctx, `DELETE FROM jobs WHERE id IN (`+filter+`)`, before.UnixNano()); err != nil {
		return err
	}

	return tx.Commit()
}

// Counts returns a snapshot of retained jobs grouped by state.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)

	for rows.Next() {
		var (
			state string
			count int
		)

		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}

		counts[state] = count
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return counts, nil
}
