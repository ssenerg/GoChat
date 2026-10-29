package safety

import (
	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/models"
)

// Block blocks UserID for P, or with Blocked unset unblocks them; it
// replies with Done.
type Block struct {
	P       *models.Principal
	UserID  string
	Blocked bool
}

// ListBlocked replies with the []*pb.BlockedUser P blocked.
type ListBlocked struct{ P *models.Principal }

// Report files a report for P; it replies with Done.
type Report struct {
	P   *models.Principal
	Req *pb.ReportRequest
}

// ListReports replies with *pb.ListReportsResponse.
type ListReports struct {
	P        *models.Principal
	TeamID   string
	Resolved bool
	After    string
}

// CountReports replies with *pb.CountReportsResponse.
type CountReports struct{ P *models.Principal }

// Resolve acts on a report and replies with it as now resolved.
type Resolve struct {
	P        *models.Principal
	ReportID string
	Action   pb.ReportAction
}

type Done struct{}
