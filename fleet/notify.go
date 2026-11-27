package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// NotifyMode is how a user wants to hear from the fleet.
type NotifyMode string

const (
	// NotifyNormal delivers every wake.
	NotifyNormal NotifyMode = "normal"
	// NotifyQuiet delivers only what cannot proceed without the user and
	// holds the rest for the summary.
	NotifyQuiet NotifyMode = "quiet"
	// NotifyAFK delivers nothing. What the user's grant covers is handled
	// automatically; everything else is held. The user gets a summary on
	// return, or when the grant expires.
	NotifyAFK NotifyMode = "afk"
)

// AFKAction is something the fleet may do on an absent user's behalf.
type AFKAction string

const (
	// AFKApprove approves tasks that are review_ready, landing their work
	// as their trust mode prescribes.
	AFKApprove AFKAction = "approve"
	// AFKResume resumes, once per task, a task that stopped for a reason
	// that needs no judgement: a timeout, the turn limit, a lost process.
	// It never answers an agent's question.
	AFKResume AFKAction = "resume"
)

// AFKGrant is what a user has authorised while away, and until when. The
// permitted actions are listed explicitly; anything not listed is not done.
type AFKGrant struct {
	// Actions the fleet may take. Empty means none: wakes are only held.
	Actions []AFKAction `json:"actions,omitempty"`
	// Projects limits the grant to these projects. Empty means all.
	Projects []string `json:"projects,omitempty"`
	// ExpiresAt ends the grant. Required. After it the user is back to
	// normal mode whether or not they said so.
	ExpiresAt time.Time `json:"expires_at"`
}

func (g AFKGrant) allows(action AFKAction, projectID string) bool {
	permitted := false
	for _, a := range g.Actions {
		if a == action {
			permitted = true
		}
	}
	if !permitted {
		return false
	}
	if len(g.Projects) == 0 {
		return true
	}
	for _, p := range g.Projects {
		if p == projectID {
			return true
		}
	}
	return false
}

// NotifyPrefs is a user's current notification setting.
type NotifyPrefs struct {
	User string
	Mode NotifyMode
	// Grant is meaningful only in NotifyAFK.
	Grant AFKGrant
	// Since is when the current mode began.
	Since time.Time
}

// Disposition is what was done with a wake for a user.
type Disposition string

const (
	// Delivered: the user was told.
	Delivered Disposition = "delivered"
	// Held: kept back for the summary.
	Held Disposition = "held"
	// AutoApproved: approved under an AFK grant.
	AutoApproved Disposition = "auto_approved"
	// AutoResumed: resumed under an AFK grant.
	AutoResumed Disposition = "auto_resumed"
)

// NotifyRecord is one wake and what was done with it.
type NotifyRecord struct {
	WakeID      int64
	TaskID      string
	ProjectID   string
	Kind        WakeKind
	Detail      string
	Disposition Disposition
	// Note explains a hold that was not simply policy, e.g. why an
	// automatic action could not be taken.
	Note string
	At   time.Time
}

// AwaySummary is what happened while a user was quiet or away.
type AwaySummary struct {
	Mode  NotifyMode
	Since time.Time
	Until time.Time
	// Handled lists what was done automatically under the AFK grant.
	Handled []NotifyRecord
	// Held lists what was kept back and still wants the user's attention.
	Held []NotifyRecord
}

// Empty reports whether nothing happened worth summarising.
func (s AwaySummary) Empty() bool { return len(s.Handled) == 0 && len(s.Held) == 0 }

// Notification is one message for the user. Either Wakes or Summary is set.
type Notification struct {
	User string
	// TaskID and ProjectID say what the wakes are about; TaskID is empty
	// for a project-wide or fleet-wide wake.
	TaskID    string
	ProjectID string
	// Wakes are everything pending about the task, merged into one message.
	Wakes []Wake
	// Summary is set instead of Wakes when an AFK grant has expired.
	Summary *AwaySummary
}

// Notifier turns the wake queue into messages for one user, applying that
// user's notification mode.
//
// A wake is consumed only once it has been delivered, held or acted on; a
// delivery that fails leaves it pending for the next pass. Several wakes
// about one task become one message, and a task is not messaged about more
// often than MinInterval.
//
// The fleet has one owner and one wake queue, so run one Notifier.
type Notifier struct {
	// Store holds the wake queue and preferences. Required.
	Store *Store
	// Commander carries out AFK actions. Required for them to happen;
	// without it, wakes an AFK grant would have acted on are held.
	Commander *Commander
	// User is who is being notified. Required.
	User string
	// Deliver sends one message. Required. Returning an error leaves the
	// wakes pending.
	Deliver func(ctx context.Context, n Notification) error
	// MinInterval is the least time between two messages about the same
	// task. Zero means DefaultNotifyInterval; negative disables the limit.
	MinInterval time.Duration
	// Interval is the time between passes in Run. Zero means
	// DefaultNotifyPoll.
	Interval time.Duration
}

// Notifier defaults.
const (
	DefaultNotifyInterval = time.Minute
	DefaultNotifyPoll     = 3 * time.Second
)

// ErrInvalidNotifyMode wraps every rejected notification setting.
var ErrInvalidNotifyMode = errors.New("fleet: invalid notification setting")

// SetMode changes how the user is notified. NotifyAFK requires a grant with
// a future expiry; the other modes take none.
func (n *Notifier) SetMode(ctx context.Context, mode NotifyMode, grant *AFKGrant) (NotifyPrefs, error) {
	if err := n.check(); err != nil {
		return NotifyPrefs{}, err
	}
	now := n.Store.now()
	prefs := NotifyPrefs{User: n.User, Mode: mode, Since: now.UTC()}
	switch mode {
	case NotifyNormal, NotifyQuiet:
		if grant != nil {
			return NotifyPrefs{}, fmt.Errorf("%w: %s mode takes no grant", ErrInvalidNotifyMode, mode)
		}
	case NotifyAFK:
		if grant == nil {
			return NotifyPrefs{}, fmt.Errorf("%w: afk mode needs a grant with an expiry", ErrInvalidNotifyMode)
		}
		if !grant.ExpiresAt.After(now) {
			return NotifyPrefs{}, fmt.Errorf("%w: the grant must expire in the future", ErrInvalidNotifyMode)
		}
		for _, a := range grant.Actions {
			if a != AFKApprove && a != AFKResume {
				return NotifyPrefs{}, fmt.Errorf("%w: unknown action %q", ErrInvalidNotifyMode, a)
			}
		}
		prefs.Grant = *grant
		prefs.Grant.ExpiresAt = grant.ExpiresAt.UTC()
	default:
		return NotifyPrefs{}, fmt.Errorf("%w: unknown mode %q", ErrInvalidNotifyMode, mode)
	}
	if err := n.Store.saveNotifyPrefs(ctx, prefs); err != nil {
		return NotifyPrefs{}, err
	}
	return prefs, nil
}

// Prefs returns the user's current setting. A user who has never set one is
// in NotifyNormal. An AFK grant that has expired is reported as it stands;
// the next Process pass ends it and delivers the summary.
func (n *Notifier) Prefs(ctx context.Context) (NotifyPrefs, error) {
	if err := n.check(); err != nil {
		return NotifyPrefs{}, err
	}
	return n.Store.notifyPrefs(ctx, n.User)
}

// Back returns the user to NotifyNormal and reports what happened while
// they were quiet or away. In NotifyNormal already, the summary is empty.
func (n *Notifier) Back(ctx context.Context) (AwaySummary, error) {
	if err := n.check(); err != nil {
		return AwaySummary{}, err
	}
	prefs, err := n.Store.notifyPrefs(ctx, n.User)
	if err != nil {
		return AwaySummary{}, err
	}
	return n.endAway(ctx, prefs)
}

// endAway summarises the period that is ending and switches to normal.
func (n *Notifier) endAway(ctx context.Context, prefs NotifyPrefs) (AwaySummary, error) {
	now := n.Store.now()
	summary := AwaySummary{Mode: prefs.Mode, Since: prefs.Since, Until: now.UTC()}
	if prefs.Mode != NotifyNormal {
		records, err := n.Store.notifyLogSince(ctx, n.User, prefs.Since)
		if err != nil {
			return AwaySummary{}, err
		}
		for _, r := range records {
			switch r.Disposition {
			case Held:
				summary.Held = append(summary.Held, r)
			case AutoApproved, AutoResumed:
				summary.Handled = append(summary.Handled, r)
			}
		}
	}
	if err := n.Store.saveNotifyPrefs(ctx, NotifyPrefs{User: n.User, Mode: NotifyNormal, Since: now.UTC()}); err != nil {
		return AwaySummary{}, err
	}
	return summary, nil
}

// Run processes the wake queue until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) error {
	if err := n.check(); err != nil {
		return err
	}
	interval := n.Interval
	if interval <= 0 {
		interval = DefaultNotifyPoll
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		_, _ = n.Process(ctx) // a failed pass leaves its wakes pending
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Process makes one pass over the pending wakes and returns the messages it
// delivered.
func (n *Notifier) Process(ctx context.Context) ([]Notification, error) {
	if err := n.check(); err != nil {
		return nil, err
	}
	var sent []Notification
	prefs, err := n.Store.notifyPrefs(ctx, n.User)
	if err != nil {
		return nil, err
	}
	now := n.Store.now()

	// An expired grant ends here, on the fleet's clock, not when the user
	// happens to come back.
	if prefs.Mode == NotifyAFK && !prefs.Grant.ExpiresAt.After(now) {
		summary, err := n.endAway(ctx, prefs)
		if err != nil {
			return nil, err
		}
		if !summary.Empty() {
			note := Notification{User: n.User, Summary: &summary}
			if err := n.Deliver(ctx, note); err == nil {
				sent = append(sent, note)
			}
		}
		if prefs, err = n.Store.notifyPrefs(ctx, n.User); err != nil {
			return sent, err
		}
	}

	pending, err := n.Store.PendingWakes(ctx, 0)
	if err != nil {
		return sent, err
	}
	// One group per task; a wake without a task stands alone.
	var order []string
	groups := map[string][]Wake{}
	for _, w := range pending {
		key := w.TaskID
		if key == "" {
			key = fmt.Sprintf("\x00wake:%d", w.ID)
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], w)
	}

	for _, key := range order {
		var deliver []Wake
		for _, w := range groups[key] {
			disposition, note := n.decide(ctx, prefs, w)
			if disposition == Delivered {
				deliver = append(deliver, w)
				continue
			}
			// Held or handled: settled now, whatever happens to the rest.
			if err := n.settle(ctx, w, disposition, note); err != nil {
				return sent, err
			}
		}
		if len(deliver) == 0 {
			continue
		}
		first := deliver[0]
		if first.TaskID != "" && n.minInterval() > 0 {
			last, ok, err := n.Store.lastDelivered(ctx, n.User, first.TaskID)
			if err != nil {
				return sent, err
			}
			if ok && now.Sub(last) < n.minInterval() {
				continue // too soon; these stay pending and merge into the next message
			}
		}
		note := Notification{User: n.User, TaskID: first.TaskID, ProjectID: first.ProjectID, Wakes: deliver}
		if err := n.Deliver(ctx, note); err != nil {
			continue // left pending; the next pass tries again
		}
		for _, w := range deliver {
			if err := n.settle(ctx, w, Delivered, ""); err != nil {
				return sent, err
			}
		}
		sent = append(sent, note)
	}
	return sent, nil
}

// decide says what to do with one wake under the user's current mode, and
// carries out an AFK action when one applies.
func (n *Notifier) decide(ctx context.Context, prefs NotifyPrefs, w Wake) (Disposition, string) {
	switch prefs.Mode {
	case NotifyQuiet:
		if needsTheUser(w.Kind) {
			return Delivered, ""
		}
		return Held, ""
	case NotifyAFK:
		return n.whileAway(ctx, prefs, w)
	default:
		return Delivered, ""
	}
}

// needsTheUser reports whether a wake is about something that cannot move
// until the user acts.
func needsTheUser(kind WakeKind) bool {
	switch kind {
	case WakeQuestion, WakeNeedsDecision, WakeFailed, WakeOverBudget, WakeProviderQuota:
		return true
	}
	return false
}

func (n *Notifier) whileAway(ctx context.Context, prefs NotifyPrefs, w Wake) (Disposition, string) {
	if w.TaskID == "" || n.Commander == nil {
		return Held, ""
	}
	actor := Actor{ID: n.User, Via: "afk"}
	switch w.Kind {
	case WakeReviewReady:
		if !prefs.Grant.allows(AFKApprove, w.ProjectID) {
			return Held, ""
		}
		if _, err := n.Commander.Approve(ctx, actor, w.TaskID); err != nil {
			return Held, "could not approve automatically: " + err.Error()
		}
		return AutoApproved, ""
	case WakeNeedsDecision:
		if !prefs.Grant.allows(AFKResume, w.ProjectID) || !resumableWithoutJudgement(w.Detail) {
			return Held, ""
		}
		already, err := n.Store.countDisposition(ctx, n.User, w.TaskID, AutoResumed, prefs.Since)
		if err != nil || already > 0 {
			return Held, "already resumed once while away"
		}
		if _, err := n.Commander.Reply(ctx, actor, ReplyRequest{TaskID: w.TaskID}); err != nil {
			return Held, "could not resume automatically: " + err.Error()
		}
		return AutoResumed, ""
	}
	return Held, ""
}

// resumableWithoutJudgement reports whether a task stopped for a reason that
// "carry on" answers. A question, a failed check, a refused delivery or a
// spent budget all need a person.
func resumableWithoutJudgement(reason string) bool {
	for _, prefix := range []string{
		"timed out after",
		"reached the turn limit",
		"runner stopped",
		"agent exited",
		"agent process is gone",
		"agent was left running",
	} {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

// settle records what was done with a wake and consumes it.
func (n *Notifier) settle(ctx context.Context, w Wake, d Disposition, note string) error {
	ctx = context.WithoutCancel(ctx) // a delivered message must not be delivered twice
	if err := n.Store.logNotification(ctx, n.User, w, d, note); err != nil {
		return err
	}
	return n.Store.ConsumeWake(ctx, w.ID)
}

func (n *Notifier) check() error {
	switch {
	case n.Store == nil:
		return errors.New("fleet: Notifier.Store is required")
	case strings.TrimSpace(n.User) == "":
		return errors.New("fleet: Notifier.User is required")
	case n.Deliver == nil:
		return errors.New("fleet: Notifier.Deliver is required")
	}
	return nil
}

func (n *Notifier) minInterval() time.Duration {
	if n.MinInterval == 0 {
		return DefaultNotifyInterval
	}
	return n.MinInterval
}

// ── storage ──────────────────────────────────────────────────────────────────

func (s *Store) notifyPrefs(ctx context.Context, user string) (NotifyPrefs, error) {
	var (
		mode, grant string
		since       int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT mode, grant_json, since FROM fleet_notify_prefs WHERE user_id = ?`, user).
		Scan(&mode, &grant, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return NotifyPrefs{User: user, Mode: NotifyNormal}, nil
	}
	if err != nil {
		return NotifyPrefs{}, fmt.Errorf("fleet: load notification setting: %w", err)
	}
	prefs := NotifyPrefs{User: user, Mode: NotifyMode(mode), Since: fromMillis(since)}
	if err := json.Unmarshal([]byte(grant), &prefs.Grant); err != nil {
		return NotifyPrefs{}, fmt.Errorf("fleet: decode notification grant: %w", err)
	}
	return prefs, nil
}

func (s *Store) saveNotifyPrefs(ctx context.Context, prefs NotifyPrefs) error {
	grant := []byte("{}")
	if prefs.Mode == NotifyAFK {
		var err error
		if grant, err = json.Marshal(prefs.Grant); err != nil {
			return fmt.Errorf("fleet: encode notification grant: %w", err)
		}
	}
	return s.inTx(ctx, func(tx querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fleet_notify_prefs (user_id, mode, grant_json, since, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET mode = excluded.mode, grant_json = excluded.grant_json,
				since = excluded.since, updated_at = excluded.updated_at`,
			prefs.User, string(prefs.Mode), string(grant), toMillis(prefs.Since), toMillis(s.now()))
		if err != nil {
			return fmt.Errorf("fleet: save notification setting: %w", err)
		}
		return nil
	})
}

func (s *Store) logNotification(ctx context.Context, user string, w Wake, d Disposition, note string) error {
	return s.inTx(ctx, func(tx querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fleet_notify_log
			(user_id, wake_id, task_id, project_id, kind, detail, disposition, note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			user, w.ID, w.TaskID, w.ProjectID, string(w.Kind), w.Detail, string(d), clip(note, 500), toMillis(s.now()))
		if err != nil {
			return fmt.Errorf("fleet: record notification: %w", err)
		}
		return nil
	})
}

// notifyLogSince returns what was done with wakes for a user from a point in
// time, oldest first.
func (s *Store) notifyLogSince(ctx context.Context, user string, since time.Time) ([]NotifyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT wake_id, task_id, project_id, kind, detail, disposition, note, created_at
		FROM fleet_notify_log WHERE user_id = ? AND created_at >= ? ORDER BY id ASC`, user, toMillis(since))
	if err != nil {
		return nil, fmt.Errorf("fleet: list notifications: %w", err)
	}
	defer rows.Close()
	var out []NotifyRecord
	for rows.Next() {
		var (
			r                 NotifyRecord
			kind, disposition string
			at                int64
		)
		if err := rows.Scan(&r.WakeID, &r.TaskID, &r.ProjectID, &kind, &r.Detail, &disposition, &r.Note, &at); err != nil {
			return nil, fmt.Errorf("fleet: list notifications: %w", err)
		}
		r.Kind, r.Disposition, r.At = WakeKind(kind), Disposition(disposition), fromMillis(at)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list notifications: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// lastDelivered returns when the user was last messaged about a task.
func (s *Store) lastDelivered(ctx context.Context, user, taskID string) (time.Time, bool, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM fleet_notify_log
		WHERE user_id = ? AND task_id = ? AND disposition = ?`, user, taskID, string(Delivered)).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("fleet: load last notification: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return fromMillis(at.Int64), true, nil
}

func (s *Store) countDisposition(ctx context.Context, user, taskID string, d Disposition, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fleet_notify_log
		WHERE user_id = ? AND task_id = ? AND disposition = ? AND created_at >= ?`,
		user, taskID, string(d), toMillis(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("fleet: count notifications: %w", err)
	}
	return n, nil
}
