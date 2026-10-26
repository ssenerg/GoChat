package drpc

import (
	"context"
	"sync"

	"github.com/gocql/gocql"

	"storj.io/drpc/drpcmux"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/minio"
	"github.com/ssenerg/gochat/internal/services"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/files"
	"github.com/ssenerg/gochat/internal/services/index"
)

type base struct {
	sys     *services.System
	storage *minio.Storage
	cfg     *services.Config
	uploads chan struct{}
	// uploading counts each user's uploads in progress.
	mu        sync.Mutex
	uploading map[gocql.UUID]int
}

// Mux registers every service on a new mux.
func Mux(sys *services.System, storage *minio.Storage, cfg *services.Config) (*drpcmux.Mux, error) {
	b := &base{sys: sys, storage: storage, cfg: cfg, uploads: make(chan struct{}, cfg.Files.MaxUploads), uploading: make(map[gocql.UUID]int)}
	mux := drpcmux.New()
	for _, reg := range []func() error{
		func() error { return pb.DRPCRegisterAuthService(mux, &authService{b}) },
		func() error { return pb.DRPCRegisterTeamService(mux, &teamService{b}) },
		func() error { return pb.DRPCRegisterChannelService(mux, &channelService{b}) },
		func() error { return pb.DRPCRegisterMessageService(mux, &messageService{b}) },
		func() error { return pb.DRPCRegisterFileService(mux, &fileService{b}) },
		func() error { return pb.DRPCRegisterUserService(mux, &userService{b}) },
		func() error { return pb.DRPCRegisterUpdatesService(mux, &updatesService{b}) },
		func() error { return pb.DRPCRegisterBotService(mux, &botService{b}) },
		func() error { return pb.DRPCRegisterGatewayService(mux, &gatewayService{b}) },
		func() error { return pb.DRPCRegisterAdminService(mux, &adminService{b}) },
		func() error { return pb.DRPCRegisterVoiceService(mux, &voiceService{b}) },
		func() error { return pb.DRPCRegisterVoiceServerService(mux, &voiceServerService{b}) },
	} {
		if err := reg(); err != nil {
			return nil, err
		}
	}
	return mux, nil
}

// Resolve authenticates a session token, from the cache when it can.
func Resolve(sys *services.System) Resolver {
	return func(ctx context.Context, token string) (*models.Principal, error) {
		if p, ok := sys.Sessions.Cached(token); ok {
			return p, nil
		}
		return actors.Ask[*models.Principal](ctx, sys.Engine, sys.Auth, accounts.Authenticate{Token: token, IP: ClientOf(ctx).IP})
	}
}

func channelTarget(ctx context.Context, raw string) (channels.Target, error) {
	p, err := Principal(ctx)
	if err != nil {
		return channels.Target{}, err
	}
	id, ok := ids.Parse(raw)
	if !ok {
		return channels.Target{}, ierrors.InvalidArgument("invalid channel id")
	}
	return channels.Target{P: p, ChannelID: id}, nil
}

type authService struct{ *base }

func (s *authService) GetSignupInfo(ctx context.Context, req *pb.GetSignupInfoRequest) (*pb.GetSignupInfoResponse, error) {
	info, err := actors.Ask[*accounts.SignupInfoResult](ctx, s.sys.Engine, s.sys.Accounts, accounts.SignupInfo{InviteToken: req.InviteToken})
	if err != nil {
		return nil, err
	}
	return &pb.GetSignupInfoResponse{
		Bootstrap: info.Bootstrap, SetupTokenRequired: info.SetupTokenRequired, SetupClosed: info.SetupClosed,
		Open: info.Open, InviteValid: info.InviteValid, TeamDisplayName: info.TeamDisplayName,
	}, nil
}

func (s *authService) SignUp(ctx context.Context, req *pb.SignUpRequest) (*pb.SignUpResponse, error) {
	cl := ClientOf(ctx)
	res, err := actors.Ask[*accounts.SignUpResult](ctx, s.sys.Engine, s.sys.Accounts, accounts.SignUp{
		Username: req.Username, Email: req.Email, DisplayName: req.DisplayName, Password: req.Password,
		InviteToken: req.InviteToken, SetupToken: req.SetupToken, IP: cl.IP, UserAgent: cl.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	out := &pb.SignUpResponse{Session: res.Session}
	// The account exists either way; a join that failed is finished by
	// opening the invite again.
	if !ids.IsZero(res.InviteTeamID) {
		if team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.JoinTeam{UserID: res.User.ID, TeamID: res.InviteTeamID, InviteHash: res.InviteHash}); err == nil {
			out.Team = team
		}
	}
	return out, nil
}

func (s *authService) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	cl := ClientOf(ctx)
	res, err := actors.Ask[*accounts.LoginResult](ctx, s.sys.Engine, s.sys.Accounts, accounts.Login{
		Login: req.Login, Password: req.Password, IP: cl.IP, UserAgent: cl.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	if res.MFAToken != "" {
		return &pb.LoginResponse{Result: &pb.LoginResponse_MfaToken{MfaToken: res.MFAToken}}, nil
	}
	return &pb.LoginResponse{Result: &pb.LoginResponse_Session{Session: res.Session}}, nil
}

func (s *authService) VerifyMFA(ctx context.Context, req *pb.VerifyMFARequest) (*pb.VerifyMFAResponse, error) {
	cl := ClientOf(ctx)
	res, err := actors.Ask[*accounts.LoginResult](ctx, s.sys.Engine, s.sys.Accounts, accounts.VerifyMFA{
		Token: req.MfaToken, Code: req.Code, IP: cl.IP, UserAgent: cl.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	return &pb.VerifyMFAResponse{Session: res.Session}, nil
}

func (s *authService) Logout(ctx context.Context, _ *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[accounts.Done](ctx, s.sys.Engine, s.sys.Accounts, accounts.Logout{P: p}); err != nil {
		return nil, err
	}
	return &pb.LogoutResponse{}, nil
}

func (s *authService) Me(ctx context.Context, _ *pb.MeRequest) (*pb.MeResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	me, err := actors.Ask[*directory.MeResult](ctx, s.sys.Engine, s.sys.Directory, directory.Me{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.MeResponse{User: me.User, Email: me.Email, SessionId: p.SessionID}, nil
}

func (s *authService) ChangePassword(ctx context.Context, req *pb.ChangePasswordRequest) (*pb.ChangePasswordResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[accounts.Done](ctx, s.sys.Engine, s.sys.Accounts, accounts.ChangePassword{
		P: p, Current: req.CurrentPassword, NewPassword: req.NewPassword,
	}); err != nil {
		return nil, err
	}
	return &pb.ChangePasswordResponse{}, nil
}

func (s *authService) BeginTOTPSetup(ctx context.Context, req *pb.BeginTOTPSetupRequest) (*pb.BeginTOTPSetupResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	setup, err := actors.Ask[*accounts.TOTPSetup](ctx, s.sys.Engine, s.sys.Accounts, accounts.BeginTOTP{P: p, Password: req.Password})
	if err != nil {
		return nil, err
	}
	return &pb.BeginTOTPSetupResponse{Secret: setup.Secret, Url: setup.URL}, nil
}

func (s *authService) ConfirmTOTPSetup(ctx context.Context, req *pb.ConfirmTOTPSetupRequest) (*pb.ConfirmTOTPSetupResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := actors.Ask[accounts.RecoveryCodes](ctx, s.sys.Engine, s.sys.Accounts, accounts.ConfirmTOTP{P: p, Code: req.Code})
	if err != nil {
		return nil, err
	}
	return &pb.ConfirmTOTPSetupResponse{RecoveryCodes: codes}, nil
}

func (s *authService) DisableTOTP(ctx context.Context, req *pb.DisableTOTPRequest) (*pb.DisableTOTPResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[accounts.Done](ctx, s.sys.Engine, s.sys.Accounts, accounts.DisableTOTP{P: p, Password: req.Password, Code: req.Code}); err != nil {
		return nil, err
	}
	return &pb.DisableTOTPResponse{}, nil
}

func (s *authService) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.SessionInfo](ctx, s.sys.Engine, s.sys.Accounts, accounts.ListSessions{P: p, UserID: req.UserId})
	if err != nil {
		return nil, err
	}
	return &pb.ListSessionsResponse{Sessions: list}, nil
}

func (s *authService) RevokeSession(ctx context.Context, req *pb.RevokeSessionRequest) (*pb.RevokeSessionResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[accounts.Done](ctx, s.sys.Engine, s.sys.Accounts, accounts.RevokeSession{P: p, SessionID: req.SessionId}); err != nil {
		return nil, err
	}
	return &pb.RevokeSessionResponse{}, nil
}

func (s *authService) RevokeSessions(ctx context.Context, req *pb.RevokeSessionsRequest) (*pb.RevokeSessionsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	n, err := actors.Ask[accounts.Revoked](ctx, s.sys.Engine, s.sys.Accounts, accounts.RevokeSessions{P: p, UserID: req.UserId})
	if err != nil {
		return nil, err
	}
	return &pb.RevokeSessionsResponse{Revoked: int32(n)}, nil //nolint:gosec // bounded by the sessions one user holds
}

type adminService struct{ *base }

func (s *adminService) FindUser(ctx context.Context, req *pb.FindUserRequest) (*pb.FindUserResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*models.User](ctx, s.sys.Engine, s.sys.Accounts, accounts.FindUser{P: p, Login: req.GetLogin(), UserID: req.GetUserId()})
	if err != nil {
		return nil, err
	}
	return &pb.FindUserResponse{User: convert.User(u), Email: u.Email}, nil
}

func (s *adminService) SetUserDeactivated(ctx context.Context, req *pb.SetUserDeactivatedRequest) (*pb.SetUserDeactivatedResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*models.User](ctx, s.sys.Engine, s.sys.Accounts, accounts.SetDeactivated{P: p, UserID: req.UserId, Deactivated: req.Deactivated})
	if err != nil {
		return nil, err
	}
	return &pb.SetUserDeactivatedResponse{User: convert.User(u)}, nil
}

func (s *adminService) SetStorageQuota(ctx context.Context, req *pb.SetStorageQuotaRequest) (*pb.SetStorageQuotaResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[files.Done](ctx, s.sys.Engine, s.sys.Files, files.SetQuota{P: p, UserID: req.UserId, Quota: req.Quota}); err != nil {
		return nil, err
	}
	return &pb.SetStorageQuotaResponse{}, nil
}

type teamService struct{ *base }

func (s *teamService) CreateTeam(ctx context.Context, req *pb.CreateTeamRequest) (*pb.CreateTeamResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.CreateTeam{
		P: p, Name: req.Name, DisplayName: req.DisplayName, Description: req.Description,
	})
	if err != nil {
		return nil, err
	}
	return &pb.CreateTeamResponse{Team: team}, nil
}

func (s *teamService) ListTeams(ctx context.Context, _ *pb.ListTeamsRequest) (*pb.ListTeamsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	teams, err := actors.Ask[[]*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.ListTeams{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.ListTeamsResponse{Teams: teams}, nil
}

func (s *teamService) GetTeam(ctx context.Context, req *pb.GetTeamRequest) (*pb.GetTeamResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.GetTeam{P: p, ID: req.GetId(), Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &pb.GetTeamResponse{Team: team}, nil
}

func (s *teamService) ListTeamMembers(ctx context.Context, req *pb.ListTeamMembersRequest) (*pb.ListTeamMembersResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	return actors.Ask[*pb.ListTeamMembersResponse](ctx, s.sys.Engine, s.sys.Directory, directory.ListTeamMembers{
		P: p, TeamID: req.TeamId, Query: req.Query, Limit: int(req.Limit), After: req.After,
	})
}

func (s *teamService) RemoveTeamMember(ctx context.Context, req *pb.RemoveTeamMemberRequest) (*pb.RemoveTeamMemberResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[directory.Done](ctx, s.sys.Engine, s.sys.Directory, directory.RemoveTeamMember{P: p, TeamID: req.TeamId, UserID: req.UserId}); err != nil {
		return nil, err
	}
	return &pb.RemoveTeamMemberResponse{}, nil
}

func (s *teamService) UpdateTeamMemberRole(ctx context.Context, req *pb.UpdateTeamMemberRoleRequest) (*pb.UpdateTeamMemberRoleResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	role, ok := convert.TeamRoleFrom(req.Role)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid role")
	}
	if _, err := actors.Ask[directory.Done](ctx, s.sys.Engine, s.sys.Directory, directory.UpdateTeamMemberRole{
		P: p, TeamID: req.TeamId, UserID: req.UserId, Role: role,
	}); err != nil {
		return nil, err
	}
	return &pb.UpdateTeamMemberRoleResponse{}, nil
}

func (s *teamService) CreateInvite(ctx context.Context, req *pb.CreateInviteRequest) (*pb.CreateInviteResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	inv, err := actors.Ask[*directory.Invite](ctx, s.sys.Engine, s.sys.Directory, directory.CreateInvite{P: p, TeamID: req.TeamId})
	if err != nil {
		return nil, err
	}
	return &pb.CreateInviteResponse{Token: inv.Token, ExpiresAt: convert.Time(inv.ExpiresAt)}, nil
}

func (s *teamService) GetInvite(ctx context.Context, req *pb.GetInviteRequest) (*pb.GetInviteResponse, error) {
	info, err := actors.Ask[*directory.InviteInfo](ctx, s.sys.Engine, s.sys.Directory, directory.GetInvite{Token: req.Token})
	if err != nil {
		return nil, err
	}
	return &pb.GetInviteResponse{Valid: info.Valid, TeamDisplayName: info.TeamDisplayName}, nil
}

func (s *teamService) AcceptInvite(ctx context.Context, req *pb.AcceptInviteRequest) (*pb.AcceptInviteResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.AcceptInvite{P: p, Token: req.Token})
	if err != nil {
		return nil, err
	}
	return &pb.AcceptInviteResponse{Team: team}, nil
}

func (s *teamService) ListInvites(ctx context.Context, req *pb.ListInvitesRequest) (*pb.ListInvitesResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	return actors.Ask[*pb.ListInvitesResponse](ctx, s.sys.Engine, s.sys.Directory, directory.ListInvites{P: p, TeamID: req.TeamId, After: req.After})
}

func (s *teamService) RevokeInvite(ctx context.Context, req *pb.RevokeInviteRequest) (*pb.RevokeInviteResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[directory.Done](ctx, s.sys.Engine, s.sys.Directory, directory.RevokeInvite{P: p, TeamID: req.TeamId, InviteID: req.InviteId}); err != nil {
		return nil, err
	}
	return &pb.RevokeInviteResponse{}, nil
}

func (s *teamService) SetTeamPhoto(ctx context.Context, req *pb.SetTeamPhotoRequest) (*pb.SetTeamPhotoResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.Image) == 0 {
		return nil, ierrors.InvalidArgument("the photo is empty")
	}
	team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.SetTeamPhoto{P: p, TeamID: req.TeamId, Image: req.Image})
	if err != nil {
		return nil, err
	}
	return &pb.SetTeamPhotoResponse{Team: team}, nil
}

func (s *teamService) RemoveTeamPhoto(ctx context.Context, req *pb.RemoveTeamPhotoRequest) (*pb.RemoveTeamPhotoResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.SetTeamPhoto{P: p, TeamID: req.TeamId})
	if err != nil {
		return nil, err
	}
	return &pb.RemoveTeamPhotoResponse{Team: team}, nil
}

type channelService struct{ *base }

func (s *channelService) ListChannels(ctx context.Context, req *pb.ListChannelsRequest) (*pb.ListChannelsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.Channel](ctx, s.sys.Engine, s.sys.Directory, directory.ListChannels{P: p, TeamID: req.TeamId})
	if err != nil {
		return nil, err
	}
	return &pb.ListChannelsResponse{Channels: list}, nil
}

func (s *channelService) BrowseChannels(ctx context.Context, req *pb.BrowseChannelsRequest) (*pb.BrowseChannelsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*directory.Browsed](ctx, s.sys.Engine, s.sys.Directory, directory.BrowseChannels{P: p, TeamID: req.TeamId})
	if err != nil {
		return nil, err
	}
	return &pb.BrowseChannelsResponse{Channels: res.Open, Archived: res.Archived}, nil
}

func (s *channelService) CreateChannel(ctx context.Context, req *pb.CreateChannelRequest) (*pb.CreateChannelResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	var kind models.ChannelKind
	switch req.Type {
	case pb.ChannelType_CHANNEL_TYPE_PUBLIC:
		kind = models.ChannelPublic
	case pb.ChannelType_CHANNEL_TYPE_PRIVATE:
		kind = models.ChannelPrivate
	case pb.ChannelType_CHANNEL_TYPE_VOICE:
		kind = models.ChannelVoice
	default:
		return nil, ierrors.InvalidArgument("channel type must be public, private or voice")
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Directory, directory.CreateChannel{
		P: p, TeamID: req.TeamId, Name: req.Name, DisplayName: req.DisplayName, Purpose: req.Purpose, Kind: kind,
		VoiceDisabled: req.VoiceDisabled,
	})
	if err != nil {
		return nil, err
	}
	return &pb.CreateChannelResponse{Channel: ch}, nil
}

func (s *channelService) GetChannel(ctx context.Context, req *pb.GetChannelRequest) (*pb.GetChannelResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Channels, channels.View{Target: t})
	if err != nil {
		return nil, err
	}
	return &pb.GetChannelResponse{Channel: ch}, nil
}

func (s *channelService) JoinChannel(ctx context.Context, req *pb.JoinChannelRequest) (*pb.JoinChannelResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Channels, channels.Join{Target: t})
	if err != nil {
		return nil, err
	}
	return &pb.JoinChannelResponse{Channel: ch}, nil
}

func (s *channelService) LeaveChannel(ctx context.Context, req *pb.LeaveChannelRequest) (*pb.LeaveChannelResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.Leave{Target: t}); err != nil {
		return nil, err
	}
	return &pb.LeaveChannelResponse{}, nil
}

func (s *channelService) AddChannelMember(ctx context.Context, req *pb.AddChannelMemberRequest) (*pb.AddChannelMemberResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.AddMember{Target: t, UserID: req.UserId}); err != nil {
		return nil, err
	}
	return &pb.AddChannelMemberResponse{}, nil
}

func (s *channelService) RemoveChannelMember(ctx context.Context, req *pb.RemoveChannelMemberRequest) (*pb.RemoveChannelMemberResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	userID, ok := ids.Parse(req.UserId)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid user id")
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.RemoveMember{Target: t, UserID: userID}); err != nil {
		return nil, err
	}
	return &pb.RemoveChannelMemberResponse{}, nil
}

func (s *channelService) ListChannelMembers(ctx context.Context, req *pb.ListChannelMembersRequest) (*pb.ListChannelMembersResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	members, err := actors.Ask[[]*pb.ChannelMember](ctx, s.sys.Engine, s.sys.Channels, channels.Members{Target: t})
	if err != nil {
		return nil, err
	}
	return &pb.ListChannelMembersResponse{Members: members}, nil
}

func (s *channelService) OpenDirectChannel(ctx context.Context, req *pb.OpenDirectChannelRequest) (*pb.OpenDirectChannelResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Directory, directory.OpenDirect{P: p, TeamID: req.TeamId, UserID: req.UserId})
	if err != nil {
		return nil, err
	}
	return &pb.OpenDirectChannelResponse{Channel: ch}, nil
}

func (s *channelService) UpdateChannel(ctx context.Context, req *pb.UpdateChannelRequest) (*pb.UpdateChannelResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Channels, channels.Settings{Target: t, DisplayName: req.DisplayName, Name: req.Name, Purpose: req.Purpose})
	if err != nil {
		return nil, err
	}
	return &pb.UpdateChannelResponse{Channel: ch}, nil
}

func (s *channelService) SetChannelPrivacy(ctx context.Context, req *pb.SetChannelPrivacyRequest) (*pb.SetChannelPrivacyResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Channels, channels.Privacy{Target: t, Private: req.Private})
	if err != nil {
		return nil, err
	}
	return &pb.SetChannelPrivacyResponse{Channel: ch}, nil
}

func (s *channelService) ArchiveChannel(ctx context.Context, req *pb.ArchiveChannelRequest) (*pb.ArchiveChannelResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Channels, channels.Archive{Target: t, Archived: req.Archived})
	if err != nil {
		return nil, err
	}
	return &pb.ArchiveChannelResponse{Channel: ch}, nil
}

func (s *channelService) DeleteConversation(ctx context.Context, req *pb.DeleteConversationRequest) (*pb.DeleteConversationResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.DeleteConversation{Target: t, Everyone: req.Everyone}); err != nil {
		return nil, err
	}
	return &pb.DeleteConversationResponse{}, nil
}

func (s *channelService) OpenGroupChannel(ctx context.Context, req *pb.OpenGroupChannelRequest) (*pb.OpenGroupChannelResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.UserIds) > 16 {
		return nil, ierrors.InvalidArgument("too many people")
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Directory, directory.OpenGroup{P: p, TeamID: req.TeamId, UserIDs: req.UserIds})
	if err != nil {
		return nil, err
	}
	return &pb.OpenGroupChannelResponse{Channel: ch}, nil
}

func (s *channelService) MarkChannelRead(ctx context.Context, req *pb.MarkChannelReadRequest) (*pb.MarkChannelReadResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.MarkRead{Target: t, MessageID: req.MessageId}); err != nil {
		return nil, err
	}
	return &pb.MarkChannelReadResponse{}, nil
}

func (s *channelService) SetChannelMuted(ctx context.Context, req *pb.SetChannelMutedRequest) (*pb.SetChannelMutedResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.Mute{Target: t, Muted: req.Muted}); err != nil {
		return nil, err
	}
	return &pb.SetChannelMutedResponse{}, nil
}

func (s *channelService) SetChannelVoice(ctx context.Context, req *pb.SetChannelVoiceRequest) (*pb.SetChannelVoiceResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	ch, err := actors.Ask[*pb.Channel](ctx, s.sys.Engine, s.sys.Channels, channels.SetVoice{Target: t, Enabled: req.Enabled})
	if err != nil {
		return nil, err
	}
	return &pb.SetChannelVoiceResponse{Channel: ch}, nil
}

func (s *channelService) ListMutedChannels(ctx context.Context, _ *pb.ListMutedChannelsRequest) (*pb.ListMutedChannelsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := actors.Ask[[]string](ctx, s.sys.Engine, s.sys.Directory, directory.ListMuted{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.ListMutedChannelsResponse{ChannelIds: ids}, nil
}

type messageService struct{ *base }

func (s *messageService) PostMessage(ctx context.Context, req *pb.PostMessageRequest) (*pb.PostMessageResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	m, err := actors.Ask[*pb.Message](ctx, s.sys.Engine, s.sys.Channels, channels.Post{
		Target: t, Content: req.Content, AttachmentIDs: req.AttachmentIds, RootID: req.RootId, Markup: req.Markup,
	})
	if err != nil {
		return nil, err
	}
	return &pb.PostMessageResponse{Message: m}, nil
}

func (s *messageService) EditMessage(ctx context.Context, req *pb.EditMessageRequest) (*pb.EditMessageResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	m, err := actors.Ask[*pb.Message](ctx, s.sys.Engine, s.sys.Channels, channels.Edit{
		Target: t, MessageID: req.MessageId, RootID: req.RootId, Content: req.Content, Markup: req.Markup,
	})
	if err != nil {
		return nil, err
	}
	return &pb.EditMessageResponse{Message: m}, nil
}

func (s *messageService) DeleteMessage(ctx context.Context, req *pb.DeleteMessageRequest) (*pb.DeleteMessageResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.Delete{Target: t, MessageID: req.MessageId, RootID: req.RootId}); err != nil {
		return nil, err
	}
	return &pb.DeleteMessageResponse{}, nil
}

func (s *messageService) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*channels.HistoryResult](ctx, s.sys.Engine, s.sys.Channels, channels.History{Target: t, BeforeID: req.BeforeId, AfterID: req.AfterId, Limit: int(req.Limit)})
	if err != nil {
		return nil, err
	}
	return &pb.ListMessagesResponse{Messages: res.Messages, HasMore: res.HasMore}, nil
}

func (s *messageService) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.GetMessageResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	m, err := actors.Ask[*pb.Message](ctx, s.sys.Engine, s.sys.Channels, channels.GetMessage{Target: t, MessageID: req.MessageId, RootID: req.RootId})
	if err != nil {
		return nil, err
	}
	return &pb.GetMessageResponse{Message: m}, nil
}

func (s *messageService) ListThread(ctx context.Context, req *pb.ListThreadRequest) (*pb.ListThreadResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*channels.ThreadResult](ctx, s.sys.Engine, s.sys.Channels, channels.Thread{Target: t, RootID: req.RootId, BeforeID: req.BeforeId, Limit: int(req.Limit)})
	if err != nil {
		return nil, err
	}
	return &pb.ListThreadResponse{Root: res.Root, Replies: res.Replies, HasMore: res.HasMore, Following: res.Following}, nil
}

func (s *messageService) React(ctx context.Context, req *pb.ReactRequest) (*pb.ReactResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	out, err := actors.Ask[[]*pb.Reaction](ctx, s.sys.Engine, s.sys.Channels, channels.React{
		Target: t, MessageID: req.MessageId, RootID: req.RootId, Emoji: req.Emoji, Remove: req.Remove,
	})
	if err != nil {
		return nil, err
	}
	return &pb.ReactResponse{Reactions: out}, nil
}

func (s *messageService) SearchMessages(ctx context.Context, req *pb.SearchMessagesRequest) (*pb.SearchMessagesResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*index.Result](ctx, s.sys.Engine, s.sys.Search, index.Search{
		P: p, TeamID: req.TeamId, Query: req.Query, Cursor: req.Cursor, Limit: int(req.Limit),
	})
	if err != nil {
		return nil, err
	}
	return &pb.SearchMessagesResponse{Hits: res.Hits, Terms: res.Terms, NextCursor: res.Next}, nil
}

func (s *messageService) ListThreads(ctx context.Context, req *pb.ListThreadsRequest) (*pb.ListThreadsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*directory.ThreadsResult](ctx, s.sys.Engine, s.sys.Directory, directory.ListThreads{
		P: p, TeamID: req.TeamId, Offset: int(req.Offset), Limit: int(req.Limit),
	})
	if err != nil {
		return nil, err
	}
	return &pb.ListThreadsResponse{Threads: res.Threads, UnreadCount: uint32(res.Unread), NextOffset: uint32(res.Next)}, nil //nolint:gosec // bounded counts
}

func (s *messageService) PinMessage(ctx context.Context, req *pb.PinMessageRequest) (*pb.PinMessageResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	out, err := actors.Ask[*pb.Message](ctx, s.sys.Engine, s.sys.Channels, channels.Pin{
		Target: t, MessageID: req.MessageId, RootID: req.RootId, Pinned: req.Pinned,
	})
	if err != nil {
		return nil, err
	}
	return &pb.PinMessageResponse{Message: out}, nil
}

func (s *messageService) ListPinned(ctx context.Context, req *pb.ListPinnedRequest) (*pb.ListPinnedResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	out, err := actors.Ask[[]*pb.Message](ctx, s.sys.Engine, s.sys.Channels, channels.Pins{Target: t})
	if err != nil {
		return nil, err
	}
	return &pb.ListPinnedResponse{Messages: out}, nil
}

func (s *messageService) SaveMessage(ctx context.Context, req *pb.SaveMessageRequest) (*pb.SaveMessageResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	out, err := actors.Ask[*pb.Message](ctx, s.sys.Engine, s.sys.Channels, channels.Save{
		Target: t, MessageID: req.MessageId, RootID: req.RootId, Saved: req.Saved,
	})
	if err != nil {
		return nil, err
	}
	return &pb.SaveMessageResponse{Message: out}, nil
}

func (s *messageService) ListSaved(ctx context.Context, req *pb.ListSavedRequest) (*pb.ListSavedResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*directory.SavedResult](ctx, s.sys.Engine, s.sys.Directory, directory.ListSaved{
		P: p, TeamID: req.TeamId, IDsOnly: req.IdsOnly,
	})
	if err != nil {
		return nil, err
	}
	return &pb.ListSavedResponse{Hits: res.Hits, MessageIds: res.IDs}, nil
}

func (s *messageService) PressButton(ctx context.Context, req *pb.PressButtonRequest) (*pb.PressButtonResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	id, err := actors.Ask[string](ctx, s.sys.Engine, s.sys.Channels, channels.Press{
		Target: t, MessageID: req.MessageId, RootID: req.RootId, Data: req.Data,
	})
	if err != nil {
		return nil, err
	}
	return &pb.PressButtonResponse{QueryId: id}, nil
}

func (s *messageService) MarkThreadRead(ctx context.Context, req *pb.MarkThreadReadRequest) (*pb.MarkThreadReadResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.ThreadRead{Target: t, RootID: req.RootId}); err != nil {
		return nil, err
	}
	return &pb.MarkThreadReadResponse{}, nil
}

func (s *messageService) FollowThread(ctx context.Context, req *pb.FollowThreadRequest) (*pb.FollowThreadResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.Follow{Target: t, RootID: req.RootId, Follow: req.Follow}); err != nil {
		return nil, err
	}
	return &pb.FollowThreadResponse{}, nil
}

func (s *messageService) SendTyping(ctx context.Context, req *pb.SendTypingRequest) (*pb.SendTypingResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[channels.Done](ctx, s.sys.Engine, s.sys.Channels, channels.Typing{Target: t}); err != nil {
		return nil, err
	}
	return &pb.SendTypingResponse{}, nil
}

type userService struct{ *base }

func (s *userService) GetUser(ctx context.Context, req *pb.GetUserRequest) (*pb.GetUserResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*pb.User](ctx, s.sys.Engine, s.sys.Directory, directory.GetUser{P: p, UserID: req.UserId})
	if err != nil {
		return nil, err
	}
	return &pb.GetUserResponse{User: u}, nil
}

func (s *userService) UpdateProfile(ctx context.Context, req *pb.UpdateProfileRequest) (*pb.UpdateProfileResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*pb.User](ctx, s.sys.Engine, s.sys.Directory, directory.UpdateProfile{P: p, DisplayName: req.DisplayName})
	if err != nil {
		return nil, err
	}
	return &pb.UpdateProfileResponse{User: u}, nil
}

func (s *userService) SetPhoto(ctx context.Context, req *pb.SetPhotoRequest) (*pb.SetPhotoResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.Image) == 0 {
		return nil, ierrors.InvalidArgument("the photo is empty")
	}
	u, err := actors.Ask[*pb.User](ctx, s.sys.Engine, s.sys.Directory, directory.SetPhoto{P: p, Image: req.Image})
	if err != nil {
		return nil, err
	}
	return &pb.SetPhotoResponse{User: u}, nil
}

func (s *userService) RemovePhoto(ctx context.Context, _ *pb.RemovePhotoRequest) (*pb.RemovePhotoResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*pb.User](ctx, s.sys.Engine, s.sys.Directory, directory.SetPhoto{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.RemovePhotoResponse{User: u}, nil
}

func (s *userService) GetPresence(ctx context.Context, req *pb.GetPresenceRequest) (*pb.GetPresenceResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	statuses, err := actors.Ask[map[string]pb.Status](ctx, s.sys.Engine, s.sys.Directory, directory.Presence{P: p, UserIDs: req.UserIds})
	if err != nil {
		return nil, err
	}
	online := make([]string, 0, len(statuses))
	for id := range statuses {
		online = append(online, id)
	}
	return &pb.GetPresenceResponse{OnlineUserIds: online, Statuses: statuses}, nil
}

func (s *userService) SetStatus(ctx context.Context, req *pb.SetStatusRequest) (*pb.SetStatusResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*pb.User](ctx, s.sys.Engine, s.sys.Directory, directory.SetStatus{P: p, Status: req.Status, DNDMinutes: int(req.DndMinutes)})
	if err != nil {
		return nil, err
	}
	return &pb.SetStatusResponse{User: u}, nil
}

func (s *userService) SetCustomStatus(ctx context.Context, req *pb.SetCustomStatusRequest) (*pb.SetCustomStatusResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*pb.User](ctx, s.sys.Engine, s.sys.Directory, directory.SetCustomStatus{
		P: p, Emoji: req.Emoji, Text: req.Text, Minutes: int(req.Minutes),
	})
	if err != nil {
		return nil, err
	}
	return &pb.SetCustomStatusResponse{User: u}, nil
}
