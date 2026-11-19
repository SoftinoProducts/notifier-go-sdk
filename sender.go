package notifier

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sender defaults. They mirror the Java SDK's non-blocking path: bounded lanes, a fixed
// worker count, a total send deadline, and retries that never duplicate an ambiguous send.
const (
	DefaultSenderHighWorkers    = 1
	DefaultSenderBulkWorkers    = 2
	DefaultSenderHighQueueSize  = 1000
	DefaultSenderBulkQueueSize  = 10000
	DefaultSenderSendTimeout    = 10 * time.Second
	DefaultSenderMaxRetries     = 2
	DefaultSenderRetryBaseDelay = 200 * time.Millisecond
	DefaultSenderRetryMaxDelay  = 2 * time.Second
	DefaultSenderQueueFullAfter = 500 * time.Millisecond
)

// Sentinel errors returned by Sender. They are also reported through errors.Is.
var (
	// ErrQueueFull reports that a lane's bounded queue was full and the send was not
	// accepted. A *QueueFullError carries the priority, capacity and a retry hint.
	ErrQueueFull = errors.New("notifier: send queue full")
	// ErrSenderClosed reports that the Sender has been closed and no longer accepts work.
	ErrSenderClosed = errors.New("notifier: sender closed")
	// ErrDrainTimeout reports that Close's deadline expired before the queues drained; the
	// remaining work was failed rather than left hanging.
	ErrDrainTimeout = errors.New("notifier: sender drain timeout")
)

// QueueFullError is returned by Sender.Enqueue and Sender.EnqueueResult when the selected
// lane cannot accept the send. Nothing was queued.
type QueueFullError struct {
	// Priority is the lane whose queue was full.
	Priority Priority
	// Capacity is that lane's configured queue size.
	Capacity int
	// RetryAfter is a suggested wait before trying again.
	RetryAfter time.Duration
}

func (e *QueueFullError) Error() string {
	return fmt.Sprintf("notifier: %s send queue is full (capacity %d); retry after %s",
		e.Priority, e.Capacity, e.RetryAfter)
}

// Is makes errors.Is(err, ErrQueueFull) work.
func (e *QueueFullError) Is(target error) bool { return target == ErrQueueFull }

// SendOutcome is the terminal outcome reported to a SendListener.
type SendOutcome string

const (
	// OutcomeSuccess means the hub accepted the notification (2xx).
	OutcomeSuccess SendOutcome = "success"
	// OutcomeFailure means the send failed with an error.
	OutcomeFailure SendOutcome = "failure"
	// OutcomeTimeout means the total send deadline or a network timeout elapsed.
	OutcomeTimeout SendOutcome = "timeout"
	// OutcomeDropped means a fire-and-forget send failed and has no result observer.
	OutcomeDropped SendOutcome = "dropped"
)

// SendEvent is the log-safe context for one Sender job. It carries identifiers only: no
// message body, OTP, token or recipient.
type SendEvent struct {
	NotificationID string
	IdempotencyKey string
	Priority       Priority
	Attempt        int
}

// SendListener observes Sender jobs. It is the Go equivalent of the Java SendListener and
// is optional.
//
// Callbacks run on Sender worker goroutines, never on the goroutine that called Enqueue.
// They must be fast and must not block: a slow listener occupies a worker and can reduce
// throughput. A panic in a listener is recovered and cannot kill a worker. All callbacks
// may run concurrently, so implementations must be safe for concurrent use. OnRejected is
// the exception: it runs on the goroutine that called Enqueue, because nothing was queued.
type SendListener interface {
	OnAccepted(SendEvent)
	OnCompleted(SendEvent, SendOutcome, time.Duration, error)
	OnRetry(SendEvent, int, time.Duration, error)
	OnRejected(SendEvent, error)
	OnDropped(SendEvent, error)
}

// SendResult is delivered on the channel returned by EnqueueResult. Notification is set
// only on success; Err explains a failure.
type SendResult struct {
	Notification   *Notification
	Err            error
	Attempts       int
	IdempotencyKey string
	Priority       Priority
	Duration       time.Duration
}

// SenderStats is a snapshot of Sender counters and gauges. Only sends are counted; status
// reads done through Client are not part of this Sender.
type SenderStats struct {
	// Accepted is the number of jobs queued successfully.
	Accepted uint64
	// Completed is the number of jobs the hub accepted (2xx).
	Completed uint64
	// Failed is the number of accepted jobs that ended in an error.
	Failed uint64
	// Retries is the number of retries scheduled.
	Retries uint64
	// Rejected is the number of local queue-full/closed refusals.
	Rejected uint64
	// Dropped is the number of failed jobs that had no result observer.
	Dropped uint64
	// InFlight is the number of jobs currently in a worker.
	InFlight int
	// HighQueueDepth is the instantaneous PriorityHigh queue depth.
	HighQueueDepth int
	// BulkQueueDepth is the instantaneous PriorityBulk queue depth.
	BulkQueueDepth int
}

// QueueDepth returns the depth of one lane.
func (s SenderStats) QueueDepth(p Priority) int {
	if p == PriorityHigh {
		return s.HighQueueDepth
	}
	return s.BulkQueueDepth
}

// SenderOptions configures a Sender. Zero values select the defaults documented on each
// field and in Sender.
type SenderOptions struct {
	// HighWorkers is the number of workers for PriorityHigh; zero selects 1.
	HighWorkers int
	// BulkWorkers is the number of workers for PriorityBulk; zero selects 2.
	BulkWorkers int
	// HighQueueSize bounds the PriorityHigh queue; zero selects 1,000.
	HighQueueSize int
	// BulkQueueSize bounds the PriorityBulk queue; zero selects 10,000.
	BulkQueueSize int

	// SendTimeout bounds one job from enqueue to terminal outcome, including retries. It
	// is detached from the caller's request context so a fire-and-forget send survives the
	// HTTP handler returning.
	SendTimeout time.Duration

	// MaxRetries is the number of retries after the first attempt. Zero selects the
	// default (2); a negative value disables retries.
	MaxRetries int
	// RetryBaseDelay is the first exponential-backoff delay; zero selects 200ms.
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps exponential backoff; zero selects 2s.
	RetryMaxDelay time.Duration

	// RetryAmbiguous permits retrying read-timeout/reset/5xx failures when the job has an
	// idempotency key. Leave false until the hub honours Idempotency-Key; 429 and pre-send
	// failures are retried regardless.
	RetryAmbiguous bool

	// QueueFullRetryAfter is the hint carried by QueueFullError; zero selects 500ms.
	QueueFullRetryAfter time.Duration
	// Listener receives lifecycle events; nil disables observation.
	Listener SendListener
}

func (o SenderOptions) withDefaults() SenderOptions {
	if o.HighWorkers <= 0 {
		o.HighWorkers = DefaultSenderHighWorkers
	}
	if o.BulkWorkers <= 0 {
		o.BulkWorkers = DefaultSenderBulkWorkers
	}
	if o.HighQueueSize <= 0 {
		o.HighQueueSize = DefaultSenderHighQueueSize
	}
	if o.BulkQueueSize <= 0 {
		o.BulkQueueSize = DefaultSenderBulkQueueSize
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = DefaultSenderSendTimeout
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = DefaultSenderMaxRetries
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0 // negative explicitly disables retries
	}
	if o.RetryBaseDelay <= 0 {
		o.RetryBaseDelay = DefaultSenderRetryBaseDelay
	}
	if o.RetryMaxDelay <= 0 {
		o.RetryMaxDelay = DefaultSenderRetryMaxDelay
	}
	if o.RetryMaxDelay < o.RetryBaseDelay {
		o.RetryMaxDelay = o.RetryBaseDelay
	}
	if o.QueueFullRetryAfter <= 0 {
		o.QueueFullRetryAfter = DefaultSenderQueueFullAfter
	}
	return o
}

// Sender is a bounded, in-process dispatcher for fire-and-forget and result-bearing sends.
//
// It has two independent lanes: PriorityHigh (OTP/transactional) and PriorityBulk. Each lane
// has a bounded queue, a fixed number of workers and no goroutine-per-message path. Enqueue
// never waits for the hub and never blocks when a queue is full; it returns *QueueFullError.
//
// A Sender does not own the Client: closing the Sender does not close the shared transport.
type Sender struct {
	client   *Client
	opts     SenderOptions
	high     chan *sendJob
	bulk     chan *sendJob
	shutdown chan struct{}
	workers  sync.WaitGroup

	// inFlightJobs lets Close cancel live HTTP requests when its drain deadline expires.
	inFlightJobs sync.Map

	closeMu   sync.Mutex
	closeDone chan struct{}
	closed    atomic.Bool
	aborting  atomic.Bool
	lifecycle sync.RWMutex // serializes the closed check + queue offer against Close

	live     atomic.Int64
	inFlight atomic.Int64

	accepted  atomic.Uint64
	completed atomic.Uint64
	failed    atomic.Uint64
	retries   atomic.Uint64
	rejected  atomic.Uint64
	dropped   atomic.Uint64
}

type sendJob struct {
	params         SendParams
	priority       Priority
	idempotencyKey string
	result         chan SendResult
	ctx            context.Context
	cancel         context.CancelFunc
	enqueuedAt     time.Time
}

// NewSender builds a Sender around client. It does not start any per-message goroutines:
// the worker count is the ceiling for concurrent hub requests, independent of send rate.
func NewSender(client *Client, opts SenderOptions) (*Sender, error) {
	if client == nil {
		return nil, errors.New("notifier: sender requires a client")
	}
	resolved := opts.withDefaults()
	s := &Sender{
		client:   client,
		opts:     resolved,
		high:     make(chan *sendJob, resolved.HighQueueSize),
		bulk:     make(chan *sendJob, resolved.BulkQueueSize),
		shutdown: make(chan struct{}),
	}
	s.startWorkers(resolved.HighWorkers, s.high)
	s.startWorkers(resolved.BulkWorkers, s.bulk)
	return s, nil
}

func (s *Sender) startWorkers(n int, jobs <-chan *sendJob) {
	s.workers.Add(n)
	for i := 0; i < n; i++ {
		go s.worker(jobs)
	}
}

func (s *Sender) worker(jobs <-chan *sendJob) {
	defer s.workers.Done()
	for {
		select {
		case <-s.shutdown:
			return
		case job := <-jobs:
			s.process(job)
		}
	}
}

// Enqueue accepts a send and returns as soon as it is queued. The result is not exposed;
// failures are counted and reported to the configured listener.
//
// The only errors returned are local and immediate: ErrSenderClosed, *QueueFullError, an
// invalid priority, or a failure generating an idempotency key. Network failures never
// block or return from Enqueue.
func (s *Sender) Enqueue(ctx context.Context, p SendParams) error {
	_, err := s.enqueue(ctx, p, false)
	return err
}

// EnqueueResult accepts a send and returns a buffered channel that receives exactly one
// SendResult and is then closed. A caller that abandons the channel cannot block a worker.
// Local refusals are returned as errors, the same as Enqueue.
func (s *Sender) EnqueueResult(ctx context.Context, p SendParams) (<-chan SendResult, error) {
	return s.enqueue(ctx, p, true)
}

func (s *Sender) enqueue(ctx context.Context, p SendParams, withResult bool) (<-chan SendResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed.Load() {
		s.rejected.Add(1)
		return nil, ErrSenderClosed
	}
	if _, err := p.wire(); err != nil {
		return nil, err
	}
	priority := p.Priority
	if priority == "" {
		priority = PriorityBulk
	}
	key := p.IdempotencyKey
	if key == "" {
		generated, err := NewIdempotencyKey()
		if err != nil {
			return nil, fmt.Errorf("notifier: generate idempotency key: %w", err)
		}
		key = generated
	}

	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.SendTimeout)
	job := &sendJob{
		params:         p,
		priority:       priority,
		idempotencyKey: key,
		ctx:            jobCtx,
		cancel:         cancel,
		enqueuedAt:     time.Now(),
	}
	var result chan SendResult
	if withResult {
		result = make(chan SendResult, 1)
		job.result = result
	}

	q := s.bulk
	if priority == PriorityHigh {
		q = s.high
	}

	// Hold the read lock across the queue offer so Close cannot set closed and finish its
	// drain between the check and the send, stranding an accepted job.
	s.lifecycle.RLock()
	if s.closed.Load() {
		s.lifecycle.RUnlock()
		s.rejected.Add(1)
		cancel()
		return nil, ErrSenderClosed
	}
	s.live.Add(1)
	select {
	case q <- job:
		s.accepted.Add(1)
		s.lifecycle.RUnlock()
		return result, nil
	default:
		s.live.Add(-1)
		s.rejected.Add(1)
		s.lifecycle.RUnlock()
		cancel()
		qerr := &QueueFullError{
			Priority:   priority,
			Capacity:   cap(q),
			RetryAfter: s.opts.QueueFullRetryAfter,
		}
		s.notifyRejected(SendEvent{Priority: priority, IdempotencyKey: key}, qerr)
		return nil, qerr
	}
}

func (s *Sender) process(job *sendJob) {
	defer job.cancel()
	if s.aborting.Load() {
		s.failClosed(job)
		return
	}
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	s.inFlightJobs.Store(job, struct{}{})
	defer s.inFlightJobs.Delete(job)
	if s.aborting.Load() {
		// Close may have set aborting between the check above and this store, so the
		// in-flight cancel sweep could have missed this job.
		job.cancel()
	}

	event := SendEvent{Priority: job.priority, IdempotencyKey: job.idempotencyKey}
	s.notifyAccepted(event)

	var lastErr error
	attempts := 0
	for {
		attempts++
		event.Attempt = attempts
		if err := job.ctx.Err(); err != nil {
			lastErr = err
			break
		}
		notification, err := s.send(job)
		if err == nil {
			s.finishSuccess(job, notification, attempts, event)
			return
		}
		lastErr = err
		if !s.shouldRetry(err, attempts, job) {
			break
		}
		delay := s.retryDelay(err, attempts)
		s.retries.Add(1)
		s.notifyRetry(event, attempts+1, delay, err)
		if !s.wait(job.ctx, delay) {
			lastErr = ErrSenderClosed
			break
		}
	}
	s.finishFailure(job, lastErr, attempts, event)
}

func (s *Sender) send(job *sendJob) (*Notification, error) {
	p := job.params
	p.Priority = job.priority
	p.IdempotencyKey = job.idempotencyKey
	return s.client.Send(job.ctx, p)
}

func (s *Sender) shouldRetry(err error, attempts int, job *sendJob) bool {
	if attempts > s.opts.MaxRetries || job.ctx.Err() != nil || s.closed.Load() {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return true
		}
		return s.opts.RetryAmbiguous && strings.TrimSpace(job.idempotencyKey) != "" &&
			apiErr.RetryableSend(job.idempotencyKey)
	}
	if IsPreSendError(err) {
		return true
	}
	// A transport failure after the request may have been written is ambiguous. Only retry
	// it when the caller explicitly opted in and the hub can deduplicate the key.
	return s.opts.RetryAmbiguous && strings.TrimSpace(job.idempotencyKey) != ""
}

func (s *Sender) retryDelay(err error, attempts int) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 429 && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	delay := s.opts.RetryBaseDelay
	for i := 1; i < attempts && delay < s.opts.RetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > s.opts.RetryMaxDelay {
		delay = s.opts.RetryMaxDelay
	}
	if delay <= 0 {
		return time.Millisecond
	}
	// Full jitter: a downed hub is not hit by a synchronized retry wave. This is not a
	// security-sensitive random value, so math/rand/v2 is appropriate.
	return time.Duration(rand.Int64N(int64(delay))) + time.Millisecond //nolint:gosec
}

func (s *Sender) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	case <-s.shutdown:
		return false
	}
}

func (s *Sender) finishSuccess(job *sendJob, n *Notification, attempts int, event SendEvent) {
	event.NotificationID = ""
	if n != nil {
		event.NotificationID = n.ID
	}
	s.completed.Add(1)
	if job.result != nil {
		job.result <- SendResult{
			Notification:   n,
			Attempts:       attempts,
			IdempotencyKey: job.idempotencyKey,
			Priority:       job.priority,
			Duration:       time.Since(job.enqueuedAt),
		}
		close(job.result)
	}
	s.live.Add(-1)
	s.notifyCompleted(event, OutcomeSuccess, time.Since(job.enqueuedAt), nil)
}

func (s *Sender) finishFailure(job *sendJob, err error, attempts int, event SendEvent) {
	if err == nil {
		err = errUnknownSendFailure
	}
	if s.closed.Load() && (errors.Is(err, context.Canceled) || errors.Is(err, ErrSenderClosed)) {
		err = ErrSenderClosed
	}
	s.failed.Add(1)
	if job.result != nil {
		job.result <- SendResult{
			Err:            err,
			Attempts:       attempts,
			IdempotencyKey: job.idempotencyKey,
			Priority:       job.priority,
			Duration:       time.Since(job.enqueuedAt),
		}
		close(job.result)
	} else {
		s.dropped.Add(1)
	}
	s.live.Add(-1)
	outcome := OutcomeFailure
	if isTimeout(err) {
		outcome = OutcomeTimeout
	} else if job.result == nil {
		outcome = OutcomeDropped
	}
	s.notifyCompleted(event, outcome, time.Since(job.enqueuedAt), err)
	if job.result == nil {
		s.notifyDropped(event, err)
	}
}

var errUnknownSendFailure = errors.New("notifier: send failed")

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Close stops accepting sends, waits up to ctx for accepted work to settle, then aborts and
// fails whatever is left. It is idempotent; every EnqueueResult channel is settled.
func (s *Sender) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.closeMu.Lock()
	if s.closeDone != nil {
		done := s.closeDone
		s.closeMu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ErrDrainTimeout
		}
	}
	s.lifecycle.Lock()
	s.closed.Store(true)
	s.lifecycle.Unlock()
	s.closeDone = make(chan struct{})
	done := s.closeDone
	s.closeMu.Unlock()

	err := s.drain(ctx)
	close(done)
	return err
}

func (s *Sender) drain(ctx context.Context) error {
	timedOut := false
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
wait:
	for s.live.Load() > 0 {
		select {
		case <-ctx.Done():
			timedOut = true
			break wait
		case <-ticker.C:
		}
	}

	if timedOut {
		s.aborting.Store(true)
	}
	close(s.shutdown)
	if timedOut {
		s.inFlightJobs.Range(func(key, _ any) bool {
			if job, ok := key.(*sendJob); ok {
				job.cancel()
			}
			return true
		})
	}
	s.workers.Wait()
	s.failQueued(s.high)
	s.failQueued(s.bulk)
	if timedOut {
		return ErrDrainTimeout
	}
	return nil
}

func (s *Sender) failQueued(q chan *sendJob) {
	for {
		select {
		case job := <-q:
			s.failClosed(job)
		default:
			return
		}
	}
}

func (s *Sender) failClosed(job *sendJob) {
	job.cancel()
	s.failed.Add(1)
	if job.result != nil {
		job.result <- SendResult{
			Err:            ErrSenderClosed,
			IdempotencyKey: job.idempotencyKey,
			Priority:       job.priority,
			Duration:       time.Since(job.enqueuedAt),
		}
		close(job.result)
	} else {
		s.dropped.Add(1)
	}
	s.live.Add(-1)
	event := SendEvent{Priority: job.priority, IdempotencyKey: job.idempotencyKey}
	outcome := OutcomeFailure
	if job.result == nil {
		outcome = OutcomeDropped
	}
	s.notifyCompleted(event, outcome, time.Since(job.enqueuedAt), ErrSenderClosed)
	if job.result == nil {
		s.notifyDropped(event, ErrSenderClosed)
	}
}

// Stats returns a snapshot of the counters and current queue/in-flight gauges.
func (s *Sender) Stats() SenderStats {
	return SenderStats{
		Accepted:       s.accepted.Load(),
		Completed:      s.completed.Load(),
		Failed:         s.failed.Load(),
		Retries:        s.retries.Load(),
		Rejected:       s.rejected.Load(),
		Dropped:        s.dropped.Load(),
		InFlight:       int(s.inFlight.Load()),
		HighQueueDepth: len(s.high),
		BulkQueueDepth: len(s.bulk),
	}
}

func (s *Sender) notifyAccepted(e SendEvent) {
	s.callListener(func(l SendListener) { l.OnAccepted(e) })
}

func (s *Sender) notifyCompleted(e SendEvent, outcome SendOutcome, d time.Duration, err error) {
	s.callListener(func(l SendListener) { l.OnCompleted(e, outcome, d, err) })
}

func (s *Sender) notifyRetry(e SendEvent, next int, d time.Duration, err error) {
	s.callListener(func(l SendListener) { l.OnRetry(e, next, d, err) })
}

func (s *Sender) notifyRejected(e SendEvent, err error) {
	s.callListener(func(l SendListener) { l.OnRejected(e, err) })
}

func (s *Sender) notifyDropped(e SendEvent, err error) {
	s.callListener(func(l SendListener) { l.OnDropped(e, err) })
}

// callListener isolates user code: a panic must not kill a worker or the caller.
func (s *Sender) callListener(fn func(SendListener)) {
	if s.opts.Listener == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(s.opts.Listener)
}
