package drpc

import (
	"context"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/services/bots"
)

type botService struct{ *base }

func (s *botService) CreateBot(ctx context.Context, req *pb.CreateBotRequest) (*pb.CreateBotResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	res, err := actors.Ask[*bots.Created](ctx, s.sys.Engine, s.sys.Bots, bots.Create{
		P: p, Username: req.Username, DisplayName: req.DisplayName, Description: req.Description,
	})
	if err != nil {
		return nil, err
	}
	return &pb.CreateBotResponse{Bot: res.Bot, Token: res.Token}, nil
}

func (s *botService) ListBots(ctx context.Context, _ *pb.ListBotsRequest) (*pb.ListBotsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.Bot](ctx, s.sys.Engine, s.sys.Bots, bots.List{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.ListBotsResponse{Bots: list}, nil
}

func (s *botService) UpdateBot(ctx context.Context, req *pb.UpdateBotRequest) (*pb.UpdateBotResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	b, err := actors.Ask[*pb.Bot](ctx, s.sys.Engine, s.sys.Bots, bots.Update{
		P: p, BotID: req.BotId, DisplayName: req.DisplayName, Description: req.Description, SeesAll: req.SeesAll,
	})
	if err != nil {
		return nil, err
	}
	return &pb.UpdateBotResponse{Bot: b}, nil
}

func (s *botService) ResetBotToken(ctx context.Context, req *pb.ResetBotTokenRequest) (*pb.ResetBotTokenResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	token, err := actors.Ask[string](ctx, s.sys.Engine, s.sys.Bots, bots.ResetToken{P: p, BotID: req.BotId})
	if err != nil {
		return nil, err
	}
	return &pb.ResetBotTokenResponse{Token: token}, nil
}

func (s *botService) DeleteBot(ctx context.Context, req *pb.DeleteBotRequest) (*pb.DeleteBotResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.Delete{P: p, BotID: req.BotId}); err != nil {
		return nil, err
	}
	return &pb.DeleteBotResponse{}, nil
}

func (s *botService) AddBotToTeam(ctx context.Context, req *pb.AddBotToTeamRequest) (*pb.AddBotToTeamResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.AddToTeam{P: p, BotID: req.BotId, TeamID: req.TeamId}); err != nil {
		return nil, err
	}
	return &pb.AddBotToTeamResponse{}, nil
}

func (s *botService) ListChannelCommands(ctx context.Context, req *pb.ListChannelCommandsRequest) (*pb.ListChannelCommandsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.ChannelCommand](ctx, s.sys.Engine, s.sys.Bots, bots.ChannelCommands{P: p, ChannelID: req.ChannelId})
	if err != nil {
		return nil, err
	}
	return &pb.ListChannelCommandsResponse{Commands: list}, nil
}

func (s *botService) SetCommands(ctx context.Context, req *pb.SetCommandsRequest) (*pb.SetCommandsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.SetCommands{P: p, Commands: req.Commands}); err != nil {
		return nil, err
	}
	return &pb.SetCommandsResponse{}, nil
}

func (s *botService) GetCommands(ctx context.Context, _ *pb.GetCommandsRequest) (*pb.GetCommandsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.BotCommand](ctx, s.sys.Engine, s.sys.Bots, bots.GetCommands{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.GetCommandsResponse{Commands: list}, nil
}

func (s *botService) AnswerCallback(ctx context.Context, req *pb.AnswerCallbackRequest) (*pb.AnswerCallbackResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.Answer{P: p, QueryID: req.QueryId, Text: req.Text, Alert: req.Alert}); err != nil {
		return nil, err
	}
	return &pb.AnswerCallbackResponse{}, nil
}

func (s *botService) SetWebhook(ctx context.Context, req *pb.SetWebhookRequest) (*pb.SetWebhookResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.SetWebhook{P: p, URL: req.Url, Secret: req.Secret}); err != nil {
		return nil, err
	}
	return &pb.SetWebhookResponse{}, nil
}

func (s *botService) DeleteWebhook(ctx context.Context, _ *pb.DeleteWebhookRequest) (*pb.DeleteWebhookResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.SetWebhook{P: p}); err != nil {
		return nil, err
	}
	return &pb.DeleteWebhookResponse{}, nil
}

func (s *botService) GetWebhookInfo(ctx context.Context, _ *pb.GetWebhookInfoRequest) (*pb.GetWebhookInfoResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	return actors.Ask[*pb.GetWebhookInfoResponse](ctx, s.sys.Engine, s.sys.Bots, bots.WebhookInfo{P: p})
}

func (s *botService) ConfirmUpdates(ctx context.Context, req *pb.ConfirmUpdatesRequest) (*pb.ConfirmUpdatesResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[bots.Done](ctx, s.sys.Engine, s.sys.Bots, bots.Confirm{P: p, Pts: req.Pts}); err != nil {
		return nil, err
	}
	return &pb.ConfirmUpdatesResponse{}, nil
}
