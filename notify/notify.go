// Copyright 2015 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package notify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cespare/xxhash/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/redis/go-redis/v9"

	"github.com/prometheus/alertmanager/inhibit"
	"github.com/prometheus/alertmanager/nflog/nflogpb"
	"github.com/prometheus/alertmanager/silence"
	"github.com/prometheus/alertmanager/timeinterval"
)

// ResolvedSender returns true if resolved notifications should be sent.
type ResolvedSender interface {
	SendResolved() bool
}

// MinTimeout is the minimum timeout that is set for the context of a call
// to a notification pipeline.
const MinTimeout = 10 * time.Second

const AlertSentPrefix = "alert-sent-"

// Notifier notifies about alerts under constraints of the given context. It
// returns an error if unsuccessful and a flag whether the error is
// recoverable. This information is useful for a retry logic.
type Notifier interface {
	Notify(context.Context, ...*types.Alert) (bool, error)
}

// Integration wraps a notifier and its configuration to be uniquely identified
// by name and index from its origin in the configuration.
type Integration struct {
	notifier Notifier
	rs       ResolvedSender
	name     string
	idx      int

	mtx                       sync.RWMutex
	lastNotifyAttempt         time.Time
	lastNotifyAttemptDuration model.Duration
	lastNotifyAttemptError    error
}

// NewIntegration returns a new integration.
func NewIntegration(notifier Notifier, rs ResolvedSender, name string, idx int) *Integration {
	return &Integration{
		notifier: notifier,
		rs:       rs,
		name:     name,
		idx:      idx,
	}
}

// Notify implements the Notifier interface.
func (i *Integration) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	return i.notifier.Notify(ctx, alerts...)
}

// SendResolved implements the ResolvedSender interface.
func (i *Integration) SendResolved() bool {
	return i.rs.SendResolved()
}

// Name returns the name of the integration.
func (i *Integration) Name() string {
	return i.name
}

// Index returns the index of the integration.
func (i *Integration) Index() int {
	return i.idx
}

func (i *Integration) Report(start time.Time, duration model.Duration, notifyError error) {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	i.lastNotifyAttempt = start
	i.lastNotifyAttemptDuration = duration
	i.lastNotifyAttemptError = notifyError
}

func (i *Integration) GetReport() (time.Time, model.Duration, error) {
	i.mtx.RLock()
	defer i.mtx.RUnlock()

	return i.lastNotifyAttempt, i.lastNotifyAttemptDuration, i.lastNotifyAttemptError
}

// String implements the Stringer interface.
func (i *Integration) String() string {
	return fmt.Sprintf("%s[%d]", i.name, i.idx)
}

// notifyKey defines a custom type with which a context is populated to
// avoid accidental collisions.
type notifyKey int

const (
	keyReceiverName notifyKey = iota
	keyRepeatInterval
	keyGroupLabels
	keyGroupKey
	keyFiringAlerts
	keyResolvedAlerts
	keyNow
	keyMuteTimeIntervals
	keyActiveTimeIntervals
	keyRuleUID
)

// WithRuleUID populates a context with a receiver name.
func WithRuleUID(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, keyRuleUID, uid)
}

// WithReceiverName populates a context with a receiver name.
func WithReceiverName(ctx context.Context, rcv string) context.Context {
	return context.WithValue(ctx, keyReceiverName, rcv)
}

// WithGroupKey populates a context with a group key.
func WithGroupKey(ctx context.Context, s string) context.Context {
	return context.WithValue(ctx, keyGroupKey, s)
}

// WithFiringAlerts populates a context with a slice of firing alerts.
func WithFiringAlerts(ctx context.Context, alerts []uint64) context.Context {
	return context.WithValue(ctx, keyFiringAlerts, alerts)
}

// WithResolvedAlerts populates a context with a slice of resolved alerts.
func WithResolvedAlerts(ctx context.Context, alerts []uint64) context.Context {
	return context.WithValue(ctx, keyResolvedAlerts, alerts)
}

// WithGroupLabels populates a context with grouping labels.
func WithGroupLabels(ctx context.Context, lset model.LabelSet) context.Context {
	return context.WithValue(ctx, keyGroupLabels, lset)
}

// WithNow populates a context with a now timestamp.
func WithNow(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, keyNow, t)
}

// WithRepeatInterval populates a context with a repeat interval.
func WithRepeatInterval(ctx context.Context, t time.Duration) context.Context {
	return context.WithValue(ctx, keyRepeatInterval, t)
}

// WithMuteTimeIntervals populates a context with a slice of mute time names.
func WithMuteTimeIntervals(ctx context.Context, mt []string) context.Context {
	return context.WithValue(ctx, keyMuteTimeIntervals, mt)
}

func WithActiveTimeIntervals(ctx context.Context, at []string) context.Context {
	return context.WithValue(ctx, keyActiveTimeIntervals, at)
}

// RepeatInterval extracts a repeat interval from the context. Iff none exists, the
// second argument is false.
func RepeatInterval(ctx context.Context) (time.Duration, bool) {
	v, ok := ctx.Value(keyRepeatInterval).(time.Duration)
	return v, ok
}

func RuleUID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyRuleUID).(string)
	return v, ok
}

// ReceiverName extracts a receiver name from the context. Iff none exists, the
// second argument is false.
func ReceiverName(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyReceiverName).(string)
	return v, ok
}

// GroupKey extracts a group key from the context. Iff none exists, the
// second argument is false.
func GroupKey(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyGroupKey).(string)
	return v, ok
}

// GroupLabels extracts grouping label set from the context. Iff none exists, the
// second argument is false.
func GroupLabels(ctx context.Context) (model.LabelSet, bool) {
	v, ok := ctx.Value(keyGroupLabels).(model.LabelSet)
	return v, ok
}

// Now extracts a now timestamp from the context. Iff none exists, the
// second argument is false.
func Now(ctx context.Context) (time.Time, bool) {
	v, ok := ctx.Value(keyNow).(time.Time)
	return v, ok
}

// FiringAlerts extracts a slice of firing alerts from the context.
// Iff none exists, the second argument is false.
func FiringAlerts(ctx context.Context) ([]uint64, bool) {
	v, ok := ctx.Value(keyFiringAlerts).([]uint64)
	return v, ok
}

// ResolvedAlerts extracts a slice of firing alerts from the context.
// Iff none exists, the second argument is false.
func ResolvedAlerts(ctx context.Context) ([]uint64, bool) {
	v, ok := ctx.Value(keyResolvedAlerts).([]uint64)
	return v, ok
}

// MuteTimeIntervalNames extracts a slice of mute time names from the context. If and only if none exists, the
// second argument is false.
func MuteTimeIntervalNames(ctx context.Context) ([]string, bool) {
	v, ok := ctx.Value(keyMuteTimeIntervals).([]string)
	return v, ok
}

// ActiveTimeIntervalNames extracts a slice of active time names from the context. If none exists, the
// second argument is false.
func ActiveTimeIntervalNames(ctx context.Context) ([]string, bool) {
	v, ok := ctx.Value(keyActiveTimeIntervals).([]string)
	return v, ok
}

// A Stage processes alerts under the constraints of the given context.
type Stage interface {
	Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)
}

// StageFunc wraps a function to represent a Stage.
type StageFunc func(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)

// Exec implements Stage interface.
func (f StageFunc) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	return f(ctx, l, alerts...)
}

type Metrics struct {
	numNotifications                   *prometheus.CounterVec
	numTotalFailedNotifications        *prometheus.CounterVec
	numNotificationRequestsTotal       *prometheus.CounterVec
	numNotificationRequestsFailedTotal *prometheus.CounterVec
	notificationLatencySeconds         *prometheus.HistogramVec
}

func NewMetrics(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		numNotifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "alertmanager",
			Name:      "notifications_total",
			Help:      "The total number of attempted notifications.",
		}, []string{"integration"}),
		numTotalFailedNotifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "alertmanager",
			Name:      "notifications_failed_total",
			Help:      "The total number of failed notifications.",
		}, []string{"integration", "reason"}),
		numNotificationRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "alertmanager",
			Name:      "notification_requests_total",
			Help:      "The total number of attempted notification requests.",
		}, []string{"integration"}),
		numNotificationRequestsFailedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "alertmanager",
			Name:      "notification_requests_failed_total",
			Help:      "The total number of failed notification requests.",
		}, []string{"integration"}),
		notificationLatencySeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "alertmanager",
			Name:      "notification_latency_seconds",
			Help:      "The latency of notifications in seconds.",
			Buckets:   []float64{1, 5, 10, 15, 20},
		}, []string{"integration"}),
	}
	for _, integration := range []string{
		"email",
		"msteams",
		"pagerduty",
		"wechat",
		"pushover",
		"slack",
		"opsgenie",
		"webhook",
		"victorops",
		"sns",
		"telegram",
		"discord",
		"webex",
		"msteams",
	} {
		m.numNotifications.WithLabelValues(integration)
		m.numNotificationRequestsTotal.WithLabelValues(integration)
		m.numNotificationRequestsFailedTotal.WithLabelValues(integration)
		m.notificationLatencySeconds.WithLabelValues(integration)

		for _, reason := range possibleFailureReasonCategory {
			m.numTotalFailedNotifications.WithLabelValues(integration, reason)
		}
	}
	r.MustRegister(
		m.numNotifications, m.numTotalFailedNotifications,
		m.numNotificationRequestsTotal, m.numNotificationRequestsFailedTotal,
		m.notificationLatencySeconds,
	)
	return m
}

type PipelineBuilder struct {
	metrics *Metrics
}

func NewPipelineBuilder(r prometheus.Registerer) *PipelineBuilder {
	return &PipelineBuilder{
		metrics: NewMetrics(r),
	}
}

// New returns a map of receivers to Stages.
func (pb *PipelineBuilder) New(
	rdb redis.Cmdable,
	receivers []*Receiver,
	inhibitor *inhibit.Inhibitor,
	silencer *silence.Silencer,
	times map[string][]timeinterval.TimeInterval,
) RoutingStage {
	rs := make(RoutingStage, len(receivers))
	is := NewMuteStage(inhibitor)
	tas := NewTimeActiveStage(times)
	tms := NewTimeMuteStage(times)
	ss := NewMuteStage(silencer)

	for _, r := range receivers {
		st := createReceiverStage(r, pb.metrics, rdb)
		rs[r.groupName] = MultiStage{is, tas, tms, ss, st}
	}
	return rs
}

// createReceiverStage creates a pipeline of stages for a receiver.
func createReceiverStage(
	receiver *Receiver,
	metrics *Metrics,
	rdb redis.Cmdable,
) Stage {
	var fs FanoutStage
	for i := range receiver.integrations {
		recv := &nflogpb.Receiver{
			GroupName:   receiver.groupName,
			Integration: receiver.integrations[i].Name(),
			Idx:         uint32(receiver.integrations[i].Index()),
		}
		var s MultiStage
		s = append(s, NewDedupStage(rdb, receiver.integrations[i], recv))
		s = append(s, NewRetryStage(receiver.integrations[i], receiver.groupName, metrics))

		fs = append(fs, s)
	}
	return fs
}

// RoutingStage executes the inner stages based on the receiver specified in
// the context.
type RoutingStage map[string]Stage

// Exec implements the Stage interface.
func (rs RoutingStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	receiver, ok := ReceiverName(ctx)
	if !ok {
		return ctx, nil, errors.New("receiver missing")
	}
	s, ok := rs[receiver]
	if !ok {
		group, _ := GroupKey(ctx)
		return ctx, nil, errors.Errorf("stage for receiver [%s] group [%s] missing", receiver, group)

	}

	return s.Exec(ctx, l, alerts...)
}

// A MultiStage executes a series of stages sequentially.
type MultiStage []Stage

// Exec implements the Stage interface.
func (ms MultiStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var err error
	for _, s := range ms {
		if len(alerts) == 0 {
			return ctx, nil, nil
		}

		ctx, alerts, err = s.Exec(ctx, l, alerts...)
		if err != nil {
			return ctx, nil, err
		}
	}
	return ctx, alerts, nil
}

// FanoutStage executes its stages concurrently
type FanoutStage []Stage

// Exec attempts to execute all stages concurrently and discards the results.
// It returns its input alerts and a types.MultiError if one or more stages fail.
func (fs FanoutStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var (
		wg sync.WaitGroup
		me types.MultiError
		as []*types.Alert
	)
	wg.Add(len(fs))

	for _, s := range fs {
		go func(s Stage) {
			if _, _, err := s.Exec(ctx, l, alerts...); err != nil {
				me.Add(err)
			}
			wg.Done()
		}(s)
	}
	wg.Wait()

	if me.Len() > 0 {
		return ctx, as, &me
	}
	return ctx, as, nil
}

// MuteStage filters alerts through a Muter.
type MuteStage struct {
	muter types.Muter
}

// NewMuteStage return a new MuteStage.
func NewMuteStage(m types.Muter) *MuteStage {
	return &MuteStage{muter: m}
}

// Exec implements the Stage interface.
func (n *MuteStage) Exec(ctx context.Context, _ log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var filtered []*types.Alert
	for _, a := range alerts {
		// TODO(fabxc): increment total alerts counter.
		// Do not send the alert if muted.
		if !n.muter.Mutes(a.Labels) {
			filtered = append(filtered, a)
		}
		// TODO(fabxc): increment muted alerts counter if muted.
	}
	return ctx, filtered, nil
}

// WaitStage waits for a certain amount of time before continuing or until the
// context is done.
type WaitStage struct {
	wait func() time.Duration
}

// DedupStage filters alerts.
// Filtering happens based on a notification log.
type DedupStage struct {
	rs   ResolvedSender
	recv *nflogpb.Receiver
	rdb  redis.Cmdable
	now  func() time.Time
	hash func(*types.Alert) uint64
}

// NewDedupStage wraps a DedupStage that runs against the given notification log.
func NewDedupStage(rdb redis.Cmdable, rs ResolvedSender, recv *nflogpb.Receiver) *DedupStage {
	return &DedupStage{
		rdb:  rdb,
		rs:   rs,
		recv: recv,
		now:  now,
		hash: hashAlert,
	}
}

func now() time.Time {
	return time.Now()
}

// Wrap a slice in a struct so we can store a pointer in sync.Pool
type hashBuffer struct {
	buf []byte
}

var hashBuffers = sync.Pool{
	New: func() interface{} { return &hashBuffer{buf: make([]byte, 0, 1024)} },
}

func hashAlert(a *types.Alert) uint64 {
	const sep = '\xff'

	hb := hashBuffers.Get().(*hashBuffer)
	defer hashBuffers.Put(hb)
	b := hb.buf[:0]

	names := make(model.LabelNames, 0, len(a.Labels))

	for ln := range a.Labels {
		names = append(names, ln)
	}
	sort.Sort(names)

	for _, ln := range names {
		b = append(b, string(ln)...)
		b = append(b, sep)
		b = append(b, string(a.Labels[ln])...)
		b = append(b, sep)
	}

	hash := xxhash.Sum64(b)

	return hash
}

func stateKey(k string, r *nflogpb.Receiver, hash uint64) string {
	return fmt.Sprintf("%s:%s:%d", k, receiverKey(r), hash)
}

func receiverKey(r *nflogpb.Receiver) string {
	return fmt.Sprintf("%s:%s:%d", r.GroupName, r.Integration, r.Idx)
}

// Exec implements the Stage interface.
func (n *DedupStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	gkey, ok := GroupKey(ctx)
	if !ok {
		return ctx, nil, errors.New("group key missing")
	}
	repeatInterval, ok := RepeatInterval(ctx)
	if !ok {
		return ctx, nil, errors.New("repeat interval missing")
	}
	var firing []uint64
	var resolved []uint64
	needsUpdateAlerts := make([]*types.Alert, 0)
	var hash uint64
	for _, a := range alerts {
		hash = n.hash(a)
		sKey := stateKey(gkey, n.recv, hash)
		if a.Resolved() {
			resolved = append(resolved, hash)
			exist, err := n.rdb.Exists(ctx, sKey).Result()
			if err != nil {
				level.Error(l).Log("msg", "Exist stateKey from redis failed", "stateKey", sKey, "err", err)
				continue
			}
			// If the firing alert send, need send resolved message, otherwise, no need.
			if exist == 1 {
				if count, err := n.rdb.Get(ctx, AlertSentPrefix+sKey).Int64(); err == nil {
					a.SentCount = count
				}
				needsUpdateAlerts = append(needsUpdateAlerts, a)

			}
		} else {
			stage, err := n.rdb.Get(ctx, sKey).Result()
			if err != nil {
				stage = a.Stage
				level.Error(l).Log("msg", "Get stage from redis failed", "stateKey", sKey, "err", err)
			}
			if stage != "" && stage != a.Stage {
				n.rdb.Del(ctx, sKey)
			}
			needsUpdate, err := n.rdb.SetNX(ctx, sKey, a.Stage, repeatInterval).Result()
			if err != nil {
				level.Error(l).Log("msg", "Set stateKey to redis failed", "stateKey", sKey, "stage", stage, "err", err)
				continue
			}
			if needsUpdate {
				firing = append(firing, hash)
				if count, err := n.rdb.Incr(ctx, AlertSentPrefix+sKey).Result(); err == nil {
					a.SentCount = count
				}
				needsUpdateAlerts = append(needsUpdateAlerts, a)
			}
		}
		ctx = WithRuleUID(ctx, a.RuleUID)
	}
	ctx = WithFiringAlerts(ctx, firing)
	ctx = WithResolvedAlerts(ctx, resolved)

	return ctx, needsUpdateAlerts, nil
}

// RetryStage notifies via passed integration with exponential backoff until it
// succeeds. It aborts if the context is canceled or timed out.
type RetryStage struct {
	integration *Integration
	groupName   string
	metrics     *Metrics
}

// NewRetryStage returns a new instance of a RetryStage.
func NewRetryStage(i *Integration, groupName string, metrics *Metrics) *RetryStage {
	return &RetryStage{
		integration: i,
		groupName:   groupName,
		metrics:     metrics,
	}
}

func (r RetryStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	r.metrics.numNotifications.WithLabelValues(r.integration.Name()).Inc()
	ctx, alerts, err := r.exec(ctx, l, alerts...)

	failureReason := DefaultReason.String()
	if err != nil {
		if e, ok := errors.Cause(err).(*ErrorWithReason); ok {
			failureReason = e.Reason.String()
		}
		r.metrics.numTotalFailedNotifications.WithLabelValues(r.integration.Name(), failureReason).Inc()
	}
	return ctx, alerts, err
}

func (r RetryStage) exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var sent []*types.Alert

	// If we shouldn't send notifications for resolved alerts, but there are only
	// resolved alerts, report them all as successfully notified (we still want the
	// notification log to log them for the next run of DedupStage).
	if !r.integration.SendResolved() {
		firing, ok := FiringAlerts(ctx)
		if !ok {
			return ctx, nil, errors.New("firing alerts missing")
		}
		if len(firing) == 0 {
			return ctx, alerts, nil
		}
		for _, a := range alerts {
			if a.Status() != model.AlertResolved {
				sent = append(sent, a)
			}
		}
	} else {
		sent = alerts
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 0 // Always retry.

	tick := backoff.NewTicker(b)
	defer tick.Stop()

	var (
		i    = 0
		iErr error
	)

	l = log.With(l, "receiver", r.groupName, "integration", r.integration.String())
	if groupKey, ok := GroupKey(ctx); ok {
		l = log.With(l, "aggrGroup", groupKey)
	}

	for {
		i++
		// Always check the context first to not notify again.
		select {
		case <-ctx.Done():
			if iErr == nil {
				iErr = ctx.Err()
			}

			return ctx, nil, errors.Wrapf(iErr, "%s/%s: notify retry canceled after %d attempts", r.groupName, r.integration.String(), i)
		default:
		}

		select {
		case <-tick.C:
			now := time.Now()
			retry, err := r.integration.Notify(ctx, sent...)
			duration := time.Since(now)

			r.metrics.notificationLatencySeconds.WithLabelValues(r.integration.Name()).Observe(duration.Seconds())
			r.metrics.numNotificationRequestsTotal.WithLabelValues(r.integration.Name()).Inc()
			r.integration.Report(now, model.Duration(duration), err)
			if err != nil {
				r.metrics.numNotificationRequestsFailedTotal.WithLabelValues(r.integration.Name()).Inc()
				if !retry {
					return ctx, alerts, errors.Wrapf(err, "%s/%s: notify retry canceled due to unrecoverable error after %d attempts", r.groupName, r.integration.String(), i)
				}
				if ctx.Err() == nil && (iErr == nil || err.Error() != iErr.Error()) {
					// Log the error if the context isn't done and the error isn't the same as before.
					level.Warn(l).Log("msg", "Notify attempt failed, will retry later", "attempts", i, "err", err)
				}

				// Save this error to be able to return the last seen error by an
				// integration upon context timeout.
				iErr = err
			} else {
				lvl := level.Info(l)
				if i <= 1 {
					lvl = level.Debug(log.With(l, "alerts", fmt.Sprintf("%v", alerts)))
				}

				lvl.Log("msg", "Notify success", "attempts", i)
				return ctx, alerts, nil
			}
		case <-ctx.Done():
			if iErr == nil {
				iErr = ctx.Err()
			}

			return ctx, nil, errors.Wrapf(iErr, "%s/%s: notify retry canceled after %d attempts", r.groupName, r.integration.String(), i)
		}
	}
}

// ClearSKeyStage sets the notification information about passed alerts. The
// passed alerts should have already been sent to the receivers.
type ClearSKeyStage struct {
	rdb  redis.Cmdable
	recv *nflogpb.Receiver
}

// NewClearSKeysStage returns a new instance of a ClearSKeyStage.
func NewClearSKeysStage(rdb redis.Cmdable, recv *nflogpb.Receiver) *ClearSKeyStage {
	return &ClearSKeyStage{
		rdb:  rdb,
		recv: recv,
	}
}

// Exec implements the Stage interface.
func (n ClearSKeyStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	gkey, ok := GroupKey(ctx)
	if !ok {
		return ctx, nil, errors.New("group key missing")
	}
	ruleUID, ok := RuleUID(ctx)
	if !ok {
		return ctx, alerts, nil
	}
	if firing, ok := FiringAlerts(ctx); ok && len(firing) > 0 {
		stateKeys := make([]string, len(firing))
		for _, hash := range firing {
			sKey := stateKey(gkey, n.recv, hash)
			stateKeys = append(stateKeys, sKey, AlertSentPrefix+sKey)
		}

		if err := n.rdb.SAdd(ctx, ruleUID, stateKeys).Err(); err != nil {
			level.Error(l).Log("msg", "Set rule uid idx to redis failed", "UID", ruleUID, "err", err)
		}
	}

	if resolved, ok := ResolvedAlerts(ctx); ok && len(resolved) > 0 {
		stateKeys := make([]string, len(resolved))
		for _, hash := range resolved {
			sKey := stateKey(gkey, n.recv, hash)
			stateKeys = append(stateKeys, sKey, AlertSentPrefix+sKey)
		}

		if err := n.rdb.Del(ctx, stateKeys...).Err(); err != nil {
			level.Error(l).Log("msg", "Del stateKeys to redis failed", "stateKeys", strings.Join(stateKeys, ","))
		}
		if err := n.rdb.SRem(ctx, ruleUID, stateKeys).Err(); err != nil {
			level.Error(l).Log("msg", "Del stateKeys idx to redis failed", "stateKeys", strings.Join(stateKeys, ","))
		}
	}
	return ctx, alerts, nil
}

type timeStage struct {
	Times map[string][]timeinterval.TimeInterval
}

type TimeMuteStage timeStage

func NewTimeMuteStage(ti map[string][]timeinterval.TimeInterval) *TimeMuteStage {
	return &TimeMuteStage{ti}
}

// Exec implements the stage interface for TimeMuteStage.
// TimeMuteStage is responsible for muting alerts whose route is not in an active time.
func (tms TimeMuteStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	muteTimeIntervalNames, ok := MuteTimeIntervalNames(ctx)
	if !ok {
		return ctx, alerts, nil
	}
	now, ok := Now(ctx)
	if !ok {
		return ctx, alerts, errors.New("missing now timestamp")
	}

	muted, err := inTimeIntervals(now, tms.Times, muteTimeIntervalNames)
	if err != nil {
		return ctx, alerts, err
	}

	// If the current time is inside a mute time, all alerts are removed from the pipeline.
	if muted {
		level.Debug(l).Log("msg", "Notifications not sent, route is within mute time")
		return ctx, nil, nil
	}
	return ctx, alerts, nil
}

type TimeActiveStage timeStage

func NewTimeActiveStage(ti map[string][]timeinterval.TimeInterval) *TimeActiveStage {
	return &TimeActiveStage{ti}
}

// Exec implements the stage interface for TimeActiveStage.
// TimeActiveStage is responsible for muting alerts whose route is not in an active time.
func (tas TimeActiveStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	activeTimeIntervalNames, ok := ActiveTimeIntervalNames(ctx)
	if !ok {
		return ctx, alerts, nil
	}

	// if we don't have active time intervals at all it is always active.
	if len(activeTimeIntervalNames) == 0 {
		return ctx, alerts, nil
	}

	now, ok := Now(ctx)
	if !ok {
		return ctx, alerts, errors.New("missing now timestamp")
	}

	active, err := inTimeIntervals(now, tas.Times, activeTimeIntervalNames)
	if err != nil {
		return ctx, alerts, err
	}

	// If the current time is not inside an active time, all alerts are removed from the pipeline
	if !active {
		level.Debug(l).Log("msg", "Notifications not sent, route is not within active time")
		return ctx, nil, nil
	}

	return ctx, alerts, nil
}

// inTimeIntervals returns true if the current time is contained in one of the given time intervals.
func inTimeIntervals(now time.Time, intervals map[string][]timeinterval.TimeInterval, intervalNames []string) (bool, error) {
	for _, name := range intervalNames {
		interval, ok := intervals[name]
		if !ok {
			return false, errors.Errorf("time interval %s doesn't exist in config", name)
		}
		for _, ti := range interval {
			if ti.ContainsTime(now.UTC()) {
				return true, nil
			}
		}
	}
	return false, nil
}
