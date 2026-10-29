package drpc

import (
	"context"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/services/safety"
)

type safetyService struct{ *base }

func (s *safetyService) block(ctx context.Context, userID string, blocked bool) error {
	p, err := Principal(ctx)
	if err != nil {
		return err
	}
	_, err = actors.Ask[safety.Done](ctx, s.sys.Engine, s.sys.Safety, safety.Block{P: p, UserID: userID, Blocked: blocked})
	return err
}

func (s *safetyService) BlockUser(ctx context.Context, req *pb.BlockUserRequest) (*pb.BlockUserResponse, error) {
	if err := s.block(ctx, req.UserId, true); err != nil {
		return nil, err
	}
	return &pb.BlockUserResponse{}, nil
}

func (s *safetyService) UnblockUser(ctx context.Context, req *pb.UnblockUserRequest) (*pb.UnblockUserResponse, error) {
	if err := s.block(ctx, req.UserId, false); err != nil {
		return nil, err
	}
	return &pb.UnblockUserResponse{}, nil
}

func (s *safetyService) ListBlocked(ctx context.Context, _ *pb.ListBlockedRequest) (*pb.ListBlockedResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := actors.Ask[[]*pb.BlockedUser](ctx, s.sys.Engine, s.sys.Safety, safety.ListBlocked{P: p})
	if err != nil {
		return nil, err
	}
	return &pb.ListBlockedResponse{Blocked: list}, nil
}

func (s *safetyService) Report(ctx context.Context, req *pb.ReportRequest) (*pb.ReportResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[safety.Done](ctx, s.sys.Engine, s.sys.Safety, safety.Report{P: p, Req: req}); err != nil {
		return nil, err
	}
	return &pb.ReportResponse{}, nil
}

func (s *safetyService) ListReports(ctx context.Context, req *pb.ListReportsRequest) (*pb.ListReportsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	return actors.Ask[*pb.ListReportsResponse](ctx, s.sys.Engine, s.sys.Safety, safety.ListReports{
		P: p, TeamID: req.TeamId, Resolved: req.Resolved, After: req.After,
	})
}

func (s *safetyService) CountReports(ctx context.Context, _ *pb.CountReportsRequest) (*pb.CountReportsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	return actors.Ask[*pb.CountReportsResponse](ctx, s.sys.Engine, s.sys.Safety, safety.CountReports{P: p})
}

func (s *safetyService) ResolveReport(ctx context.Context, req *pb.ResolveReportRequest) (*pb.ResolveReportResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	rep, err := actors.Ask[*pb.Report](ctx, s.sys.Engine, s.sys.Safety, safety.Resolve{P: p, ReportID: req.ReportId, Action: req.Action})
	if err != nil {
		return nil, err
	}
	return &pb.ResolveReportResponse{Report: rep}, nil
}
