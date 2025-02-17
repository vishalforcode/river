package leadership

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/riverqueue/river/internal/baseservice"
	"github.com/riverqueue/river/internal/maintenance/startstop"
	"github.com/riverqueue/river/internal/notifier"
	"github.com/riverqueue/river/internal/rivercommon"
	"github.com/riverqueue/river/internal/util/dbutil"
	"github.com/riverqueue/river/riverdriver"
)

const (
	electInterval           = 5 * time.Second
	electInteralJitter      = 1 * time.Second
	electIntervalTTLPadding = 10 * time.Second
)

type pgNotification struct {
	Name     string `json:"name"`
	LeaderID string `json:"leader_id"`
	Action   string `json:"action"`
}

type Notification struct {
	IsLeader  bool
	Timestamp time.Time
}

type Subscription struct {
	creationTime time.Time
	ch           chan *Notification

	unlistenOnce *sync.Once
	e            *Elector
}

func (s *Subscription) C() <-chan *Notification {
	return s.ch
}

func (s *Subscription) Unlisten() {
	s.unlistenOnce.Do(func() {
		s.e.unlisten(s)
	})
}

// Test-only properties.
type electorTestSignals struct {
	DeniedLeadership     rivercommon.TestSignal[struct{}] // notifies when elector fails to gain leadership
	GainedLeadership     rivercommon.TestSignal[struct{}] // notifies when elector gains leadership
	LostLeadership       rivercommon.TestSignal[struct{}] // notifies when an elected leader loses leadership
	MaintainedLeadership rivercommon.TestSignal[struct{}] // notifies when elector maintains leadership
	ResignedLeadership   rivercommon.TestSignal[struct{}] // notifies when elector resigns leadership
}

func (ts *electorTestSignals) Init() {
	ts.DeniedLeadership.Init()
	ts.GainedLeadership.Init()
	ts.LostLeadership.Init()
	ts.MaintainedLeadership.Init()
	ts.ResignedLeadership.Init()
}

type Elector struct {
	baseservice.BaseService
	startstop.BaseStartStop

	clientID                   string
	electInterval              time.Duration // period on which each elector attempts elect even without having received a resignation notification
	electIntervalJitter        time.Duration
	exec                       riverdriver.Executor
	instanceName               string
	leadershipNotificationChan chan struct{}
	notifier                   *notifier.Notifier
	testSignals                electorTestSignals
	ttl                        time.Duration

	mu            sync.Mutex
	isLeader      bool
	subscriptions []*Subscription
}

// NewElector returns an Elector using the given adapter. The name should correspond
// to the name of the database + schema combo and should be shared across all Clients
// running with that combination. The id should be unique to the Client.
func NewElector(archetype *baseservice.Archetype, exec riverdriver.Executor, notifier *notifier.Notifier, instanceName, clientID string) *Elector {
	return baseservice.Init(archetype, &Elector{
		exec:                exec,
		clientID:            clientID,
		electInterval:       electInterval,
		electIntervalJitter: electInteralJitter,
		instanceName:        instanceName,
		notifier:            notifier,

		// TTL is at least the relect run interval used by clients to try and
		// gain leadership or reelect themselves as leader, plus a little
		// padding to account to give the leader a little breathing room in its
		// reelection loop.
		ttl: electInterval + electIntervalTTLPadding,
	})
}

func (e *Elector) Start(ctx context.Context) error {
	ctx, shouldStart, stopped := e.StartInit(ctx)
	if !shouldStart {
		return nil
	}

	// We'll send to this channel anytime a leader resigns on the key with `name`
	e.leadershipNotificationChan = make(chan struct{})

	var sub *notifier.Subscription
	if e.notifier == nil {
		e.Logger.InfoContext(ctx, e.Name+": No notifier configured; starting in poll mode", "client_id", e.clientID)
	} else {
		handleNotification := func(topic notifier.NotificationTopic, payload string) {
			if topic != notifier.NotificationTopicLeadership {
				// This should not happen unless the notifier is broken.
				e.Logger.Error(e.Name+": Received unexpected notification", "client_id", e.clientID, "topic", topic, "payload", payload)
				return
			}

			notification := pgNotification{}
			if err := json.Unmarshal([]byte(payload), &notification); err != nil {
				e.Logger.Error(e.Name+": Unable to unmarshal leadership notification", "client_id", e.clientID, "err", err)
				return
			}

			e.Logger.InfoContext(ctx, e.Name+": Received notification from notifier", "action", notification.Action, "client_id", e.clientID)

			if notification.Action != "resigned" || notification.Name != e.instanceName {
				// We only care about resignations because we use them to preempt the
				// election attempt backoff. And we only care about our own key name.
				return
			}

			select {
			case <-ctx.Done():
				return
			case e.leadershipNotificationChan <- struct{}{}:
			}
		}

		e.Logger.InfoContext(ctx, e.Name+": Listening for leadership changes", "client_id", e.clientID, "topic", notifier.NotificationTopicLeadership)

		// TODO(brandur): Get rid of this retry loop after refactor.
		var err error
		sub, err = notifier.ListenRetryLoop(ctx, &e.BaseService, e.notifier, notifier.NotificationTopicLeadership, handleNotification)
		if err != nil {
			close(stopped)
			if strings.HasSuffix(err.Error(), "conn closed") || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}

	go func() {
		// This defer should come first so that it's last out, thereby avoiding
		// races.
		defer close(stopped)

		e.Logger.InfoContext(ctx, e.Name+": Run loop started")
		defer e.Logger.InfoContext(ctx, e.Name+": Run loop stopped")

		if sub != nil {
			defer sub.Unlisten(ctx)
		}

		for {
			if err := e.attemptGainLeadershipLoop(ctx); err != nil {
				// Function above only returns an error if context was cancelled
				// or overall context is done.
				if !errors.Is(err, context.Canceled) && ctx.Err() == nil {
					panic(err)
				}
				return
			}

			e.Logger.InfoContext(ctx, e.Name+": Gained leadership", "client_id", e.clientID)
			e.testSignals.GainedLeadership.Signal(struct{}{})

			err := e.keepLeadershipLoop(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}

				if errors.Is(err, errLostLeadershipReelection) {
					continue // lost leadership reelection; unusual but not a problem; don't log
				}

				e.Logger.Error(e.Name+": Error keeping leadership", "client_id", e.clientID, "err", err)
			}
		}
	}()

	return nil
}

func (e *Elector) attemptGainLeadershipLoop(ctx context.Context) error {
	var numErrors int

	for {
		e.Logger.InfoContext(ctx, e.Name+": Attempting to gain leadership", "client_id", e.clientID)

		elected, err := attemptElectOrReelect(ctx, e.exec, false, &riverdriver.LeaderElectParams{
			LeaderID: e.clientID,
			Name:     e.instanceName,
			TTL:      e.ttl,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return err
			}

			numErrors++
			e.Logger.Error(e.Name+": Error attempting to elect", "client_id", e.clientID, "err", err, "num_errors", numErrors)
			e.CancellableSleepExponentialBackoff(ctx, numErrors-1, baseservice.MaxAttemptsBeforeResetDefault)
			continue
		}
		if elected {
			return nil
		}

		numErrors = 0

		e.Logger.DebugContext(ctx, e.Name+": Leadership bid was unsuccessful (not an error)", "client_id", e.clientID)
		e.testSignals.DeniedLeadership.Signal(struct{}{})

		select {
		// TODO: This could potentially leak memory / timers if we're seeing a ton
		// of resignations. May want to make this reusable & cancel it when retrying?
		// We may also want to consider a specialized ticker utility that can tick
		// within a random range.
		case <-e.CancellableSleepRandomBetweenC(ctx, e.electInterval, e.electInterval+e.electIntervalJitter):
			if ctx.Err() != nil { // context done
				return ctx.Err()
			}

		case <-e.leadershipNotificationChan:
			// Somebody just resigned, try to win the next election after a very
			// short random interval (to prevent all clients from bidding at once).
			e.CancellableSleepRandomBetween(ctx, 0, 50*time.Millisecond)
		}
	}
}

var errLostLeadershipReelection = errors.New("lost leadership with no error")

func (e *Elector) keepLeadershipLoop(ctx context.Context) error {
	// notify all subscribers that we're the leader
	e.notifySubscribers(true)

	// Defer is LIFO. This will run after the resign below.
	defer e.notifySubscribers(false)

	var lostLeadership bool

	// Before the elector returns, run a delete with NOTIFY to give up any
	// leadership that we have. If we do that here, we guarantee that any locks
	// we have will be released (even if they were acquired in
	// attemptGainLeadership but we didn't wait for the response)
	//
	// This doesn't use ctx because it runs *after* the ctx is done.
	defer func() {
		if !lostLeadership {
			e.attemptResignLoop(ctx) // will resign using background context, but ctx sent for logging
		}
	}()

	const maxNumErrors = 5

	var (
		numErrors = 0
		timer     = time.NewTimer(0) // reset immediately below
	)
	<-timer.C

	for {
		timer.Reset(e.electInterval)

		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return ctx.Err()

		case <-timer.C:
			// Reelect timer expired; attempt releection below.

		case <-e.leadershipNotificationChan:
			// Used only in tests for force an immediately reelect attempt.

			if !timer.Stop() {
				<-timer.C
			}
		}

		e.Logger.InfoContext(ctx, e.Name+": Current leader attempting reelect", "client_id", e.clientID)

		reelected, err := attemptElectOrReelect(ctx, e.exec, true, &riverdriver.LeaderElectParams{
			LeaderID: e.clientID,
			Name:     e.instanceName,
			TTL:      e.ttl,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}

			numErrors++
			if numErrors >= maxNumErrors {
				return err
			}

			e.Logger.Error(e.Name+": Error attempting reelection", "client_id", e.clientID, "err", err)
			e.CancellableSleepExponentialBackoff(ctx, numErrors-1, baseservice.MaxAttemptsBeforeResetDefault)
			continue
		}
		if !reelected {
			lostLeadership = true
			e.testSignals.LostLeadership.Signal(struct{}{})
			return errLostLeadershipReelection
		}

		numErrors = 0
		e.testSignals.MaintainedLeadership.Signal(struct{}{})
	}
}

// Try up to 3 times to give up any currently held leadership.
//
// The context received is used for logging purposes, but the function actually
// makes use of a background context to try and guarantee that leadership is
// always surrendered in a timely manner so it can be picked up quickly by
// another client, even in the event of a cancellation.
func (e *Elector) attemptResignLoop(ctx context.Context) {
	e.Logger.InfoContext(ctx, e.Name+": Attempting to resign leadership", "client_id", e.clientID)

	// Make a good faith attempt to resign, even in the presence of errors, but
	// don't keep hammering if it doesn't work. In case a resignation failure,
	// leader TTLs will act as an additional hedge to ensure a new leader can
	// still be elected.
	const maxNumErrors = 3

	// This does not inherit the parent context because we want to give up leadership
	// even during a shutdown. There is no way to short-circuit this.
	ctx = context.Background()

	for attempt := 1; attempt <= maxNumErrors; attempt++ {
		if err := e.attemptResign(ctx, attempt); err != nil { //nolint:contextcheck
			e.Logger.Error(e.Name+": Error attempting to resign", "attempt", attempt, "client_id", e.clientID, "err", err)

			e.CancellableSleepExponentialBackoff(ctx, attempt-1, baseservice.MaxAttemptsBeforeResetDefault) //nolint:contextcheck

			continue
		}

		return
	}
}

// attemptResign attempts to resign any currently held leaderships for the
// elector's name and leader ID.
func (e *Elector) attemptResign(ctx context.Context, attempt int) error {
	// Wait one second longer each time we try to resign:
	timeout := time.Duration(attempt) * time.Second

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resigned, err := e.exec.LeaderResign(ctx, &riverdriver.LeaderResignParams{
		LeaderID:        e.clientID,
		LeadershipTopic: string(notifier.NotificationTopicLeadership),
		Name:            e.instanceName,
	})
	if err != nil {
		return err
	}

	if resigned {
		e.Logger.InfoContext(ctx, e.Name+": Resigned leadership successfully", "client_id", e.clientID)
		e.testSignals.ResignedLeadership.Signal(struct{}{})
	}

	return nil
}

func (e *Elector) Listen() *Subscription {
	sub := &Subscription{
		creationTime: time.Now().UTC(),
		ch:           make(chan *Notification, 1),
		e:            e,
		unlistenOnce: &sync.Once{},
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	initialNotification := &Notification{
		IsLeader:  e.isLeader,
		Timestamp: sub.creationTime,
	}
	sub.ch <- initialNotification

	e.subscriptions = append(e.subscriptions, sub)
	return sub
}

func (e *Elector) unlisten(sub *Subscription) {
	success := e.tryUnlisten(sub)
	if !success {
		panic("BUG: tried to unlisten for subscription not in list")
	}
}

// needs to be in a separate method so the defer will cleanly unlock the mutex,
// even if we panic.
func (e *Elector) tryUnlisten(sub *Subscription) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, s := range e.subscriptions {
		if s.creationTime.Equal(sub.creationTime) {
			e.subscriptions = append(e.subscriptions[:i], e.subscriptions[i+1:]...)
			return true
		}
	}
	return false
}

func (e *Elector) notifySubscribers(isLeader bool) {
	notifyTime := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()

	e.isLeader = isLeader

	for _, s := range e.subscriptions {
		s.ch <- &Notification{
			IsLeader:  isLeader,
			Timestamp: notifyTime,
		}
	}
}

const deadlineTimeout = 5 * time.Second

// attemptElectOrReelect attempts to elect a leader for the given name. The
// bool alreadyElected indicates whether this is a potential reelection of
// an already-elected leader. If the election is successful because there is
// no leader or the previous leader expired, the provided leaderID will be
// set as the new leader with a TTL of ttl.
//
// Returns whether this leader was successfully elected or an error if one
// occurred.
func attemptElectOrReelect(ctx context.Context, exec riverdriver.Executor, alreadyElected bool, params *riverdriver.LeaderElectParams) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, deadlineTimeout)
	defer cancel()

	return dbutil.WithTxV(ctx, exec, func(ctx context.Context, exec riverdriver.ExecutorTx) (bool, error) {
		if _, err := exec.LeaderDeleteExpired(ctx, params.Name); err != nil {
			return false, err
		}

		var (
			elected bool
			err     error
		)
		if alreadyElected {
			elected, err = exec.LeaderAttemptReelect(ctx, params)
		} else {
			elected, err = exec.LeaderAttemptElect(ctx, params)
		}
		if err != nil {
			return false, err
		}

		return elected, nil
	})
}
