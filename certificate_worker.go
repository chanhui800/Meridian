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
	jobs    chan certificateJob
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[int64]int
}

var certificateRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

func newCertificateWorker(parent context.Context, db *DB, manager *panelCertificateManager) *certificateWorker {
	if db == nil || manager == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	w := &certificateWorker{db: db, manager: manager, ctx: ctx, cancel: cancel, jobs: make(chan certificateJob, 64), pending: make(map[int64]int)}
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
	w.pending[nodeID] = 0
	w.mu.Unlock()
	select {
	case w.jobs <- certificateJob{nodeID: nodeID}:
	case <-w.ctx.Done():
		w.mu.Lock()
		delete(w.pending, nodeID)
		w.mu.Unlock()
	}
}

func (w *certificateWorker) run() {
	defer w.wg.Done()
	for {
		select {
		case job := <-w.jobs:
			w.process(job)
		case <-w.ctx.Done():
			return
		}
	}
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
		delete(w.pending, job.nodeID)
		w.mu.Unlock()
		return
	}
	if job.attempt >= len(certificateRetryDelays) {
		w.mu.Lock()
		delete(w.pending, job.nodeID)
		w.mu.Unlock()
		log.Printf("[edge-certificate] node %d provisioning failed after retries: %v", job.nodeID, err)
		return
	}
	delay := certificateRetryDelays[job.attempt]
	next := certificateJob{nodeID: job.nodeID, attempt: job.attempt + 1}
	log.Printf("[edge-certificate] node %d provisioning failed: %v; retrying in %s", job.nodeID, err, delay)
	time.AfterFunc(delay, func() {
		select {
		case w.jobs <- next:
		case <-w.ctx.Done():
			w.mu.Lock()
			delete(w.pending, job.nodeID)
			w.mu.Unlock()
		}
	})
}

func (w *certificateWorker) close() {
	if w == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
}
