package jobsvc

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver (CGO_ENABLED=0 discipline)

	"github.com/easylab-platform/easyworker/internal/shellh"
)

// Store persists jobs and their line history to sqlite (modernc). The DB is
// the unbounded authority for history; the in-memory ring only covers the
// live window. Sandbox semantics: the DB lives on an emptyDir, so everything
// is ephemeral to the pod — a container restart (same pod) keeps it, a pod
// recreation wipes it. Retention trims finished jobs past the window.
type Store struct {
	db      *sql.DB
	ch      chan interface{}
	done    chan struct{}
	stopped chan struct{}
	dropped int64
}

const (
	writeBatchLen   = 500
	writeFlushEvery = 200 * time.Millisecond
	channelCap      = 65536
)

// Stream identifiers in job_lines.stream.
const (
	StreamStdout = 0
	StreamStderr = 1
)

type storeLine struct {
	jobID  string
	seq    int64
	stream int
	line   string
}
type storeFinish struct {
	jobID      string
	state      string
	exitCode   int32
	finishedAt int64
}

// OpenStore opens (creating if needed) the sqlite store and recovers orphaned
// running jobs (their processes died with the previous worker process).
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, ch: make(chan interface{}, channelCap), done: make(chan struct{}), stopped: make(chan struct{})}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.recover(); err != nil {
		db.Close()
		return nil, err
	}
	go s.writer()
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			command TEXT NOT NULL,
			state TEXT NOT NULL,
			exit_code INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS job_lines (
			job_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			stream INTEGER NOT NULL,
			line TEXT NOT NULL,
			PRIMARY KEY (job_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_job_lines_stream ON job_lines(job_id, stream, seq)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// recover marks running jobs as failed — their processes died with the
// previous worker process (exit 137 = SIGKILL-ish).
func (s *Store) recover() error {
	_, err := s.db.Exec(`UPDATE jobs SET state='failed', exit_code=137, finished_at=? WHERE state='running'`, time.Now().UnixMilli())
	return err
}

// writer is the single DB writer: FIFO ordering guarantees a job's finish
// record commits after all its line records.
func (s *Store) writer() {
	defer close(s.stopped)
	batch := make([]storeLine, 0, writeBatchLen)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		tx, err := s.db.Begin()
		if err != nil {
			log.Printf("store: begin: %v", err)
			batch = batch[:0]
			return
		}
		stmt, err := tx.Prepare(`INSERT OR REPLACE INTO job_lines (job_id, seq, stream, line) VALUES (?,?,?,?)`)
		if err != nil {
			log.Printf("store: prepare: %v", err)
			tx.Rollback()
			batch = batch[:0]
			return
		}
		for _, r := range batch {
			if _, err := stmt.Exec(r.jobID, r.seq, r.stream, r.line); err != nil {
				log.Printf("store: insert line: %v", err)
				break
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			log.Printf("store: commit: %v", err)
		}
		batch = batch[:0]
	}
	ticker := time.NewTicker(writeFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case rec := <-s.ch:
			switch v := rec.(type) {
			case storeLine:
				batch = append(batch, v)
				if len(batch) >= writeBatchLen {
					flush()
				}
			case storeFinish:
				flush() // commit pending lines BEFORE the finish update
				if _, err := s.db.Exec(`UPDATE jobs SET state=?, exit_code=?, finished_at=? WHERE id=?`,
					v.state, v.exitCode, v.finishedAt, v.jobID); err != nil {
					log.Printf("store: finish job: %v", err)
				}
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			flush()
			return
		}
	}
}

// EnqueueJob registers a running job (direct write so ListJobs sees it
// immediately).
func (s *Store) EnqueueJob(id, command string, startedAt int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO jobs (id, command, state, exit_code, started_at, finished_at) VALUES (?,?,?,?,?,0)`,
		id, command, StateRunning, 0, startedAt)
	return err
}

// TryEnqueueLine feeds one output line to the writer. Non-blocking: on a full
// channel the line is dropped (memory ring still holds it for live viewers;
// the counter tracks the loss).
func (s *Store) TryEnqueueLine(jobID string, seq int64, stream int, line string) {
	select {
	case s.ch <- storeLine{jobID: jobID, seq: seq, stream: stream, line: line}:
	default:
		s.dropped++
	}
}

// FinishJob enqueues the terminal state (ordered after all prior lines).
func (s *Store) FinishJob(id, state string, exitCode int32, finishedAt int64) {
	select {
	case s.ch <- storeFinish{jobID: id, state: state, exitCode: exitCode, finishedAt: finishedAt}:
	default:
		log.Printf("store: finish enqueue dropped for %s", id)
	}
}

// JobRow is one persisted job.
type JobRow struct {
	ID         string
	Command    string
	State      string
	ExitCode   int32
	StartedAt  int64
	FinishedAt int64
}

// QueryJob fetches one job row.
func (s *Store) QueryJob(id string) (JobRow, bool, error) {
	row := s.db.QueryRow(`SELECT id, command, state, exit_code, started_at, finished_at FROM jobs WHERE id=?`, id)
	var r JobRow
	err := row.Scan(&r.ID, &r.Command, &r.State, &r.ExitCode, &r.StartedAt, &r.FinishedAt)
	if err == sql.ErrNoRows {
		return JobRow{}, false, nil
	}
	if err != nil {
		return JobRow{}, false, err
	}
	return r, true, nil
}

// QueryJobs lists the newest jobs up to limit.
func (s *Store) QueryJobs(limit int) ([]JobRow, error) {
	rows, err := s.db.Query(`SELECT id, command, state, exit_code, started_at, finished_at FROM jobs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var r JobRow
		if err := rows.Scan(&r.ID, &r.Command, &r.State, &r.ExitCode, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryLines returns persisted lines for a job, optionally filtered by
// stream (-1 = all), ordered by seq.
func (s *Store) QueryLines(jobID string, stream int) ([]shellh.LineRec, int, error) {
	q := `SELECT seq, line FROM job_lines WHERE job_id=?`
	args := []interface{}{jobID}
	if stream >= 0 {
		q += ` AND stream=?`
		args = append(args, stream)
	}
	q += ` ORDER BY seq`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []shellh.LineRec
	var streamCol int
	for rows.Next() {
		var r shellh.LineRec
		if err := rows.Scan(&r.Seq, &r.Line); err != nil {
			return nil, 0, err
		}
		_ = streamCol
		out = append(out, r)
	}
	return out, len(out), rows.Err()
}

// MaxSeq returns the highest persisted seq for a job/stream (-1 = any), 0
// when nothing persisted yet.
func (s *Store) MaxSeq(jobID string, stream int) (int64, error) {
	q := `SELECT COALESCE(MAX(seq),0) FROM job_lines WHERE job_id=?`
	args := []interface{}{jobID}
	if stream >= 0 {
		q += ` AND stream=?`
		args = append(args, stream)
	}
	var max int64
	err := s.db.QueryRow(q, args...).Scan(&max)
	return max, err
}

// Retention deletes finished jobs (and their lines) older than hours.
// Returns the number of deleted jobs.
func (s *Store) Retention(hours int) int {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return 0
	}
	res, err := tx.Exec(`DELETE FROM jobs WHERE state != 'running' AND finished_at > 0 AND finished_at < ?`, cutoff)
	if err != nil {
		tx.Rollback()
		return 0
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM job_lines WHERE job_id NOT IN (SELECT id FROM jobs)`); err != nil {
		tx.Rollback()
		return 0
	}
	tx.Commit()
	return int(n)
}

// Close drains the writer and closes the DB.
func (s *Store) Close() error {
	close(s.done)
	<-s.stopped
	return s.db.Close()
}
