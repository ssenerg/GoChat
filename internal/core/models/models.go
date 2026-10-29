// Package models holds the stored entities.
package models

import (
	"time"

	"github.com/gocql/gocql"
)

type SystemRole string

const (
	SystemRoleUser  SystemRole = "system_user"
	SystemRoleAdmin SystemRole = "system_admin"
)

type TeamRole string

const (
	TeamRoleMember TeamRole = "team_member"
	TeamRoleAdmin  TeamRole = "team_admin"
)

type ChannelRole string

const (
	ChannelRoleMember ChannelRole = "channel_member"
	ChannelRoleAdmin  ChannelRole = "channel_admin"
)

type ChannelKind string

const (
	ChannelPublic  ChannelKind = "public"
	ChannelPrivate ChannelKind = "private"
	ChannelDirect  ChannelKind = "direct"
	ChannelGroup   ChannelKind = "group"
	// ChannelVoice is a room to talk in. Everyone in its team may join the
	// talk; it carries no messages.
	ChannelVoice ChannelKind = "voice"
)

// Conversation reports whether channels of k belong to no team and have a
// fixed set of people: direct and group conversations.
func (k ChannelKind) Conversation() bool {
	return k == ChannelDirect || k == ChannelGroup
}

type User struct {
	ID           gocql.UUID `db:"id"`
	Username     string     `db:"username"`
	Email        string     `db:"email"`
	DisplayName  string     `db:"display_name"`
	PasswordHash string     `db:"password_hash"`
	Role         SystemRole `db:"system_role"`
	TOTPEnabled  bool       `db:"totp_enabled"`
	TOTPSecret   []byte     `db:"totp_secret"`
	TOTPPending  []byte     `db:"totp_pending"`
	TOTPLastStep int64      `db:"totp_last_step"`
	Deactivated  bool       `db:"deactivated"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`
	// Photo is the version of the profile photo, empty when there is none.
	Photo string `db:"photo"`
	// Status is the availability the user chose, empty for online.
	Status      string    `db:"status"`
	DNDUntil    time.Time `db:"dnd_until"`
	StatusEmoji string    `db:"status_emoji"`
	StatusText  string    `db:"status_text"`
	StatusUntil time.Time `db:"status_until"`
	// Bot marks a bot, which BotOwner owns.
	Bot      bool       `db:"bot"`
	BotOwner gocql.UUID `db:"bot_owner"`
}

type Session struct {
	TokenHash []byte     `db:"token_hash"`
	UserID    gocql.UUID `db:"user_id"`
	CreatedAt time.Time  `db:"created_at"`
	ExpiresAt time.Time  `db:"expires_at"`
	IP        string     `db:"ip"`
	UserAgent string     `db:"user_agent"`
	// LastSeen is when it was last used, recorded at most every few minutes,
	// from LastIP.
	LastSeen time.Time `db:"last_seen"`
	LastIP   string    `db:"last_ip"`
}

type MFAChallenge struct {
	TokenHash []byte     `db:"token_hash"`
	UserID    gocql.UUID `db:"user_id"`
	Attempts  int        `db:"attempts"`
	ExpiresAt time.Time  `db:"expires_at"`
	IP        string     `db:"ip"`
	UserAgent string     `db:"user_agent"`
}

// SSOFlow is a sign-in with an identity provider that has yet to come back.
type SSOFlow struct {
	StateHash []byte `db:"state_hash"`
	Provider  string `db:"provider"`
	Nonce     string `db:"nonce"`
	// Verifier is the PKCE verifier, sealed.
	Verifier    []byte `db:"verifier"`
	RedirectURI string `db:"redirect_uri"`
	// LinkUser is who links the identity, when someone signed in started it.
	LinkUser gocql.UUID `db:"link_user"`
	// AppChallenge is the PKCE challenge of an app the sign-in hands off to.
	AppChallenge string    `db:"app_challenge"`
	ExpiresAt    time.Time `db:"expires_at"`
}

// The kinds of SSO tickets.
const (
	// SSOTicketApp lets an app redeem a sign-in that finished in a browser.
	SSOTicketApp = "app"
	// SSOTicketLink waits for an account's owner to link the identity.
	SSOTicketLink = "link"
)

// SSOTicket is what a finished sign-in leaves for the step after it.
type SSOTicket struct {
	TokenHash []byte     `db:"token_hash"`
	Kind      string     `db:"kind"`
	UserID    gocql.UUID `db:"user_id"`
	Challenge string     `db:"challenge"`
	Provider  string     `db:"provider"`
	Issuer    string     `db:"issuer"`
	Subject   string     `db:"subject"`
	Email     string     `db:"email"`
	ExpiresAt time.Time  `db:"expires_at"`
}

// SSOIdentity links an account at an identity provider, known by its issuer
// and subject, to a user.
type SSOIdentity struct {
	Issuer   string     `db:"issuer"`
	Subject  string     `db:"subject"`
	UserID   gocql.UUID `db:"user_id"`
	Provider string     `db:"provider"`
	Email    string     `db:"email"`
	LinkedAt time.Time  `db:"linked_at"`
}

type Team struct {
	ID          gocql.UUID `db:"id"`
	Name        string     `db:"name"`
	DisplayName string     `db:"display_name"`
	Description string     `db:"description"`
	CreatedBy   gocql.UUID `db:"created_by"`
	CreatedAt   time.Time  `db:"created_at"`
	Photo       string     `db:"photo"`
}

type TeamMember struct {
	TeamID   gocql.UUID `db:"team_id"`
	UserID   gocql.UUID `db:"user_id"`
	Role     TeamRole   `db:"role"`
	JoinedAt time.Time  `db:"joined_at"`
}

type Channel struct {
	ID            gocql.UUID  `db:"id"`
	TeamID        gocql.UUID  `db:"team_id"`
	Name          string      `db:"name"`
	DisplayName   string      `db:"display_name"`
	Purpose       string      `db:"purpose"`
	Kind          ChannelKind `db:"kind"`
	CreatedBy     gocql.UUID  `db:"created_by"`
	CreatedAt     time.Time   `db:"created_at"`
	LastMessageID gocql.UUID  `db:"last_message_id"`
	// ArchivedAt is set while the channel is archived: read only.
	ArchivedAt time.Time `db:"archived_at"`
	// VoiceDisabled stops members posting voice messages; channels allow
	// them unless set.
	VoiceDisabled bool `db:"voice_disabled"`
}

type ChannelMember struct {
	ChannelID gocql.UUID  `db:"channel_id"`
	UserID    gocql.UUID  `db:"user_id"`
	TeamID    gocql.UUID  `db:"team_id"`
	Role      ChannelRole `db:"role"`
	JoinedAt  time.Time   `db:"joined_at"`
	// Bot marks a bot, and BotSeesAll one that gets every message of the
	// channel, as it was when added.
	Bot        bool `db:"bot"`
	BotSeesAll bool `db:"bot_sees_all"`
}

type Message struct {
	ChannelID   gocql.UUID   `db:"channel_id"`
	Bucket      int32        `db:"bucket"`
	ID          gocql.UUID   `db:"id"`
	UserID      gocql.UUID   `db:"user_id"`
	Content     string       `db:"content"`
	Attachments []gocql.UUID `db:"attachments"`
	EditedAt    time.Time    `db:"edited_at"`
	// RootID is set on a reply. A root counts its replies and knows who
	// took part in its thread.
	RootID       gocql.UUID   `db:"root_id"`
	ReplyCount   int          `db:"reply_count"`
	LastReplyAt  time.Time    `db:"last_reply_at"`
	Participants []gocql.UUID `db:"participants"`
	Pinned       bool         `db:"pinned"`
	// Markup holds the buttons a bot put under the message, a pb.Markup.
	Markup []byte `db:"markup"`
	// LinkPreview is the card of its first link, a pb.LinkPreview;
	// NoLinkPreview keeps it from having one.
	LinkPreview   []byte `db:"link_preview"`
	NoLinkPreview bool   `db:"no_link_preview"`
}

type Attachment struct {
	ID          gocql.UUID `db:"id"`
	ChannelID   gocql.UUID `db:"channel_id"`
	UploaderID  gocql.UUID `db:"uploader_id"`
	Name        string     `db:"name"`
	ContentType string     `db:"content_type"`
	Size        int64      `db:"size"`
	ObjectKey   string     `db:"object_key"`
	MessageID   gocql.UUID `db:"message_id"`
	CreatedAt   time.Time  `db:"created_at"`
	// Counted is set when Size is part of the uploader's storage usage.
	Counted bool `db:"counted"`
	// Voice marks a voice message, DurationMS its length as recorded.
	Voice      bool `db:"voice"`
	DurationMS int  `db:"duration_ms"`
}

type Invite struct {
	TokenHash []byte     `db:"token_hash"`
	TeamID    gocql.UUID `db:"team_id"`
	CreatedBy gocql.UUID `db:"created_by"`
	CreatedAt time.Time  `db:"created_at"`
	ExpiresAt time.Time  `db:"expires_at"`
	UsedBy    gocql.UUID `db:"used_by"`
	UsedAt    time.Time  `db:"used_at"`
}

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID      gocql.UUID
	Role        SystemRole
	SessionHash []byte
	SessionID   string
	ExpiresAt   time.Time
	// Bot is set for a bot signed in with its token.
	Bot bool
}

// Bot is what a bot has beyond its user.
type Bot struct {
	ID          gocql.UUID `db:"id"`
	OwnerID     gocql.UUID `db:"owner_id"`
	Description string     `db:"description"`
	SeesAll     bool       `db:"sees_all"`
	// Commands is the bot's commands, a pb.GetCommandsResponse.
	Commands  []byte    `db:"commands"`
	TokenHash []byte    `db:"token_hash"`
	CreatedAt time.Time `db:"created_at"`
	// The webhook, and how its deliveries go.
	WebhookURL     string    `db:"webhook_url"`
	WebhookSecret  string    `db:"webhook_secret"`
	DeliveredPts   int64     `db:"delivered_pts"`
	WebhookError   string    `db:"webhook_error"`
	WebhookErrorAt time.Time `db:"webhook_error_at"`
}

// The kinds of an update log's entries: a message's post or edit is kept
// as a reference to it, anything else as its event.
const (
	UpdateEvent  = 0
	UpdatePosted = 1
	UpdateEdited = 2
	UpdatePress  = 3
)

// The push providers a device's token belongs to.
const (
	PushFCM  = "fcm"
	PushAPNs = "apns"
)

// PushDevice is the device of a session, which push notifications reach
// through Platform's provider while the app is closed.
type PushDevice struct {
	UserID      gocql.UUID `db:"user_id"`
	SessionHash []byte     `db:"session_hash"`
	Platform    string     `db:"platform"`
	Token       string     `db:"device_token"`
	// Sandbox marks an APNs token of Apple's sandbox.
	Sandbox   bool      `db:"sandbox"`
	CreatedAt time.Time `db:"created_at"`
}

// Block is a person someone blocked.
type Block struct {
	UserID    gocql.UUID `db:"user_id"`
	BlockedID gocql.UUID `db:"blocked_id"`
	CreatedAt time.Time  `db:"created_at"`
}

// Report is a message or a person reported to a team's admins: UserID is
// the person reported, or the message's author. Action stays empty while
// the report is open. A report of a conversation of no team has a zero
// TeamID, for system admins alone.
type Report struct {
	ID         gocql.UUID `db:"id"`
	TeamID     gocql.UUID `db:"team_id"`
	ReporterID gocql.UUID `db:"reporter_id"`
	UserID     gocql.UUID `db:"user_id"`
	ChannelID  gocql.UUID `db:"channel_id"`
	MessageID  gocql.UUID `db:"message_id"`
	RootID     gocql.UUID `db:"root_id"`
	// Excerpt is the message as it read when reported.
	Excerpt    string     `db:"excerpt"`
	Reason     string     `db:"reason"`
	Details    string     `db:"details"`
	Action     string     `db:"action"`
	ResolvedBy gocql.UUID `db:"resolved_by"`
	ResolvedAt time.Time  `db:"resolved_at"`
}

// The reasons a report gives, and what an admin did about one.
const (
	ReportSpam       = "spam"
	ReportHarassment = "harassment"
	ReportHate       = "hate"
	ReportSexual     = "sexual"
	ReportViolence   = "violence"
	ReportOther      = "other"

	ReportDismissed     = "dismissed"
	ReportDeleteMessage = "message_deleted"
	ReportRemoveMember  = "member_removed"
	ReportDeactivate    = "deactivated"
)
