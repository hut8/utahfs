package persistent

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/prometheus/client_golang/prometheus"
)

var LocalWALSize = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "local_wal_size",
		Help: "The number of entries in the local WAL.",
	},
	[]string{"path"},
)
var LocalWALDrainDuration = prometheus.NewSummary(
	prometheus.SummaryOpts{
		Name: "local_wal_drain_duration",
		Help: "Duration that drainOnce() takes in msec.",
	},
)
var LocalWALDrainSetDuration = prometheus.NewSummary(
	prometheus.SummaryOpts{
		Name: "local_wal_drain_set_duration",
		Help: "Duration that writing to underlying store takes in msec.",
	},
)
var LocalWALQueryDuration = prometheus.NewSummary(
	prometheus.SummaryOpts{
		Name: "local_wal_query_duration",
		Help: "Duration that loading chunk of wal takes in msec.",
	},
)

type localWAL struct {
	mu sync.Mutex

	base  ObjectStorage
	local *sql.DB

	loc         string
	maxSize     int
	parallelism int
	wake        chan struct{}

	currSize  int
	lastCount time.Time
}

// NewLocalWAL returns a ReliableStorage implementation that achieves reliable
// writes over a base object storage provider by buffering writes in a
// Write-Ahead Log (WAL) stored at `loc`.
//
// The WAL may have at least `maxSize` buffered entries before new writes start
// blocking on old writes being flushed.
func NewLocalWAL(base ObjectStorage, loc string, maxSize, parallelism int) (ReliableStorage, error) {
	if err := os.MkdirAll(path.Dir(loc), 0744); err != nil {
		return nil, err
	}
	local, err := sql.Open("sqlite3", loc)
	if err != nil {
		return nil, err
	}
	_, err = local.Exec("CREATE TABLE IF NOT EXISTS wal (id integer primary key autoincrement, key integer, val bytea, dt integer, UNIQUE(key))")
	if err != nil {
		return nil, err
	}
	wal := &localWAL{
		base:  base,
		local: local,

		loc:         loc,
		maxSize:     maxSize,
		parallelism: parallelism,
		wake:        make(chan struct{}),

		currSize:  0,
		lastCount: time.Time{},
	}
	go wal.drain()
	go func() {
		for {
			time.Sleep(10 * time.Second)
			wal.count()
		}
	}()

	return wal, nil
}

func (lw *localWAL) drain() {
	tick := time.Tick(5 * time.Second)

	for {
		select {
		case <-tick:
		case <-lw.wake:
		}

		if err := lw.drainOnce(); err != nil {
			log.Println(err)
		}
	}
}

type walReq struct {
	key uint64
	val []byte
	dt  DataType
}

func (lw *localWAL) drainOnce() error {
	reqs := make(chan walReq, 100)
	errs := make(chan error, 100)
	defer close(reqs)
	// benchmarking for draining
	startTime := time.Now()
	defer func() {
		elapsed := time.Now().Sub(startTime)
		LocalWALDrainDuration.Observe(float64(elapsed.Milliseconds()))
	}()

	for i := 0; i < lw.parallelism; i++ {
		go func() {
			for {
				req, ok := <-reqs
				if !ok {
					return
				}

				var err error
				if len(req.val) > 0 {
					drainSetStart := time.Now()
					err = lw.base.Set(context.Background(), hex(req.key), req.val, req.dt)
					drainSetElapsed := time.Now().Sub(drainSetStart)
					LocalWALDrainSetDuration.Observe(float64(drainSetElapsed.Milliseconds()))
				} else {
					err = lw.base.Delete(context.Background(), hex(req.key))
				}

				errs <- err
			}
		}()
	}

	for {
		var (
			ids  []int64
			keys []uint64
			vals [][]byte
			dts  []DataType
		)

		queryStart := time.Now()
		rows, err := lw.local.Query("SELECT id, key, val, dt FROM wal LIMIT 100")
		if err != nil {
			return fmt.Errorf("wal: could not query data: %v", err)
		}
		for rows.Next() {
			var (
				id  int64
				key uint64
				val []byte
				dt  DataType
			)
			if err := rows.Scan(&id, &key, &val, &dt); err != nil {
				rows.Close()
				return fmt.Errorf("wal: could not scan wal rows: %v", err)
			}
			ids = append(ids, id)
			keys = append(keys, key)
			vals = append(vals, val)
			dts = append(dts, dt)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("wal: rows query encountered error: %v", err)
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		queryElapsed := time.Now().Sub(queryStart)
		LocalWALQueryDuration.Observe(float64(queryElapsed.Milliseconds()))

		// Write entries read from the WAL to the underlying storage. This is
		// done outside of the database query to prevent blocking other threads.
		for i, _ := range ids {
			reqs <- walReq{keys[i], vals[i], dts[i]}
		}
		for range ids {
			if subErr := <-errs; subErr != nil {
				err = subErr
			}
		}
		if err != nil {
			return fmt.Errorf("wal: write error writing to underlying storage: %v", err)
		}

		idStrs := make([]string, 0, len(ids))
		for _, id := range ids {
			idStrs = append(idStrs, fmt.Sprint(id))
		}
		_, err = lw.local.Exec("DELETE FROM wal WHERE id in (" + strings.Join(idStrs, ",") + ")")
		if err != nil {
			return fmt.Errorf("wal: error deleting entries after write: %v", err)
		}
	}
}

func (lw *localWAL) count() (int, error) {
	lw.mu.Lock()
	if time.Since(lw.lastCount) < 10*time.Second {
		curr := lw.currSize
		lw.mu.Unlock()
		return curr, nil
	}
	lw.mu.Unlock()

	var count int
	err := lw.local.QueryRow("SELECT COUNT(*) FROM wal").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("wal: could not count rows: %v", err)
	}

	lw.mu.Lock()
	lw.lastCount = time.Now()
	lw.currSize = count
	lw.mu.Unlock()

	LocalWALSize.WithLabelValues(lw.loc).Set(float64(count))
	return count, nil
}

func (lw *localWAL) Start(ctx context.Context, prefetch []uint64) (map[uint64][]byte, error) {
	// Block until the database has drained enough to accept new writes.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		count, err := lw.count()
		if err != nil {
			return nil, err
		}

		if count > lw.maxSize {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case lw.wake <- struct{}{}:
			case <-ticker.C:
			}
			continue
		}
		return lw.GetMany(ctx, prefetch)
	}
}

func (lw *localWAL) Get(ctx context.Context, key uint64) ([]byte, error) {
	var val []byte
	err := lw.local.QueryRowContext(ctx, "SELECT val FROM wal WHERE key = ?", key).Scan(&val)
	if err == sql.ErrNoRows {
		return lw.base.Get(ctx, hex(key))
	} else if len(val) == 0 {
		return nil, ErrObjectNotFound
	} else if err != nil {
		return nil, fmt.Errorf("wal: error querying for value with key %v: %v", key, err)
	}
	return val, nil
}

func (lw *localWAL) GetMany(ctx context.Context, keys []uint64) (map[uint64][]byte, error) {
	out := make(map[uint64][]byte)
	for _, key := range keys {
		val, err := lw.Get(ctx, key)
		if err == ErrObjectNotFound {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("wal: could not getmany with key %v: %v", key, err)
		}
		out[key] = val
	}
	return out, nil
}

func (lw *localWAL) Commit(ctx context.Context, writes map[uint64]WriteData) error {
	if len(writes) == 0 {
		return nil
	}

	tx, err := lw.local.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("wal: could not begin tx: %v", err)
	}
	delStmt, err := tx.Prepare("DELETE FROM wal WHERE key = ?")
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("wal: could not prepare delete statement: %v", err)
	}
	insertStmt, err := tx.Prepare("INSERT INTO wal (key, val, dt) VALUES (?, ?, ?)")
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("wal: could not prepare insert statement: %v", err)
	}

	for key, wr := range writes {
		if _, err := delStmt.Exec(key); err != nil {
			tx.Rollback()
			return fmt.Errorf("wal: could not execute delete statement for key %v: %v", key, err)
		} else if _, err := insertStmt.Exec(key, wr.Data, wr.Type); err != nil {
			tx.Rollback()
			return fmt.Errorf("wal: could not execute insert statement for key %v: %v", key, err)
		}
	}

	return tx.Commit()
}
