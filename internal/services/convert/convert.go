// Package convert renders stored entities as the API's protobuf messages.
package convert

import (
	"strings"
	"time"

	"github.com/gocql/gocql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
)

func Time(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func ID(u gocql.UUID) string {
	if ids.IsZero(u) {
		return ""
	}
	return u.String()
}

func SystemRole(r models.SystemRole) pb.SystemRole {
	if r == models.SystemRoleAdmin {
		return pb.SystemRole_SYSTEM_ROLE_ADMIN
	}
	return pb.SystemRole_SYSTEM_ROLE_USER
}

func TeamRole(r models.TeamRole) pb.TeamRole {
	switch r {
	case models.TeamRoleAdmin:
		return pb.TeamRole_TEAM_ROLE_ADMIN
	case models.TeamRoleMember:
		return pb.TeamRole_TEAM_ROLE_MEMBER
	default:
		return pb.TeamRole_TEAM_ROLE_UNSPECIFIED
	}
}

func TeamRoleFrom(r pb.TeamRole) (models.TeamRole, bool) {
	switch r {
	case pb.TeamRole_TEAM_ROLE_ADMIN:
		return models.TeamRoleAdmin, true
	case pb.TeamRole_TEAM_ROLE_MEMBER:
		return models.TeamRoleMember, true
	default:
		return "", false
	}
}

func ChannelRole(r models.ChannelRole) pb.ChannelRole {
	switch r {
	case models.ChannelRoleAdmin:
		return pb.ChannelRole_CHANNEL_ROLE_ADMIN
	case models.ChannelRoleMember:
		return pb.ChannelRole_CHANNEL_ROLE_MEMBER
	default:
		return pb.ChannelRole_CHANNEL_ROLE_UNSPECIFIED
	}
}

func ChannelType(k models.ChannelKind) pb.ChannelType {
	switch k {
	case models.ChannelPublic:
		return pb.ChannelType_CHANNEL_TYPE_PUBLIC
	case models.ChannelPrivate:
		return pb.ChannelType_CHANNEL_TYPE_PRIVATE
	case models.ChannelDirect:
		return pb.ChannelType_CHANNEL_TYPE_DIRECT
	case models.ChannelGroup:
		return pb.ChannelType_CHANNEL_TYPE_GROUP
	case models.ChannelVoice:
		return pb.ChannelType_CHANNEL_TYPE_VOICE
	default:
		return pb.ChannelType_CHANNEL_TYPE_UNSPECIFIED
	}
}

func User(u *models.User) *pb.User {
	if u == nil {
		return nil
	}
	return &pb.User{
		Id:           u.ID.String(),
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		Role:         SystemRole(u.Role),
		TotpEnabled:  u.TOTPEnabled,
		CreatedAt:    Time(u.CreatedAt),
		Deactivated:  u.Deactivated,
		Photo:        u.Photo,
		Status:       Status(u.Status),
		DndUntil:     Time(u.DNDUntil),
		CustomStatus: Custom(u, time.Now()),
		Bot:          u.Bot,
		BotOwnerId:   ID(u.BotOwner),
	}
}

// Session describes s, the session of the request when current.
func Session(s *models.Session, current bool) *pb.SessionInfo {
	return &pb.SessionInfo{
		Id:         secure.PublicID(s.TokenHash),
		UserAgent:  s.UserAgent,
		Ip:         s.IP,
		LastIp:     s.LastIP,
		CreatedAt:  Time(s.CreatedAt),
		LastSeenAt: Time(s.LastSeen),
		ExpiresAt:  Time(s.ExpiresAt),
		Current:    current,
	}
}

// PublicUser is User without what only its owner should see.
func PublicUser(u *models.User) *pb.User {
	if u == nil {
		return nil
	}
	return &pb.User{
		Id:           u.ID.String(),
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		Role:         SystemRole(u.Role),
		Deactivated:  u.Deactivated,
		Photo:        u.Photo,
		CustomStatus: Custom(u, time.Now()),
		Bot:          u.Bot,
		BotOwnerId:   ID(u.BotOwner),
	}
}

// Status is the availability a user chose.
func Status(s string) pb.Status {
	switch s {
	case "away":
		return pb.Status_STATUS_AWAY
	case "dnd":
		return pb.Status_STATUS_DND
	case "offline":
		return pb.Status_STATUS_OFFLINE
	}
	return pb.Status_STATUS_ONLINE
}

// Custom is the custom status of u, nil when it has none or it ran out.
func Custom(u *models.User, now time.Time) *pb.CustomStatus {
	if u.StatusEmoji == "" || (!u.StatusUntil.IsZero() && !now.Before(u.StatusUntil)) {
		return nil
	}
	return &pb.CustomStatus{Emoji: u.StatusEmoji, Text: u.StatusText, ExpiresAt: Time(u.StatusUntil)}
}

func Team(t *models.Team, role models.TeamRole) *pb.Team {
	return &pb.Team{
		Id:          t.ID.String(),
		Name:        t.Name,
		DisplayName: t.DisplayName,
		Description: t.Description,
		Role:        TeamRole(role),
		CreatedAt:   Time(t.CreatedAt),
		Photo:       t.Photo,
		OwnerId:     t.CreatedBy.String(),
	}
}

func Channel(ch *models.Channel, role models.ChannelRole, lastRead gocql.UUID, peer *pb.User) *pb.Channel {
	return &pb.Channel{
		Id:            ch.ID.String(),
		TeamId:        ID(ch.TeamID),
		Name:          ch.Name,
		DisplayName:   ch.DisplayName,
		Purpose:       ch.Purpose,
		Type:          ChannelType(ch.Kind),
		Role:          ChannelRole(role),
		LastMessageId: ID(ch.LastMessageID),
		LastReadId:    ID(lastRead),
		Peer:          peer,
		CreatedAt:     Time(ch.CreatedAt),
		ArchivedAt:    Time(ch.ArchivedAt),
		VoiceEnabled:  !ch.VoiceDisabled,
	}
}

func Attachment(a *models.Attachment) *pb.Attachment {
	return &pb.Attachment{
		Id: a.ID.String(), Name: a.Name, ContentType: a.ContentType, Size: a.Size,
		// Durations are bounded at upload; uint32 cannot overflow here.
		Voice: a.Voice, DurationMs: uint32(max(a.DurationMS, 0)), //nolint:gosec
	}
}

func Message(m *models.Message, author *pb.User, atts []*models.Attachment) *pb.Message {
	out := &pb.Message{
		Id:        m.ID.String(),
		ChannelId: m.ChannelID.String(),
		Author:    author,
		Content:   m.Content,
		CreatedAt: Time(m.ID.Time()),
		EditedAt:  Time(m.EditedAt),
		RootId:    ID(m.RootID),
		// Counts are small; int32 cannot overflow here.
		ReplyCount:  int32(m.ReplyCount), //nolint:gosec
		LastReplyAt: Time(m.LastReplyAt),
		Pinned:      m.Pinned,
	}
	for _, a := range atts {
		out.Attachments = append(out.Attachments, Attachment(a))
	}
	if len(m.Markup) > 0 {
		var markup pb.Markup
		if proto.Unmarshal(m.Markup, &markup) == nil {
			out.Markup = &markup
		}
	}
	return out
}

// Conversation renders ch as self sees it, naming the others of a direct
// or group conversation from people, which holds them.
func Conversation(ch *models.Channel, role models.ChannelRole, lastRead, self gocql.UUID, people map[gocql.UUID]*models.User) *pb.Channel {
	out := Channel(ch, role, lastRead, nil)
	for _, id := range PeerIDs(ch, self) {
		if u := PublicUser(people[id]); u != nil {
			switch ch.Kind {
			case models.ChannelDirect:
				out.Peer = u
			case models.ChannelGroup:
				out.Peers = append(out.Peers, u)
			}
		}
	}
	return out
}

// PeerIDs returns the other people of a conversation, whose name lists its
// members' ids.
func PeerIDs(ch *models.Channel, self gocql.UUID) []gocql.UUID {
	var out []gocql.UUID
	for _, raw := range strings.Split(ch.Name, ":") {
		if id, ok := ids.Parse(raw); ok && id != self {
			out = append(out, id)
		}
	}
	return out
}
