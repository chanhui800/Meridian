package main

import (
	"context"
	"log"
	"sync"
	"time"
)

type certificateJob struct {
	nodeID  int64
	attempt int
}

// certificateWorker serializes initial edge certificate provisioning and keeps
// enrollment HTTP requests independent from ACME wait time. Retries use
// bounded backoff so a temporary DNS or ACME outage does not require a node to
// re-enroll.
type certificateWorker struct {
	db      *DB
	manager *panelCertificateManager
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[int64]certificateJob
	running map[int64]struct{}
	retry   map[int64]struct{}
	wake    chan struct{}
}

var certificateRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

func newCertificateWorker(parent context.Context, db *DB, manager *panelCertificateManager) *certificateWorker {
	if db == nil || manager == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	w := &certificateWorker{
		db: db, manager: manager, ctx: ctx, cancel: cancel,
		pending: make(map[int64]certificateJob),
		running: make(map[int64]struct{}),
		retry:   make(map[int64]struct{}),
		wake:    make(chan struct{}, 1),
	}
	w.wg.Add(1)
	go w.run()
	return w
}

func (w *certificateWorker) enqueue(nodeID int64) {
	if w == nil || nodeID <= 0 {
		return
	}
	w.mu.Lock()
	if _, exists := w.pending[nodeID]; exists {
		w.mu.Unlock()
		return
	}
	if _, exists := w.running[nodeID]; exists {
		w.mu.Unlock()
		return
	}
	if _, exists := w.retry[nodeID]; exists {
		w.mu.Unlock()
		return
	}
	w.pending[nodeID] = certificateJob{nodeID: nodeID}
	w.mu.Unlock()
	w.signal()
}

// queue schedules a retry without recursively creating short-lived timers.
// The worker keeps one pending job per node and wakes its single consumer when
// the retry becomes due. This prevents a full queue from creating a timer storm
// during large enrollments or an ACME outage.
func (w *certificateWorker) queue(job certificateJob) {
	if w == nil {
		return
	}
	if w.ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if _, exists := w.running[job.nodeID]; exists {
		w.mu.Unlock()
		return
	}
	if existing, exists := w.pending[job.nodeID]; exists && existing.attempt >= job.attempt {
		w.mu.Unlock()
		return
	}
	w.pending[job.nodeID] = job
	w.mu.Unlock()
	w.signal()
}

func (w *certificateWorker) signal() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *certificateWorker) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.wake:
			for {
				job, ok := w.takePending()
				if !ok {
					break
				}
				w.process(job)
			}
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *certificateWorker) takePending() (certificateJob, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for nodeID, job := range w.pending {
		delete(w.pending, nodeID)
		w.running[nodeID] = struct{}{}
		return job, true
	}
	return certificateJob{}, false
}

func (w *certificateWorker) process(job certificateJob) {
	node, err := w.db.controlNodeByID(job.nodeID, time.Now())
	if err == nil {
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Minute)
		err = provisionEdgeCertificateForNode(ctx, w.db, w.manager, node)
		cancel()
	}
	if err == nil {
		w.mu.Lock()
		delete(w.running, job.nodeID)
		w.mu.Unlock()
		return
	}
	if job.attempt >= len(certificateRetryDelays) {
		w.mu.Lock()
		delete(w.running, job.nodeID)
		w.mu.Unlock()
		log.Printf("[edge-certificate] node %d provisioning failed after retries: %v", job.nodeID, err)
		return
	}
	delay := certificateRetryDelays[job.attempt]
	next := certificateJob{nodeID: job.nodeID, attempt: job.attempt + 1}
	log.Printf("[edge-certificate] node %d provisioning failed: %v; retrying in %s", job.nodeID, err, delay)
	w.mu.Lock()
	delete(w.running, job.nodeID)
	w.retry[job.nodeID] = struct{}{}
	w.mu.Unlock()
	time.AfterFunc(delay, func() {
		w.mu.Lock()
		delete(w.retry, next.nodeID)
		w.mu.Unlock()
		w.queue(next)
	})
}

func (w *certificateWorker) close() {
	if w == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
}
