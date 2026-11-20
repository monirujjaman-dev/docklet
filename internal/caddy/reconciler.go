package caddy

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Source provides the desired routes and options.
type Source interface {
	DesiredRoutes(ctx context.Context) ([]Route, Options, error)
}

// Status describes the last reconcile attempt.
type Status struct {
	Reachable bool      `json:"reachable"`
	LastSync  time.Time `json:"lastSync,omitzero"`
	LastError string    `json:"lastError,omitempty"`
}

// Reconciler keeps Caddy's config equal to the desired state. It runs on
// start, whenever Trigger is called, and on a short interval so a restarted
// or hand-edited Caddy is corrected automatically.
type Reconciler struct {
	client   *Client
	src      Source
	log      *slog.Logger
	interval time.Duration
	trigger  chan struct{}

	passMu sync.Mutex // serialises reconcile passes

	mu     sync.Mutex
	status Status
	synced chan struct{} // closed and replaced after every pass (for waiters)
}

// NewReconciler creates a reconciler; interval <= 0 means 10s.
func NewReconciler(client *Client, src Source, log *slog.Logger, interval time.Duration) *Reconciler {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Reconciler{
		client: client, src: src, log: log, interval: interval,
		trigger: make(chan struct{}, 1),
		synced:  make(chan struct{}),
	}
}

// Trigger requests a reconcile soon. It never blocks.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Status returns the latest status.
func (r *Reconciler) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Run reconciles until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	_ = r.Once(ctx) // errors are recorded in Status
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.trigger:
		}
		_ = r.Once(ctx)
	}
}

// Sync applies the desired config now and returns once Caddy has it. Use it
// before tearing down an old upstream so traffic has already moved.
func (r *Reconciler) Sync(ctx context.Context) error { return r.Once(ctx) }

// Once performs a single reconcile pass and returns its error.
func (r *Reconciler) Once(ctx context.Context) error {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	err := r.reconcile(ctx)
	r.mu.Lock()
	if err != nil {
		r.status.LastError = err.Error()
	} else {
		r.status.LastError = ""
		r.status.LastSync = time.Now()
	}
	close(r.synced)
	r.synced = make(chan struct{})
	r.mu.Unlock()
	return err
}

// WaitSync blocks until the next reconcile pass finishes or ctx ends.
func (r *Reconciler) WaitSync(ctx context.Context) {
	r.mu.Lock()
	ch := r.synced
	r.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	routes, opts, err := r.src.DesiredRoutes(ctx)
	if err != nil {
		return err
	}
	opts.AdminListen = r.client.AdminListen()
	desired := BuildConfig(routes, opts)

	current, err := r.client.Config(ctx)
	r.setReachable(err == nil)
	if err != nil {
		return err
	}
	if Equal(current, desired) {
		return nil
	}
	if err := r.client.Load(ctx, desired); err != nil {
		return err
	}
	r.log.Info("caddy config applied", "routes", len(routes))
	return nil
}

func (r *Reconciler) setReachable(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Reachable != ok {
		if ok {
			r.log.Info("caddy admin API reachable")
		} else {
			r.log.Warn("caddy admin API unreachable")
		}
	}
	r.status.Reachable = ok
}
