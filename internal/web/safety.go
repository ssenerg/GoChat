package web

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/web/render"
)

// blockedSet is whom the signed-in person blocked, by id.
func (h *Handler) blockedSet(ctx context.Context) (map[string]bool, error) {
	res, err := h.client.Safety.ListBlocked(ctx, &pb.ListBlockedRequest{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(res.Blocked))
	for _, b := range res.Blocked {
		out[b.GetUser().GetId()] = true
	}
	return out, nil
}

// blockedOrNone is blockedSet, or no one when it cannot be read: a page
// shows then without folding.
func (h *Handler) blockedOrNone(ctx context.Context) map[string]bool {
	blocked, err := h.blockedSet(ctx)
	if err != nil {
		h.logger.Debug("failed to list the people blocked", zap.Error(err))
	}
	return blocked
}

type personCard struct {
	User      *pb.User
	Team      *pb.Team
	ChannelID string
	Me        bool
	Blocked   bool
}

// card gathers what a person's card shows, from the channel it was opened
// in.
func (h *Handler) card(ctx context.Context, me *pb.User, userID, channelID string) (*personCard, error) {
	res, err := h.client.Users.GetUser(ctx, &pb.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, err
	}
	ch, err := h.client.Channels.GetChannel(ctx, &pb.GetChannelRequest{ChannelId: channelID})
	if err != nil {
		return nil, err
	}
	blocked, err := h.blockedSet(ctx)
	if err != nil {
		return nil, err
	}
	c := &personCard{User: res.User, ChannelID: channelID, Me: userID == me.GetId(), Blocked: blocked[userID]}
	if id := ch.GetChannel().GetTeamId(); id != "" {
		t, err := h.client.Teams.GetTeam(ctx, &pb.GetTeamRequest{Key: &pb.GetTeamRequest_Id{Id: id}})
		if err != nil {
			return nil, err
		}
		c.Team = t.Team
	}
	return c, nil
}

// personCardModal shows a person: whom to message, block or report.
func (h *Handler) personCardModal(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	userID, channelID := r.PathValue("user"), r.URL.Query().Get("channel")
	if !uuidRe.MatchString(userID) || !uuidRe.MatchString(channelID) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	c, err := h.card(ctx, u, userID, channelID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "person-card", c)
}

// block blocks or unblocks a person, and answers with what showed the
// choice: their card, the list of people blocked, or a note.
func (h *Handler) block(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	userID := r.PathValue("user")
	if !uuidRe.MatchString(userID) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	blocked := r.PostFormValue("blocked") == "1"
	var err error
	if blocked {
		_, err = h.client.Safety.BlockUser(ctx, &pb.BlockUserRequest{UserId: userID})
	} else {
		_, err = h.client.Safety.UnblockUser(ctx, &pb.UnblockUserRequest{UserId: userID})
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	switch channelID := r.PostFormValue("channel"); {
	case uuidRe.MatchString(channelID):
		c, err := h.card(ctx, u, userID, channelID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.fragment(w, r, http.StatusOK, "person-card", c)
	case r.PostFormValue("from") == "settings":
		h.blockedList(w, r)
	default:
		text := "Unblocked."
		if blocked {
			text = "Blocked."
		}
		h.fragment(w, r, http.StatusOK, "toast", render.Toast{Kind: "ok", Text: text})
	}
}

// blockedList answers with the people the signed-in person blocked, for
// their settings.
func (h *Handler) blockedList(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Safety.ListBlocked(ctx, &pb.ListBlockedRequest{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "blocked-list", res.Blocked)
}

type reportReason struct {
	Value, Label string
	Reason       pb.ReportReason
}

var reportReasons = []reportReason{
	{"spam", "Spam", pb.ReportReason_REPORT_REASON_SPAM},
	{"harassment", "Harassment or bullying", pb.ReportReason_REPORT_REASON_HARASSMENT},
	{"hate", "Hate speech", pb.ReportReason_REPORT_REASON_HATE},
	{"sexual", "Sexual content", pb.ReportReason_REPORT_REASON_SEXUAL},
	{"violence", "Violence or threats", pb.ReportReason_REPORT_REASON_VIOLENCE},
	{"other", "Something else", pb.ReportReason_REPORT_REASON_OTHER},
}

func reasonFrom(v string) pb.ReportReason {
	for _, rr := range reportReasons {
		if rr.Value == v {
			return rr.Reason
		}
	}
	return pb.ReportReason_REPORT_REASON_UNSPECIFIED
}

// reasonLabel is how a report's reason reads to admins.
func reasonLabel(r pb.ReportReason) string {
	for _, rr := range reportReasons {
		if rr.Reason == r {
			return rr.Label
		}
	}
	return ""
}

type reportForm struct {
	Heading, URL string
	Reasons      []reportReason
	Reason       string
	Details      string
	Error        string
}

// reportTarget reads what a report form is about: a message, or a person
// of a team.
func reportTarget(r *http.Request) (*pb.ReportRequest, string, bool) {
	q := r.URL.Query()
	if userID := r.PathValue("user"); userID != "" {
		team := q.Get("team")
		if !uuidRe.MatchString(userID) || !uuidRe.MatchString(team) {
			return nil, "", false
		}
		return &pb.ReportRequest{Target: &pb.ReportRequest_User{User: &pb.ReportedUser{TeamId: team, UserId: userID}}},
			"/u/" + userID + "/report?team=" + team, true
	}
	chID, msgID, root := r.PathValue("channel"), r.PathValue("message"), q.Get("root")
	if !uuidRe.MatchString(chID) || !uuidRe.MatchString(msgID) || (root != "" && !uuidRe.MatchString(root)) {
		return nil, "", false
	}
	url := "/m/" + chID + "/" + msgID + "/report"
	if root != "" {
		url += "?root=" + root
	}
	return &pb.ReportRequest{Target: &pb.ReportRequest_Message{Message: &pb.ReportedMessage{ChannelId: chID, MessageId: msgID, RootId: root}}}, url, true
}

func reportHeading(req *pb.ReportRequest) string {
	if req.GetUser() != nil {
		return "Report this person"
	}
	return "Report this message"
}

func (h *Handler) reportModal(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	req, url, ok := reportTarget(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	h.fragment(w, r, http.StatusOK, "report-form", reportForm{Heading: reportHeading(req), URL: url, Reasons: reportReasons})
}

// report sends a report to the team's admins.
func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	req, url, ok := reportTarget(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	req.Reason, req.Details = reasonFrom(r.PostFormValue("reason")), r.PostFormValue("details")
	ctx, cancel := h.ctx(r)
	defer cancel()
	if _, err := h.client.Safety.Report(ctx, req); err != nil {
		msg, ok := formError(err)
		if !ok {
			h.fail(w, r, err)
			return
		}
		h.fragment(w, r, http.StatusOK, "report-form", reportForm{
			Heading: reportHeading(req), URL: url, Reasons: reportReasons,
			Reason: r.PostFormValue("reason"), Details: req.Details, Error: msg,
		})
		return
	}
	w.Header().Set("HX-Trigger", "closeModal")
	h.fragment(w, r, http.StatusOK, "toast", render.Toast{Kind: "ok", Text: "Thank you. The admins will look at your report."})
}

type reportRow struct {
	Report *pb.Report
	// System is set on the admin pages, where a system admin may also
	// deactivate the person. Mine is set on a report the reader sent,
	// which another admin acts on.
	System bool
	Mine   bool
	Error  string
}

func (row reportRow) Reason() string { return reasonLabel(row.Report.GetReason()) }

// Done says what the admin did, as the row shows it.
func (row reportRow) Done() string {
	switch row.Report.GetAction() {
	case pb.ReportAction_REPORT_ACTION_DISMISS:
		return "Dismissed"
	case pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE:
		return "Message deleted"
	case pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER:
		return "Removed from the team"
	case pb.ReportAction_REPORT_ACTION_DEACTIVATE:
		return "Account deactivated"
	}
	return ""
}

type reportList struct {
	Rows    []reportRow
	MoreURL string
	Empty   string
}

func rows(list []*pb.Report, system bool, me *pb.User) []reportRow {
	out := make([]reportRow, len(list))
	for i, rep := range list {
		out[i] = reportRow{Report: rep, System: system, Mine: rep.GetReporter().GetId() == me.GetId()}
	}
	return out
}

// teamReports lists a team's open reports, for its admins' team settings.
func (h *Handler) teamReports(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	after := r.URL.Query().Get("after")
	res, err := h.client.Safety.ListReports(ctx, &pb.ListReportsRequest{TeamId: team.Id, After: after})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := reportList{Rows: rows(res.Reports, false, u), Empty: "No open reports."}
	if res.Next != "" {
		d.MoreURL = "/t/" + team.Name + "/reports?after=" + res.Next
	}
	if after != "" {
		h.fragment(w, r, http.StatusOK, "report-rows", d)
		return
	}
	h.fragment(w, r, http.StatusOK, "report-list", d)
}

// reportCounts answers with the badges of the reports waiting for the
// signed-in admin: their team's, and every team's for system admins.
func (h *Handler) reportCounts(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	res, err := h.client.Safety.CountReports(ctx, &pb.CountReportsRequest{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "reports-badges", map[string]any{
		"Team": res.Teams[team.Id], "All": res.All, "System": u.Role == pb.SystemRole_SYSTEM_ROLE_ADMIN,
	})
}

func actionFrom(v string) pb.ReportAction {
	switch v {
	case "dismiss":
		return pb.ReportAction_REPORT_ACTION_DISMISS
	case "delete":
		return pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE
	case "remove":
		return pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER
	case "deactivate":
		return pb.ReportAction_REPORT_ACTION_DEACTIVATE
	}
	return pb.ReportAction_REPORT_ACTION_UNSPECIFIED
}

// resolveReport acts on a report and answers with its row as now
// resolved, or as it was with what went wrong.
func (h *Handler) resolveReport(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	id := r.PathValue("report")
	if !uuidRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	system := r.PostFormValue("system") == "1"
	res, err := h.client.Safety.ResolveReport(ctx, &pb.ResolveReportRequest{ReportId: id, Action: actionFrom(r.PostFormValue("action"))})
	if err != nil {
		msg, ok := formError(err)
		if !ok {
			h.fail(w, r, err)
			return
		}
		w.Header().Set("HX-Reswap", "none")
		h.fragment(w, r, http.StatusOK, "toast", render.Toast{Kind: "error", Text: msg})
		return
	}
	h.fragment(w, r, http.StatusOK, "report-row", reportRow{Report: res.Report, System: system})
}

type adminReportsData struct {
	Page
	Resolved bool
	List     reportList
}

// adminReports lists every team's reports for system admins.
func (h *Handler) adminReports(w http.ResponseWriter, r *http.Request) {
	me := h.admin(w, r)
	if me == nil {
		return
	}
	q := r.URL.Query()
	resolved, after := q.Get("resolved") == "1", q.Get("after")
	if after != "" && !uuidRe.MatchString(after) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Safety.ListReports(ctx, &pb.ListReportsRequest{Resolved: resolved, After: after})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := adminReportsData{
		Page:     Page{Title: "Reports", CSRF: h.csrfToken(w, r), User: me},
		Resolved: resolved,
		List:     reportList{Rows: rows(res.Reports, true, me), Empty: "No open reports."},
	}
	if resolved {
		d.List.Empty = "No resolved reports."
	}
	if res.Next != "" {
		d.List.MoreURL = "/admin/reports?after=" + res.Next
		if resolved {
			d.List.MoreURL += "&resolved=1"
		}
	}
	if isHTMX(r) && after != "" {
		h.fragment(w, r, http.StatusOK, "report-rows", d.List)
		return
	}
	h.page(w, r, http.StatusOK, "admin-reports", d)
}
