package directory

import (
	"time"

	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/models"
)

type CreateTeam struct {
	P                              *models.Principal
	Name, DisplayName, Description string
}

type ListTeams struct{ P *models.Principal }

// GetTeam finds a team by ID or, when set, Name.
type GetTeam struct {
	P        *models.Principal
	ID, Name string
}

// ListTeamMembers replies with a *pb.ListTeamMembersResponse.
type ListTeamMembers struct {
	P      *models.Principal
	TeamID string
	Query  string
	Limit  int
	After  string
}

type RemoveTeamMember struct {
	P              *models.Principal
	TeamID, UserID string
}

type UpdateTeamMemberRole struct {
	P              *models.Principal
	TeamID, UserID string
	Role           models.TeamRole
}

type CreateInvite struct {
	P      *models.Principal
	TeamID string
}

type Invite struct {
	Token     string
	ExpiresAt time.Time
}

// ListInvites replies with a *pb.ListInvitesResponse.
type ListInvites struct {
	P      *models.Principal
	TeamID string
	After  string
}

type RevokeInvite struct {
	P                *models.Principal
	TeamID, InviteID string
}

type GetInvite struct{ Token string }

type InviteInfo struct {
	Valid           bool
	TeamDisplayName string
}

type AcceptInvite struct {
	P     *models.Principal
	Token string
}

// JoinTeam admits a user whose invite was already consumed.
type JoinTeam struct {
	UserID, TeamID gocql.UUID
	// InviteHash names the invite UserID consumed to join, spent once the
	// join completes.
	InviteHash []byte
}

type ListChannels struct {
	P      *models.Principal
	TeamID string
}

// BrowseChannels replies with *Browsed.
type BrowseChannels struct {
	P      *models.Principal
	TeamID string
}

// Browsed holds the public channels the caller may join, and the archived
// ones they belong to.
type Browsed struct {
	Open, Archived []*pb.Channel
}

type CreateChannel struct {
	P                                  *models.Principal
	TeamID, Name, DisplayName, Purpose string
	Kind                               models.ChannelKind
	VoiceDisabled                      bool
}

// OpenDirect opens the conversation of the caller and UserID in TeamID.
type OpenDirect struct {
	P              *models.Principal
	TeamID, UserID string
}

// OpenGroup opens the conversation of the caller and UserIDs in TeamID.
type OpenGroup struct {
	P       *models.Principal
	TeamID  string
	UserIDs []string
}

// ListThreads replies with *ThreadsResult. A zero Limit counts the unread
// threads only.
type ListThreads struct {
	P             *models.Principal
	TeamID        string
	Offset, Limit int
}

type ThreadsResult struct {
	Threads []*pb.ThreadSummary
	Unread  int
	// Next is the offset of the next page, zero at the end.
	Next int
}

// ListSaved replies with *SavedResult; with IDsOnly, the ids of all the
// caller saved, in any team.
type ListSaved struct {
	P       *models.Principal
	TeamID  string
	IDsOnly bool
}

type SavedResult struct {
	Hits []*pb.SearchHit
	IDs  []string
}

type GetUser struct {
	P      *models.Principal
	UserID string
}

type UpdateProfile struct {
	P           *models.Principal
	DisplayName string
}

// SetPhoto replaces the caller's photo with a processed one, or removes it
// when Image is nil.
type SetPhoto struct {
	P     *models.Principal
	Image []byte
}

type SetTeamPhoto struct {
	P      *models.Principal
	TeamID string
	Image  []byte
}

// GetPhoto names a user or a team, and the photo version the caller saw.
type GetPhoto struct {
	P       *models.Principal
	UserID  string
	TeamID  string
	Version string
}

type Photo struct {
	Data        []byte
	ContentType string
	Version     string
}

type Me struct{ P *models.Principal }

type MeResult struct {
	User        *pb.User
	Email       string
	HasPassword bool
}

// Presence replies with the statuses of the users among UserIDs who are not
// offline.
type Presence struct {
	P       *models.Principal
	UserIDs []string
}

// Acquainted replies with those of UserIDs that UserID shares a team with,
// UserID included: whose presence they may see.
type Acquainted struct {
	UserID  string
	UserIDs []string
}

// SetStatus and SetCustomStatus reply with the caller as they now are.
type (
	SetStatus struct {
		P          *models.Principal
		Status     pb.Status
		DNDMinutes int
	}
	SetCustomStatus struct {
		P           *models.Principal
		Emoji, Text string
		Minutes     int
	}
)

// ListMuted replies with the ids of the channels the caller muted.
type ListMuted struct{ P *models.Principal }

type Done struct{}
