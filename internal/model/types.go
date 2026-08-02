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
	Status         string    `json:"status"`
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
