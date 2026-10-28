package drpc

import (
	"context"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/directory"
)

// optionalPrincipal is the caller of a call that runs with or without a
// session.
func optionalPrincipal(ctx context.Context) *models.Principal {
	p, _ := Principal(ctx)
	return p
}

func (s *authService) ListSSOProviders(ctx context.Context, _ *pb.ListSSOProvidersRequest) (*pb.ListSSOProvidersResponse, error) {
	list, err := actors.Ask[[]accounts.SSOProviderInfo](ctx, s.sys.Engine, s.sys.Accounts, accounts.SSOProviders{})
	if err != nil {
		return nil, err
	}
	out := &pb.ListSSOProvidersResponse{Providers: make([]*pb.SSOProvider, len(list))}
	for i, p := range list {
		out.Providers[i] = &pb.SSOProvider{Id: p.ID, Name: p.Name}
	}
	return out, nil
}

func (s *authService) BeginSSO(ctx context.Context, req *pb.BeginSSORequest) (*pb.BeginSSOResponse, error) {
	start, err := actors.Ask[*accounts.SSOStart](ctx, s.sys.Engine, s.sys.Accounts, accounts.BeginSSO{
		Provider: req.ProviderId, RedirectURI: req.RedirectUri, AppChallenge: req.AppChallenge, P: optionalPrincipal(ctx),
	})
	if err != nil {
		return nil, err
	}
	return &pb.BeginSSOResponse{Url: start.URL, State: start.State}, nil
}

func (s *authService) FinishSSO(ctx context.Context, req *pb.FinishSSORequest) (*pb.FinishSSOResponse, error) {
	cl := ClientOf(ctx)
	res, err := actors.Ask[*accounts.SSOResult](ctx, s.sys.Engine, s.sys.Accounts, accounts.FinishSSO{
		State: req.State, Code: req.Code, InviteToken: req.InviteToken, P: optionalPrincipal(ctx), IP: cl.IP, UserAgent: cl.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	out := &pb.FinishSSOResponse{ProviderName: res.Provider}
	switch {
	case res.Linked:
		out.Result = &pb.FinishSSOResponse_Linked{Linked: true}
	case res.LinkToken != "":
		out.Result = &pb.FinishSSOResponse_LinkToken{LinkToken: res.LinkToken}
	case res.AppCode != "":
		out.Result = &pb.FinishSSOResponse_AppCode{AppCode: res.AppCode}
	case res.Login != nil && res.Login.MFAToken != "":
		out.Result = &pb.FinishSSOResponse_MfaToken{MfaToken: res.Login.MFAToken}
	case res.Login != nil:
		out.Result = &pb.FinishSSOResponse_Session{Session: res.Login.Session}
	}
	// As with SignUp, a join that failed is finished by opening the invite
	// again.
	if !ids.IsZero(res.InviteTeamID) {
		if team, err := actors.Ask[*pb.Team](ctx, s.sys.Engine, s.sys.Directory, directory.JoinTeam{UserID: res.User.ID, TeamID: res.InviteTeamID, InviteHash: res.InviteHash}); err == nil {
			out.Team = team
		}
	}
	return out, nil
}

func (s *authService) RedeemSSO(ctx context.Context, req *pb.RedeemSSORequest) (*pb.RedeemSSOResponse, error) {
	cl := ClientOf(ctx)
	res, err := actors.Ask[*accounts.LoginResult](ctx, s.sys.Engine, s.sys.Accounts, accounts.RedeemSSO{
		Code: req.Code, Verifier: req.Verifier, IP: cl.IP, UserAgent: cl.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	if res.MFAToken != "" {
		return &pb.RedeemSSOResponse{Result: &pb.RedeemSSOResponse_MfaToken{MfaToken: res.MFAToken}}, nil
	}
	return &pb.RedeemSSOResponse{Result: &pb.RedeemSSOResponse_Session{Session: res.Session}}, nil
}

func (s *authService) GetSSOLink(ctx context.Context, req *pb.GetSSOLinkRequest) (*pb.GetSSOLinkResponse, error) {
	info, err := actors.Ask[*accounts.SSOLinkInfo](ctx, s.sys.Engine, s.sys.Accounts, accounts.GetSSOLink{Token: req.LinkToken})
	if err != nil {
		return nil, err
	}
	return &pb.GetSSOLinkResponse{ProviderName: info.Provider, Email: info.Email}, nil
}

func (s *authService) LinkSSO(ctx context.Context, req *pb.LinkSSORequest) (*pb.LinkSSOResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	info, err := actors.Ask[*accounts.SSOLinkInfo](ctx, s.sys.Engine, s.sys.Accounts, accounts.LinkSSO{P: p, Token: req.LinkToken})
	if err != nil {
		return nil, err
	}
	return &pb.LinkSSOResponse{ProviderName: info.Provider, Email: info.Email}, nil
}

func (s *authService) ListSSOIdentities(ctx context.Context, req *pb.ListSSOIdentitiesRequest) (*pb.ListSSOIdentitiesResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.SSOIdentity](ctx, s.sys.Engine, s.sys.Accounts, accounts.ListSSOIdentities{P: p, UserID: req.UserId})
	if err != nil {
		return nil, err
	}
	return &pb.ListSSOIdentitiesResponse{Identities: list}, nil
}

func (s *authService) UnlinkSSO(ctx context.Context, req *pb.UnlinkSSORequest) (*pb.UnlinkSSOResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[accounts.Done](ctx, s.sys.Engine, s.sys.Accounts, accounts.UnlinkSSO{
		P: p, UserID: req.UserId, Issuer: req.Issuer, Subject: req.Subject,
	}); err != nil {
		return nil, err
	}
	return &pb.UnlinkSSOResponse{}, nil
}

func (s *adminService) ListSSOAccounts(ctx context.Context, req *pb.ListSSOAccountsRequest) (*pb.ListSSOAccountsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	return actors.Ask[*pb.ListSSOAccountsResponse](ctx, s.sys.Engine, s.sys.Accounts, accounts.ListSSOAccounts{P: p, Provider: req.ProviderId, After: req.After})
}
