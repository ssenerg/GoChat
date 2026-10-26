package drpc

import (
	"context"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/updates"
)

type updatesService struct{ *base }

func (s *updatesService) GetState(ctx context.Context, _ *pb.GetStateRequest) (*pb.GetStateResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if s.sys.Updates == nil {
		return nil, ierrors.Unavailable("updates are unavailable")
	}
	pts, err := actors.Ask[int64](ctx, s.sys.Engine, s.sys.Updates, updates.State{UserID: p.UserID})
	if err != nil {
		return nil, err
	}
	return &pb.GetStateResponse{Pts: pts}, nil
}

func (s *updatesService) GetDifference(ctx context.Context, req *pb.GetDifferenceRequest) (*pb.GetDifferenceResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if s.sys.Updates == nil {
		return nil, ierrors.Unavailable("updates are unavailable")
	}
	d, err := actors.Ask[*updates.Difference](ctx, s.sys.Engine, s.sys.Updates, updates.GetDifference{
		UserID: p.UserID, Pts: req.Pts, Limit: int(req.Limit),
	})
	if err != nil {
		return nil, err
	}
	return &pb.GetDifferenceResponse{Updates: d.Updates, Pts: d.Pts, TooLong: d.TooLong, More: d.More}, nil
}

func (s *updatesService) GetChannelDifference(ctx context.Context, req *pb.GetChannelDifferenceRequest) (*pb.GetChannelDifferenceResponse, error) {
	t, err := channelTarget(ctx, req.ChannelId)
	if err != nil {
		return nil, err
	}
	d, err := actors.Ask[*updates.Difference](ctx, s.sys.Engine, s.sys.Channels, channels.Difference{
		Target: t, Pts: req.Pts, Limit: int(req.Limit),
	})
	if err != nil {
		return nil, err
	}
	return &pb.GetChannelDifferenceResponse{Updates: d.Updates, Pts: d.Pts, TooLong: d.TooLong, More: d.More}, nil
}
