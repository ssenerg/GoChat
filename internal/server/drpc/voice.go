package drpc

import (
	"context"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strconv"

	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/voice"
)

type voiceService struct{ *base }

func (s *voiceService) access(ctx context.Context, rawChannel string) (*channels.VoiceAccessResult, channels.Target, error) {
	t, err := channelTarget(ctx, rawChannel)
	if err != nil {
		return nil, t, err
	}
	acc, err := actors.Ask[*channels.VoiceAccessResult](ctx, s.sys.Engine, s.sys.Channels, channels.VoiceAccess{Target: t})
	return acc, t, err
}

func (s *voiceService) JoinVoice(ctx context.Context, req *pb.JoinVoiceRequest) (*pb.JoinVoiceResponse, error) {
	acc, t, err := s.access(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if !acc.CanConnect {
		return nil, ierrors.PermissionDenied()
	}
	u, err := s.sys.Users.Get(ctx, t.P.UserID)
	if err != nil {
		return nil, err
	}
	ticket, err := actors.Ask[*voice.Ticket](ctx, s.sys.Engine, s.sys.Voice, voice.Join{P: t.P, User: convert.PublicUser(u), Channel: acc.Channel})
	if err != nil {
		return nil, err
	}
	return &pb.JoinVoiceResponse{Endpoint: ticket.Endpoint, Token: ticket.Token, Channel: convert.Channel(acc.Channel, "", gocql.UUID{}, nil)}, nil
}

func (s *voiceService) LeaveVoice(ctx context.Context, _ *pb.LeaveVoiceRequest) (*pb.LeaveVoiceResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[voice.Done](ctx, s.sys.Engine, s.sys.Voice, voice.Leave{UserID: p.UserID}); err != nil {
		return nil, err
	}
	return &pb.LeaveVoiceResponse{}, nil
}

func (s *voiceService) UpdateVoiceState(ctx context.Context, req *pb.UpdateVoiceStateRequest) (*pb.UpdateVoiceStateResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[voice.Done](ctx, s.sys.Engine, s.sys.Voice, voice.Update{UserID: p.UserID, Mute: req.SelfMute, Deaf: req.SelfDeaf}); err != nil {
		return nil, err
	}
	return &pb.UpdateVoiceStateResponse{}, nil
}

func (s *voiceService) DisconnectVoiceMember(ctx context.Context, req *pb.DisconnectVoiceMemberRequest) (*pb.DisconnectVoiceMemberResponse, error) {
	acc, t, err := s.access(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if !acc.CanModerate {
		return nil, ierrors.PermissionDenied()
	}
	target, ok := ids.Parse(req.UserId)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid user id")
	}
	if _, err := actors.Ask[voice.Done](ctx, s.sys.Engine, s.sys.Voice, voice.Kick{ChannelID: t.ChannelID, UserID: target}); err != nil {
		return nil, err
	}
	return &pb.DisconnectVoiceMemberResponse{}, nil
}

func (s *voiceService) ListVoiceStates(ctx context.Context, req *pb.ListVoiceStatesRequest) (*pb.ListVoiceStatesResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	teamID, ok := ids.Parse(req.TeamId)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid team id")
	}
	list, err := actors.Ask[[]*pb.VoiceChannelState](ctx, s.sys.Engine, s.sys.Voice, voice.List{P: p, TeamID: teamID})
	if err != nil {
		return nil, err
	}
	return &pb.ListVoiceStatesResponse{Channels: list}, nil
}

var voiceServerIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type voiceServerService struct{ *base }

// Connect serves one voice server: it claims the tokens of the people who
// connect to it and hears who left, and the voice actor streams back whom
// to disconnect.
func (s *voiceServerService) Connect(stream pb.DRPCVoiceServerService_ConnectStream) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || !voiceServerIDRe.MatchString(hello.ServerId) || !validEndpoint(hello.Endpoint) {
		return ierrors.InvalidArgument("the first frame must be a valid hello")
	}
	key := hello.ServerId + "#" + strconv.FormatUint(streamSeq.Add(1), 36)
	defer s.sys.Engine.Send(s.sys.Voice, voice.ServerDown{Key: key})
	if _, err := actors.Ask[voice.Registered](ctx, s.sys.Engine, s.sys.Voice, voice.ServerUp{Key: key, ID: hello.ServerId, Endpoint: hello.Endpoint, Stream: stream}); err != nil {
		return err
	}
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch f := req.Frame.(type) {
		case *pb.VoiceServerServiceConnectRequest_Claim:
			s.sys.Engine.Send(s.sys.Voice, voice.Claim{Key: key, RequestID: f.Claim.RequestId, Token: f.Claim.Token})
		case *pb.VoiceServerServiceConnectRequest_PeerLeft:
			l := f.PeerLeft
			s.sys.Engine.Send(s.sys.Voice, voice.PeerLeft{Key: key, UserID: l.UserId, ChannelID: l.ChannelId, Session: l.Session})
		case *pb.VoiceServerServiceConnectRequest_PeerMedia:
			m := f.PeerMedia
			s.sys.Engine.Send(s.sys.Voice, voice.PeerMedia{Key: key, UserID: m.UserId, ChannelID: m.ChannelId, Session: m.Session, Video: m.Video, Stream: m.Stream})
		}
	}
}

// validEndpoint accepts the websocket URL browsers are sent to.
func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 512 && (u.Scheme == "ws" || u.Scheme == "wss") && u.Host != "" && u.User == nil
}
