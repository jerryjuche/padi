// Package approvals implements the M3b approval gate: policy evaluation,
// owner decisions, expiry and the Telegram inline-keyboard payload helpers.
//
// This package deliberately does not import internal/gateway/telegram (or any
// other gateway package): the store is supplied by the caller through small
// consumer-defined interfaces, which keeps the gate free of import cycles.
package approvals

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Decision is the policy outcome for an action.
type Decision string

const (
	// Auto needs no owner approval.
	Auto Decision = "auto"
	// Ask requires an owner decision before the action proceeds.
	Ask Decision = "ask"
	// Deny is absolute: the owner cannot approve it.
	Deny Decision = "deny"
	// AutoReadonly is an allowed read-only action that is still audited.
	AutoReadonly Decision = "auto_readonly"
)

// Action identifies an action subject to the approval policy.
type Action string

const (
	ActionTaskContract       Action = "task_contract"
	ActionPlanNewApp         Action = "plan_new_app"
	ActionEditProtectedPath  Action = "edit_protected_path"
	ActionDeleteDirOrMany    Action = "delete_dir_or_many"
	ActionPushBranch         Action = "push_branch"
	ActionOpenPR             Action = "open_pr"
	ActionStartPreview       Action = "start_preview"
	ActionNewDomain          Action = "new_domain"
	ActionWebResearch        Action = "web_research"
	ActionInstallDependency  Action = "install_dependency"
	ActionLogIncident        Action = "log_incident"
	ActionBrowserInteractive Action = "browser_interactive"
	ActionRoutineContract    Action = "routine_contract"
	ActionCredentialUse      Action = "credential_use"
	ActionTouchDefaultBranch Action = "touch_default_branch"
)

// Approval statuses, matching agent.approvals.status in sql/001_init.sql.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
)

// Gate errors. Decide's "already decided" and "expired" outcomes are distinct
// sentinels so callers can tell them apart with errors.Is.
var (
	ErrDenied         = errors.New("approvals: action denied by policy")
	ErrNotApproved    = errors.New("approvals: no approved gate yet (pending) for this task and action")
	ErrAlreadyDecided = errors.New("approvals: already decided")
	ErrExpired        = errors.New("approvals: approval expired")
	ErrCrossChat      = errors.New("approvals: approval belongs to another chat")
	ErrUnauthorized   = errors.New("approvals: unauthorized user")
	ErrInvalidVia     = errors.New("approvals: invalid decision via")
	ErrNotFound       = errors.New("approvals: approval not found")
)

// ErrNotPending is an alias for the duplicate-decision error, for callers that
// prefer the database's wording. ErrPending is the gate's wording.
var (
	ErrNotPending = ErrAlreadyDecided
	ErrPending    = ErrNotApproved
)

// Policy is the subset of config/policies.yaml needed by the gate.
type Policy struct {
	Actions        map[Action]Decision `yaml:"actions"`
	ProtectedPaths []string            `yaml:"protected_paths"`
	SensitiveTags  map[string][]string `yaml:"sensitive_tags"`
	TestPatterns   []string            `yaml:"test_patterns"`
	LockfilePairs  map[string]string   `yaml:"lockfile_pairs"`
}

// Load reads a policies.yaml file with strict field and action validation.
// Unknown action keys and invalid decision values are errors.
func Load(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("approvals: read %s: %w", path, err)
	}
	p := &Policy{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("approvals: parse %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("approvals: parse %s: unexpected second YAML document", path)
		}
		return nil, fmt.Errorf("approvals: parse %s: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("approvals: validate %s: %w", path, err)
	}
	return p, nil
}

func (p *Policy) validate() error {
	if p == nil {
		return nil
	}
	for action, decision := range p.Actions {
		if !knownAction(action) {
			return fmt.Errorf("unknown action key %q", action)
		}
		canonical, ok := parseDecision(string(decision))
		if !ok {
			return fmt.Errorf("action %q: invalid decision %q", action, decision)
		}
		p.Actions[action] = canonical
	}
	return nil
}

func knownAction(a Action) bool {
	switch a {
	case ActionTaskContract, ActionPlanNewApp, ActionEditProtectedPath,
		ActionDeleteDirOrMany, ActionPushBranch, ActionOpenPR,
		ActionStartPreview, ActionNewDomain, ActionWebResearch,
		ActionInstallDependency, ActionLogIncident, ActionBrowserInteractive,
		ActionRoutineContract, ActionCredentialUse, ActionTouchDefaultBranch:
		return true
	default:
		return false
	}
}

func parseDecision(s string) (Decision, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return Auto, true
	case "ask":
		return Ask, true
	case "deny":
		return Deny, true
	case "auto_readonly", "auto-readonly", "autoreadonly":
		return AutoReadonly, true
	default:
		return "", false
	}
}

// Evaluate returns the configured decision. Missing actions default to Ask;
// touching the default branch is always Deny, even with no policy loaded.
func (p *Policy) Evaluate(a Action) Decision {
	if a == ActionTouchDefaultBranch {
		return Deny
	}
	if p == nil || p.Actions == nil {
		return Ask
	}
	d, ok := p.Actions[a]
	if !ok || d == "" {
		return Ask
	}
	return d
}

// RequiresLockfileApproval reports whether a changed lockfile has no paired
// manifest in changed. Defaults from CONTRACT.md §3.1 always apply; entries in
// LockfilePairs add to or override them.
func (p *Policy) RequiresLockfileApproval(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	pairs := p.lockfilePairs()
	present := make(map[string]bool, len(changed)*2)
	for _, f := range changed {
		n := cleanPath(f)
		if n != "" {
			present[n] = true
			present[path.Base(n)] = true
		}
	}
	for _, f := range changed {
		n := cleanPath(f)
		if n == "" {
			continue
		}
		manifest, ok := pairs[n]
		if !ok {
			manifest, ok = pairs[path.Base(n)]
		}
		if !ok || manifest == "" {
			continue
		}
		if present[manifest] || present[path.Base(manifest)] {
			continue
		}
		return true
	}
	return false
}

// lockfilePairs returns a fresh map; there are no mutable package globals.
func (p *Policy) lockfilePairs() map[string]string {
	pairs := map[string]string{
		"go.sum":            "go.mod",
		"package-lock.json": "package.json",
		"pnpm-lock.yaml":    "package.json",
		"yarn.lock":         "package.json",
		"Cargo.lock":        "Cargo.toml",
		"poetry.lock":       "pyproject.toml",
	}
	if p == nil {
		return pairs
	}
	for lock, manifest := range p.LockfilePairs {
		lock = cleanPath(lock)
		manifest = cleanPath(manifest)
		if lock == "" || lock == "." || manifest == "" || manifest == "." {
			continue
		}
		pairs[lock] = manifest
		pairs[path.Base(lock)] = path.Base(manifest)
	}
	return pairs
}

// IsProtected reports whether path matches a protected-path glob. Glob
// segments support '*' (within one path segment), '?' (one character) and
// '**' (zero or more whole path segments).
func (p *Policy) IsProtected(pth string) bool {
	if p == nil || len(p.ProtectedPaths) == 0 {
		return false
	}
	nameSegs := strings.Split(cleanPath(pth), "/")
	for _, pattern := range p.ProtectedPaths {
		patSegs := strings.Split(cleanPath(pattern), "/")
		if matchSegments(patSegs, nameSegs) {
			return true
		}
	}
	return false
}

func matchSegments(pattern, name []string) bool {
	memo := make(map[[2]int]bool)
	var walk func(i, j int) bool
	walk = func(i, j int) bool {
		key := [2]int{i, j}
		if seen, ok := memo[key]; ok {
			return seen
		}
		var result bool
		switch {
		case i == len(pattern):
			result = j == len(name)
		case pattern[i] == "**":
			result = walk(i+1, j) || (j < len(name) && walk(i, j+1))
		case j == len(name):
			result = false
		default:
			result = matchSegment(pattern[i], name[j]) && walk(i+1, j+1)
		}
		memo[key] = result
		return result
	}
	return walk(0, 0)
}

// matchSegment is the classic linear-time wildcard matcher. '**' embedded in a
// segment is just two '*' characters: it does not cross the segment boundary.
func matchSegment(pattern, name string) bool {
	pi, ni := 0, 0
	star, mark := -1, -1
	for ni < len(name) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == name[ni]):
			pi++
			ni++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			mark = ni
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

func cleanPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" {
		return ""
	}
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// Request is an action the caller wants the owner to approve.
type Request struct {
	Action    Action
	Detail    map[string]any
	ChatID    int64
	SessionID uuid.UUID
}

// Approval mirrors agent.approvals plus the chat/session routing columns.
type Approval struct {
	ID         uuid.UUID
	TaskID     uuid.UUID
	Action     Action
	Detail     map[string]any
	Status     string
	ChatID     int64
	SessionID  uuid.UUID
	ExpiresAt  time.Time
	CreatedAt  time.Time
	DecidedAt  *time.Time
	DecidedVia string
}

// Store is the persistence surface the gate needs. Implementations must make
// DecideApproval an atomic compare-and-set on status='pending'; whichever
// client decides first wins.
type Store interface {
	CreateApproval(ctx context.Context, in Approval) (Approval, error)
	GetApproval(ctx context.Context, id uuid.UUID) (Approval, error)
	DecideApproval(ctx context.Context, id uuid.UUID, approve bool, via string) (Approval, error)
	Audit(ctx context.Context, taskID *uuid.UUID, action, detail string) error
	ListExpired(ctx context.Context, now time.Time) ([]Approval, error)
	HasApproved(ctx context.Context, taskID uuid.UUID, a Action) (bool, error)
}

// ExpirationStore is optional. When a Store implements it, ExpireLoop marks
// approvals with status='expired'. Stores without it fall back to
// DecideApproval with via="expired" and must interpret that accordingly.
type ExpirationStore interface {
	ExpireApproval(ctx context.Context, id uuid.UUID, at time.Time) error
}

// Manager coordinates policy, persistence and owner decisions.
type Manager struct {
	Policy  *Policy
	Store   Store
	TTL     time.Duration
	OwnerID int64
	Now     func() time.Time
}

func (m *Manager) now() time.Time {
	if m != nil && m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) ttl() time.Duration {
	if m != nil && m.TTL > 0 {
		return m.TTL
	}
	return 30 * time.Minute
}

// Request creates a pending approval. Denied actions are refused outright;
// Ask actions are created and the caller must wait for Decide.
func (m *Manager) Request(ctx context.Context, taskID uuid.UUID, r Request) (uuid.UUID, error) {
	if m == nil {
		return uuid.Nil, errors.New("approvals: nil manager")
	}
	if decision := m.Policy.Evaluate(r.Action); decision == Deny {
		return uuid.Nil, fmt.Errorf("%w: %s", ErrDenied, r.Action)
	}
	if m.Store == nil {
		return uuid.Nil, errors.New("approvals: nil store")
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, fmt.Errorf("approvals: generate id: %w", err)
	}
	now := m.now()
	a := Approval{
		ID:        id,
		TaskID:    taskID,
		Action:    r.Action,
		Detail:    cloneDetail(r.Detail),
		Status:    StatusPending,
		ChatID:    r.ChatID,
		SessionID: r.SessionID,
		ExpiresAt: now.Add(m.ttl()),
		CreatedAt: now,
	}
	if _, err := m.Store.CreateApproval(ctx, a); err != nil {
		return uuid.Nil, fmt.Errorf("approvals: create: %w", err)
	}
	// Audit lines never include Detail values: they may contain tokens.
	m.audit(ctx, taskID, "approval_requested",
		fmt.Sprintf("approval=%s action=%s task=%s session=%s", id, r.Action, taskID, r.SessionID))
	return id, nil
}

// Decide records an owner decision. via must be "telegram:<chatID>:<userID>";
// a callback from another chat or any user other than OwnerID is rejected.
func (m *Manager) Decide(ctx context.Context, approvalID uuid.UUID, approve bool, via string) error {
	if m == nil {
		return errors.New("approvals: nil manager")
	}
	chatID, userID, err := parseVia(via)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidVia, err)
	}
	if m.Store == nil {
		return errors.New("approvals: nil store")
	}
	a, err := m.Store.GetApproval(ctx, approvalID)
	if err != nil {
		return fmt.Errorf("approvals: get %s: %w", approvalID, err)
	}
	if chatID != a.ChatID {
		return fmt.Errorf("%w: approval chat=%d callback chat=%d", ErrCrossChat, a.ChatID, chatID)
	}
	if userID != m.OwnerID {
		return fmt.Errorf("%w: user=%d", ErrUnauthorized, userID)
	}
	switch a.Status {
	case StatusExpired:
		return fmt.Errorf("%w: %s", ErrExpired, approvalID)
	case StatusApproved, StatusRejected:
		// A decided approval stays decided even if its TTL later passes.
		return fmt.Errorf("%w: %s", ErrAlreadyDecided, approvalID)
	case StatusPending, "":
	default:
		return fmt.Errorf("approvals: unexpected status %q for %s", a.Status, approvalID)
	}
	if !m.now().Before(a.ExpiresAt) {
		return fmt.Errorf("%w: %s", ErrExpired, approvalID)
	}
	if _, err := m.Store.DecideApproval(ctx, approvalID, approve, via); err != nil {
		return decideError(err)
	}
	decision := StatusRejected
	if approve {
		decision = StatusApproved
	}
	m.audit(ctx, a.TaskID, "approval_decided",
		fmt.Sprintf("approval=%s action=%s decision=%s chat=%d user=%d", approvalID, a.Action, decision, chatID, userID))
	return nil
}

// Gate enforces the publish boundary. Auto actions pass, Deny actions fail,
// and Ask actions require a prior approved decision for this task and action.
func (m *Manager) Gate(ctx context.Context, taskID uuid.UUID, a Action) error {
	if m == nil {
		return errors.New("approvals: nil manager")
	}
	switch m.Policy.Evaluate(a) {
	case Deny:
		return fmt.Errorf("%w: %s", ErrDenied, a)
	case Auto, AutoReadonly:
		return nil
	default:
		if m.Store == nil {
			return fmt.Errorf("%w: %s", ErrNotApproved, a)
		}
		ok, err := m.Store.HasApproved(ctx, taskID, a)
		if err != nil {
			return fmt.Errorf("approvals: gate %s: %w", a, err)
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotApproved, a)
		}
		return nil
	}
}

// PendingGate is a descriptive alias for Gate.
func (m *Manager) PendingGate(ctx context.Context, taskID uuid.UUID, a Action) error {
	return m.Gate(ctx, taskID, a)
}

// ShortIDLookup is an optional store capability used by Telegram callbacks.
type ShortIDLookup interface {
	ApprovalByShortID(ctx context.Context, shortID string) (Approval, error)
}

// ResolveShortID maps an eight-character callback prefix to one approval.
func (m *Manager) ResolveShortID(ctx context.Context, shortID string) (Approval, error) {
	if m == nil || m.Store == nil {
		return Approval{}, errors.New("approvals: cannot resolve without a store")
	}
	lookup, ok := m.Store.(ShortIDLookup)
	if !ok {
		return Approval{}, errors.New("approvals: store cannot resolve short ids")
	}
	shortID = strings.ToLower(strings.TrimSpace(shortID))
	if len(shortID) != 8 {
		return Approval{}, fmt.Errorf("%w: short id %q", ErrInvalidVia, shortID)
	}
	if _, err := hex.DecodeString(shortID); err != nil {
		return Approval{}, fmt.Errorf("%w: short id %q", ErrInvalidVia, shortID)
	}
	return lookup.ApprovalByShortID(ctx, shortID)
}

// ExpireLoop scans for expired pending approvals immediately and once a minute
// thereafter until ctx is cancelled. Expired approvals leave their task paused;
// this package never transitions task state.
func (m *Manager) ExpireLoop(ctx context.Context) {
	if m == nil || m.Store == nil {
		<-ctx.Done()
		return
	}
	m.expireOnce(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.expireOnce(ctx)
		}
	}
}

func (m *Manager) expireOnce(ctx context.Context) {
	if ctx.Err() != nil || m == nil || m.Store == nil {
		return
	}
	now := m.now()
	items, err := m.Store.ListExpired(ctx, now)
	if err != nil {
		return
	}
	for _, a := range items {
		if now.Before(a.ExpiresAt) || (a.Status != "" && a.Status != StatusPending) {
			continue
		}
		if exp, ok := m.Store.(ExpirationStore); ok {
			err = exp.ExpireApproval(ctx, a.ID, now)
		} else {
			_, err = m.Store.DecideApproval(ctx, a.ID, false, "expired")
		}
		if err != nil {
			continue
		}
		m.audit(ctx, a.TaskID, "approval_expired",
			fmt.Sprintf("approval=%s action=%s task=%s", a.ID, a.Action, a.TaskID))
	}
}

func (m *Manager) audit(ctx context.Context, taskID uuid.UUID, action, detail string) {
	if m == nil || m.Store == nil {
		return
	}
	id := taskID
	_ = m.Store.Audit(ctx, &id, action, detail)
}

func parseVia(via string) (chatID, userID int64, err error) {
	parts := strings.Split(via, ":")
	if len(parts) != 3 || parts[0] != "telegram" {
		return 0, 0, fmt.Errorf("want telegram:<chatID>:<userID>, got %q", via)
	}
	chatID, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("chat id %q: %w", parts[1], err)
	}
	userID, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("user id %q: %w", parts[2], err)
	}
	return chatID, userID, nil
}

func decideError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrAlreadyDecided) || errors.Is(err, ErrExpired) {
		return err
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "expired"):
		return fmt.Errorf("%w: %v", ErrExpired, err)
	case strings.Contains(msg, "already decided"), strings.Contains(msg, "not pending"),
		strings.Contains(msg, "zero rows"), strings.Contains(msg, "conflict"):
		return fmt.Errorf("%w: %v", ErrAlreadyDecided, err)
	default:
		return fmt.Errorf("approvals: decide: %w", err)
	}
}

// cloneDetail makes a shallow copy so a caller mutating its map cannot change
// a pending approval after creation. Values are copied as-is for the card
// renderer; they are never written to audit lines or Telegram messages.
func cloneDetail(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
