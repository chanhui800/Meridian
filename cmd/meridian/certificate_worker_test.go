package main

import (
	"context"
	"testing"
	"time"
)

func TestCertificateWorkerDeduplicatesPendingAndRunningJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &certificateWorker{
		ctx:     ctx,
		pending: make(map[int64]certificateJob),
		running: make(map[int64]struct{}),
		retry:   make(map[int64]*time.Timer),
		wake:    make(chan struct{}, 1),
	}

	w.enqueue(17)
	w.enqueue(17)
	if len(w.pending) != 1 {
		t.Fatalf("pending jobs=%d, want one deduplicated job", len(w.pending))
	}
	job, ok := w.takePending()
	if !ok || job.nodeID != 17 {
		t.Fatalf("takePending=%#v, ok=%t", job, ok)
	}
	w.enqueue(17)
	if len(w.pending) != 0 {
		t.Fatal("running node accepted a duplicate job")
	}

	w.mu.Lock()
	delete(w.running, 17)
	w.mu.Unlock()
	w.queue(certificateJob{nodeID: 17, attempt: 1})
	w.queue(certificateJob{nodeID: 17, attempt: 1})
	if got := w.pending[17].attempt; got != 1 || len(w.pending) != 1 {
		t.Fatalf("retry pending=%#v, want one attempt-1 job", w.pending)
	}
}

func TestCertificateWorkerQueueDoesNotCreateOverflowTimers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &certificateWorker{
		ctx:     ctx,
		pending: make(map[int64]certificateJob),
		running: make(map[int64]struct{}),
		retry:   make(map[int64]*time.Timer),
		wake:    make(chan struct{}, 1),
	}

	for nodeID := int64(1); nodeID <= 1000; nodeID++ {
		w.queue(certificateJob{nodeID: nodeID})
	}
	if len(w.pending) != 1000 {
		t.Fatalf("pending jobs=%d, want 1000", len(w.pending))
	}
	select {
	case <-w.wake:
	default:
		t.Fatal("queue did not wake the worker")
	}
	select {
	case <-w.wake:
		t.Fatal("duplicate queue operations created extra wake work")
	default:
	}
}

func TestCertificateWorkerManualEnqueueCancelsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	w := &certificateWorker{
		ctx:     ctx,
		pending: make(map[int64]certificateJob),
		running: make(map[int64]struct{}),
		retry:   map[int64]*time.Timer{17: timer},
		wake:    make(chan struct{}, 1),
	}

	w.enqueue(17)
	if _, exists := w.retry[17]; exists {
		t.Fatal("manual enqueue left the retry timer installed")
	}
	job, ok := w.pending[17]
	if !ok || job.attempt != 0 {
		t.Fatalf("manual enqueue job=%#v, ok=%t; want immediate attempt zero", job, ok)
	}
}
