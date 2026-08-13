package model

import "time"

type Binding struct {
	CanonicalID string    `json:"canonical_id"`
	NewAPIID    int       `json:"newapi_id"`
	Email       string    `json:"email"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type PendingBind struct {
	CanonicalID string    `json:"canonical_id"`
	NewAPIID    int       `json:"newapi_id"`
	Email       string    `json:"email"`
	CodeMAC     string    `json:"code_mac"`
	ExpiresAt   time.Time `json:"expires_at"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
}

type LinkChallenge struct {
	CodeMAC     string    `json:"code_mac"`
	CanonicalID string    `json:"canonical_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type CheckinRecord struct {
	CanonicalID    string    `json:"canonical_id"`
	NewAPIID       int       `json:"newapi_id"`
	PeriodKey      string    `json:"period_key"`
	RedemptionName string    `json:"redemption_name"`
	EncryptedCode  string    `json:"encrypted_code"`
	RawQuota       int64     `json:"raw_quota"`
	DisplayCredit  string    `json:"display_credit"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Status         string    `json:"status"`
	LastError      string    `json:"last_error,omitempty"`
}

type AuditRecord struct {
	At          time.Time      `json:"at"`
	Actor       string         `json:"actor"`
	Action      string         `json:"action"`
	Target      string         `json:"target,omitempty"`
	Success     bool           `json:"success"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type QuotaNotification struct {
	CanonicalID  string    `json:"canonical_id"`
	NewAPIID     int       `json:"newapi_id"`
	GroupOpenID  string    `json:"group_openid"`
	MemberOpenID string    `json:"member_openid"`
	Threshold    string    `json:"threshold"`
	Enabled      bool      `json:"enabled"`
	Alerted      bool      `json:"alerted"`
	LastAlertAt  time.Time `json:"last_alert_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	DailyEnabled bool      `json:"daily_enabled"`
	LastDailyKey string    `json:"last_daily_key"`
}

type GroupWelcome struct {
	GroupOpenID string    `json:"group_openid"`
	Enabled     bool      `json:"enabled"`
	Message     string    `json:"message"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GroupJoinApproval struct {
	GroupOpenID string    `json:"group_openid"`
	Enabled     bool      `json:"enabled"`
	MinQQLevel  int       `json:"min_qq_level"`
	MatchText   string    `json:"match_text"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type PendingAdminAction struct {
	Code        string    `json:"code"`
	Actor       string    `json:"actor"`
	Action      string    `json:"action"`
	TargetID    int       `json:"target_id"`
	TargetLabel string    `json:"target_label"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type SentBotMessage struct {
	GroupOpenID string    `json:"group_openid"`
	MessageID   string    `json:"message_id"`
	MessageIdx  string    `json:"message_idx"`
	SentAt      time.Time `json:"sent_at"`
}

type PendingGatewayEvent struct {
	Key       string    `json:"-"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

type BenefitCampaign struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Actor          string        `json:"actor"`
	GroupOpenID    string        `json:"group_openid"`
	DisplayCredit  string        `json:"display_credit"`
	RawQuota       int64         `json:"raw_quota"`
	Count          int           `json:"count"`
	ValidHours     int           `json:"valid_hours"`
	BanDays        int           `json:"ban_days"`
	RedemptionIDs  []int         `json:"redemption_ids"`
	EncryptedCodes []string      `json:"encrypted_codes"`
	ClaimedCodes   map[int][]int `json:"claimed_codes,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	ExpiresAt      time.Time     `json:"expires_at"`
	LastCheckedAt  time.Time     `json:"last_checked_at,omitempty"`
	Status         string        `json:"status"`
	Announced      bool          `json:"announced"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type BenefitBan struct {
	Key         string    `json:"key"`
	CampaignID  string    `json:"campaign_id"`
	GroupOpenID string    `json:"group_openid"`
	UserID      int       `json:"user_id"`
	RedeemCount int       `json:"redeem_count"`
	DisabledAt  time.Time `json:"disabled_at"`
	EnableAt    time.Time `json:"enable_at"`
	Status      string    `json:"status"`
	LastError   string    `json:"last_error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CommandRule struct {
	Keyword   string    `json:"keyword"`
	Enabled   bool      `json:"enabled"`
	Actor     string    `json:"actor"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ResetStage string

const (
	ResetStageUnknown   ResetStage = "unknown"
	ResetStagePossible  ResetStage = "possible"
	ResetStageImminent  ResetStage = "imminent"
	ResetStageConfirmed ResetStage = "confirmed"
)

type ResetActivityStatus string

const (
	ResetActivityActive    ResetActivityStatus = "active"
	ResetActivitySettling  ResetActivityStatus = "settling"
	ResetActivityCompleted ResetActivityStatus = "completed"
	ResetActivityStopped   ResetActivityStatus = "stopped"
)

type ResetAwardStatus string

const (
	ResetAwardPending             ResetAwardStatus = "pending"
	ResetAwardGranting            ResetAwardStatus = "granting"
	ResetAwardGranted             ResetAwardStatus = "granted"
	ResetAwardZero                ResetAwardStatus = "zero"
	ResetAwardFailed              ResetAwardStatus = "failed"
	ResetAwardPendingConfirmation ResetAwardStatus = "pending_confirmation"
)

type ResetNotificationKind string

const (
	ResetNotificationSignal            ResetNotificationKind = "signal"
	ResetNotificationActivityStarted   ResetNotificationKind = "activity_started"
	ResetNotificationActivityCompleted ResetNotificationKind = "activity_completed"
)

type ResetNotificationStatus string

const (
	ResetNotificationPending    ResetNotificationStatus = "pending"
	ResetNotificationSent       ResetNotificationStatus = "sent"
	ResetNotificationSuperseded ResetNotificationStatus = "superseded"
)

const (
	DefaultResetDuration    = 5 * time.Hour
	DefaultResetWinnerCount = 5
	DefaultResetLookback    = 24 * time.Hour
)

type ResetSettings struct {
	GroupOpenID string        `json:"group_openid"`
	Duration    time.Duration `json:"duration"`
	WinnerCount int           `json:"winner_count"`
	Lookback    time.Duration `json:"lookback"`
	Subscribed  bool          `json:"subscribed"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

type ResetSignal struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	ExternalID string     `json:"external_id,omitempty"`
	Stage      ResetStage `json:"stage"`
	Status     string     `json:"status,omitempty"`
	Title      string     `json:"title,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	URL        string     `json:"url,omitempty"`
	OccurredAt time.Time  `json:"occurred_at"`
	DetectedAt time.Time  `json:"detected_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type ResetGroupState struct {
	GroupOpenID     string     `json:"group_openid"`
	Stage           ResetStage `json:"stage"`
	SignalID        string     `json:"signal_id,omitempty"`
	ActivityID      string     `json:"activity_id,omitempty"`
	Summary         string     `json:"summary,omitempty"`
	Source          string     `json:"source,omitempty"`
	SourceURL       string     `json:"source_url,omitempty"`
	LastCompletedAt time.Time  `json:"last_completed_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type ResetSignalDelivery struct {
	GroupOpenID string     `json:"group_openid"`
	SignalID    string     `json:"signal_id"`
	Stage       ResetStage `json:"stage"`
	Status      string     `json:"status,omitempty"`
	ActivityID  string     `json:"activity_id,omitempty"`
	ProcessedAt time.Time  `json:"processed_at"`
}

type ResetParticipant struct {
	ActivityID   string    `json:"activity_id"`
	GroupOpenID  string    `json:"group_openid"`
	NewAPIID     int       `json:"newapi_id"`
	CanonicalID  string    `json:"canonical_id"`
	MemberOpenID string    `json:"member_openid,omitempty"`
	JoinedAt     time.Time `json:"joined_at"`
}

type ResetAward struct {
	NewAPIID     int              `json:"newapi_id"`
	CanonicalID  string           `json:"canonical_id"`
	MemberOpenID string           `json:"member_openid,omitempty"`
	RawQuota     int64            `json:"raw_quota"`
	Status       ResetAwardStatus `json:"status"`
	LastError    string           `json:"last_error,omitempty"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

type ResetActivity struct {
	ID               string              `json:"id"`
	GroupOpenID      string              `json:"group_openid"`
	SignalID         string              `json:"signal_id"`
	Status           ResetActivityStatus `json:"status"`
	StartedAt        time.Time           `json:"started_at"`
	EndsAt           time.Time           `json:"ends_at"`
	ClosedAt         time.Time           `json:"closed_at,omitempty"`
	WinnerCount      int                 `json:"winner_count"`
	Lookback         time.Duration       `json:"lookback"`
	ParticipantCount int                 `json:"participant_count"`
	Awards           []ResetAward        `json:"awards,omitempty"`
	SelectedAt       time.Time           `json:"selected_at,omitempty"`
	CompletedAt      time.Time           `json:"completed_at,omitempty"`
	StoppedAt        time.Time           `json:"stopped_at,omitempty"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

type ResetNotification struct {
	ID            string                  `json:"id"`
	Kind          ResetNotificationKind   `json:"kind"`
	Status        ResetNotificationStatus `json:"status"`
	GroupOpenID   string                  `json:"group_openid"`
	SignalID      string                  `json:"signal_id,omitempty"`
	SignalStage   ResetStage              `json:"signal_stage,omitempty"`
	SignalStatus  string                  `json:"signal_status,omitempty"`
	SignalSource  string                  `json:"signal_source,omitempty"`
	SignalSummary string                  `json:"signal_summary,omitempty"`
	SignalURL     string                  `json:"signal_url,omitempty"`
	ActivityID    string                  `json:"activity_id,omitempty"`
	Chunks        []string                `json:"chunks,omitempty"`
	NextChunk     int                     `json:"next_chunk,omitempty"`
	Attempts      int                     `json:"attempts"`
	NextAttemptAt time.Time               `json:"next_attempt_at"`
	LastError     string                  `json:"last_error,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	SentAt        time.Time               `json:"sent_at,omitempty"`
}

type QQIdentity struct {
	UnionOpenID  string
	UserOpenID   string
	MemberOpenID string
	GroupOpenID  string
}

func (i QQIdentity) Canonical() string {
	if i.UnionOpenID != "" {
		return "union:" + i.UnionOpenID
	}
	if i.UserOpenID != "" {
		return "user:" + i.UserOpenID
	}
	return ""
}

func (i QQIdentity) GroupAlias() string {
	if i.GroupOpenID != "" && i.MemberOpenID != "" {
		return "member:" + i.GroupOpenID + ":" + i.MemberOpenID
	}
	return ""
}

func (i QQIdentity) AdminCandidates() []string {
	result := make([]string, 0, 3)
	if i.UnionOpenID != "" {
		result = append(result, "union:"+i.UnionOpenID)
	}
	if i.UserOpenID != "" {
		result = append(result, "user:"+i.UserOpenID)
	}
	if alias := i.GroupAlias(); alias != "" {
		result = append(result, alias)
	}
	return result
}
