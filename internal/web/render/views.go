package render

import (
	"fmt"
	"html/template"
	"slices"
	"strings"
	"time"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/emoji"
	"github.com/ssenerg/gochat/internal/core/mention"
)

type Message struct {
	*pb.Message
	Body      template.HTML
	Time      time.Time
	Edited    bool
	Mine      bool
	CanDelete bool
	// Mentioned is set when the message calls out its reader, Focus when it
	// is the one a link opened.
	Mentioned bool
	Focus     bool
	OOB       bool
	// Saved is whether the reader saved the message, Blocked whether they
	// blocked its author: it shows folded.
	Saved   bool
	Blocked bool
	bar     ReactionsView
}

// Message prepares m for the viewer meID; moderators may delete any message.
func (r *Renderer) Message(m *pb.Message, me *pb.User, moderator bool) *Message {
	mine := m.GetAuthor().GetId() == me.GetId()
	return &Message{
		Message:   m,
		Body:      r.Body(m),
		Time:      m.GetCreatedAt().AsTime(),
		Edited:    m.EditedAt != nil,
		Mine:      mine,
		CanDelete: mine || moderator,
		Mentioned: !mine && Mentions(m, me),
		Saved:     m.GetSaved(),
		bar:       Reactions(m.GetChannelId(), m.GetId(), m.GetRootId(), m.GetReactions(), me, false),
	}
}

func (r *Renderer) Messages(list []*pb.Message, me *pb.User, moderator bool) []*Message {
	out := make([]*Message, len(list))
	for i, m := range list {
		out[i] = r.Message(m, me, moderator)
	}
	return out
}

// Fold folds the messages whose author the reader blocked.
func Fold(list []*Message, blocked map[string]bool) {
	for _, m := range list {
		m.Blocked = !m.Mine && blocked[m.GetAuthor().GetId()]
	}
}

// Mentions reports whether m calls out me, by name or as one of everyone.
func Mentions(m *pb.Message, me *pb.User) bool {
	found := mention.Parse(m.GetContent())
	return found.Channel || found.Here || found.Of(me.GetUsername())
}

type Rail struct {
	Teams []*pb.Team
	Team  *pb.Team
	OOB   bool
}

type Lists struct {
	Team     *pb.Team
	Channels []*pb.Channel
	Voice    []*pb.Channel
	DMs      []*pb.Channel
	// VoiceStates holds who talks in each voice channel, by channel id.
	VoiceStates map[string]*pb.VoiceChannelState
	// Active is the open channel's id, or "threads".
	Active string
	// Status holds the peers who are not offline.
	Status map[string]pb.Status
	// ThreadsUnread counts the followed threads with unread replies.
	ThreadsUnread int
	// Key hands the sidebar to the page's socket.
	Key string
	OOB bool
}

// Conversation reports whether ch is a direct or group conversation.
func Conversation(ch *pb.Channel) bool {
	return ch != nil && (ch.Type == pb.ChannelType_CHANNEL_TYPE_DIRECT || ch.Type == pb.ChannelType_CHANNEL_TYPE_GROUP)
}

// Split separates a team's text channels, its voice channels and the
// conversations.
func Split(all []*pb.Channel) (channels, voice, direct []*pb.Channel) {
	for _, ch := range all {
		switch {
		case Conversation(ch):
			direct = append(direct, ch)
		case ch.Type == pb.ChannelType_CHANNEL_TYPE_VOICE:
			voice = append(voice, ch)
		default:
			channels = append(channels, ch)
		}
	}
	return channels, voice, direct
}

// VoiceStates indexes a team's voice states by channel.
func VoiceStates(list []*pb.VoiceChannelState) map[string]*pb.VoiceChannelState {
	out := make(map[string]*pb.VoiceChannelState, len(list))
	for _, s := range list {
		out[s.ChannelId] = s
	}
	return out
}

// VoiceRoom is who talks in one voice channel, for the sidebar or the
// channel's own page. State is nil when nobody does.
type VoiceRoom struct {
	TeamName  string
	ChannelID string
	State     *pb.VoiceChannelState
	// Moderator may disconnect the others; MeID is the viewer.
	Moderator bool
	MeID      string
	OOB       bool
}

// Peers returns the other sides of direct channels.
func Peers(direct []*pb.Channel) []string {
	out := make([]string, 0, len(direct))
	for _, ch := range direct {
		if ch.Peer != nil {
			out = append(out, ch.Peer.Id)
		}
	}
	return out
}

type ChannelView struct {
	Team       *pb.Team
	Channel    *pb.Channel
	Title      string
	Member     bool
	Messages   []*Message
	HasMore    bool
	Oldest     string
	Newest     string
	Moderator  bool
	PeerStatus pb.Status
	// OpenThread is the root of a thread to open with the channel.
	OpenThread string
	// Anchored is set when newer messages than those shown exist: the view
	// shows an older part of the channel and takes no new messages.
	Anchored bool
	Archived bool
	// Voice is who talks in a voice channel, MeID the viewer.
	Voice *pb.VoiceChannelState
	MeID  string
	// PeerBlocked is set when the viewer blocked the other of a direct
	// conversation: it reads, but takes no messages.
	PeerBlocked bool
	OOB         bool
}

// Manage reports whether the viewer may change the channel's settings.
func (v *ChannelView) Manage() bool {
	return v.Moderator && v.Member && !Conversation(v.Channel)
}

func Title(ch *pb.Channel) string {
	if ch.Peer != nil {
		return DisplayName(ch.Peer)
	}
	if len(ch.Peers) > 0 {
		names := make([]string, len(ch.Peers))
		for i, p := range ch.Peers {
			names[i] = DisplayName(p)
		}
		return strings.Join(names, ", ")
	}
	return ch.DisplayName
}

type Typing struct {
	ChannelID string
	Name      string
}

type Presence struct {
	ID     string
	Status pb.Status
}

type Toast struct {
	Kind string
	Text string
}

// ReactionView is one emoji on a message as a reader sees it.
type ReactionView struct {
	Name  string
	Char  string
	Count int
	Mine  bool
	Who   string
}

// ReactionsView is a message's reaction bar.
type ReactionsView struct {
	ID   string
	URL  string
	List []ReactionView
	OOB  bool
}

// ReactURL is where a message's reactions are added and taken back.
func ReactURL(channelID, messageID, rootID string) string {
	u := "/m/" + channelID + "/" + messageID + "/react"
	if rootID != "" {
		u += "?root=" + rootID
	}
	return u
}

// Reactions prepares a message's reactions for me.
func Reactions(channelID, messageID, rootID string, list []*pb.Reaction, me *pb.User, oob bool) ReactionsView {
	out := ReactionsView{ID: messageID, URL: ReactURL(channelID, messageID, rootID), OOB: oob}
	for _, r := range list {
		char, ok := emoji.Char(r.Emoji)
		if !ok || len(r.UserIds) == 0 {
			continue
		}
		v := ReactionView{Name: r.Emoji, Char: char, Count: len(r.UserIds), Mine: slices.Contains(r.UserIds, me.GetId())}
		switch {
		case v.Mine && v.Count == 1:
			v.Who = "You reacted with :" + r.Emoji + ":"
		case v.Mine:
			v.Who = fmt.Sprintf("You and %d more reacted with :%s:", v.Count-1, r.Emoji)
		default:
			v.Who = fmt.Sprintf("%d reacted with :%s:", v.Count, r.Emoji)
		}
		out.List = append(out.List, v)
	}
	return out
}

// Bar is the message's reaction bar.
func (m *Message) Bar() ReactionsView {
	return m.bar
}

// key tells the reaction bars of two readers apart, for caching.
func (v ReactionsView) key() string {
	var b strings.Builder
	for _, r := range v.List {
		fmt.Fprintf(&b, "%s:%d:%t,", r.Name, r.Count, r.Mine)
	}
	return b.String()
}

// Notice is a desktop notification a tab shows for a message.
type Notice struct {
	Title, Body, URL, Tag string
}

const excerptRunes = 140

// Excerpt is a message's text, short and on one line, for a notification.
func Excerpt(m *pb.Message) string {
	text := strings.Join(strings.Fields(m.GetContent()), " ")
	if r := []rune(text); len(r) > excerptRunes {
		text = string(r[:excerptRunes]) + "…"
	}
	if text == "" {
		if n := len(m.GetAttachments()); n == 1 && m.Attachments[0].GetVoice() {
			text = "Sent a voice message"
		} else if n == 1 {
			text = "Sent a file"
		} else if n > 1 {
			text = fmt.Sprintf("Sent %d files", n)
		}
	}
	return text
}

// ThreadCard is a followed thread as the threads view lists it.
type ThreadCard struct {
	Root         *pb.Message
	Channel      *pb.Channel
	Where        string
	Excerpt      template.HTML
	Unread       bool
	Participants []*pb.User
}

func ThreadCards(list []*pb.ThreadSummary) []ThreadCard {
	out := make([]ThreadCard, 0, len(list))
	for _, t := range list {
		if t.GetRoot() == nil || t.GetChannel() == nil {
			continue
		}
		out = append(out, ThreadCard{
			Root: t.Root, Channel: t.Channel, Where: Title(t.Channel), Excerpt: Highlight(t.Root.Content, nil),
			Unread: t.Unread, Participants: t.Participants,
		})
	}
	return out
}
