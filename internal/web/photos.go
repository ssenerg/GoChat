package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/photo"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const (
	// photoReaders bounds the uploads buffered at once, photoDecoders the
	// pictures decoded at once: one can take about 100MB.
	photoReaders  = 16
	photoDecoders = 2
	photoWait     = 15 * time.Second
)

var errBusy = ierrors.ResourceExhausted("the server is busy, try again in a moment")

// readPhoto reads the "photo" part of a multipart upload and turns it into
// the small square photo the server stores.
func (h *Handler) readPhoto(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	select {
	case h.photoReads <- struct{}{}:
		defer func() { <-h.photoReads }()
	default:
		return nil, errBusy
	}
	rc := http.NewResponseController(w)
	end := time.Now().Add(h.cfg.HTTP.TransferTimeout)
	_ = rc.SetWriteDeadline(end)
	r.Body = http.MaxBytesReader(w, r.Body, photo.MaxUpload+64<<10)
	r.Body = &idleBody{ReadCloser: r.Body, rc: rc, idle: h.cfg.HTTP.TransferIdleTimeout, end: end}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, ierrors.InvalidArgument("expected a multipart upload")
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, ierrors.InvalidArgument("choose a picture")
		}
		if err != nil {
			return nil, ierrors.InvalidArgument("the upload was cut short")
		}
		if part.FormName() != "photo" {
			_ = part.Close()
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(part, photo.MaxUpload+1))
		_ = part.Close()
		if err != nil {
			return nil, ierrors.InvalidArgument("the upload was cut short")
		}
		if len(raw) == 0 {
			return nil, ierrors.InvalidArgument("choose a picture")
		}
		return h.processPhoto(r.Context(), raw)
	}
}

func (h *Handler) processPhoto(ctx context.Context, raw []byte) ([]byte, error) {
	wait := time.NewTimer(photoWait)
	defer wait.Stop()
	select {
	case h.photoWork <- struct{}{}:
		defer func() { <-h.photoWork }()
	case <-wait.C:
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	data, _, err := photo.Process(bytes.NewReader(raw))
	switch {
	case errors.Is(err, photo.ErrUnsupported), errors.Is(err, photo.ErrTooLarge):
		return nil, ierrors.InvalidArgument(err.Error())
	case err != nil:
		return nil, ierrors.Internal(err)
	}
	return data, nil
}

type photoField struct {
	ID     string
	URL    string
	User   *pb.User
	Team   *pb.Team
	Error  string
	Status string
}

func (h *Handler) photoError(err error) string {
	msg := drpc.Err(err).Message
	if msg == "" {
		return "Could not save the photo."
	}
	return strings.ToUpper(msg[:1]) + strings.TrimSuffix(msg[1:], ".") + "."
}

func (h *Handler) setPhoto(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	field := photoField{ID: "profile-photo", URL: "/settings/photo", User: u}
	remove := strings.HasSuffix(r.URL.Path, "/remove")
	var data []byte
	var err error
	if !remove {
		data, err = h.readPhoto(w, r)
	}
	if err == nil {
		ctx, cancel := h.ctx(r)
		defer cancel()
		var res interface{ GetUser() *pb.User }
		if remove {
			res, err = h.client.Users.RemovePhoto(ctx, &pb.RemovePhotoRequest{})
		} else {
			res, err = h.client.Users.SetPhoto(ctx, &pb.SetPhotoRequest{Image: data})
		}
		if err == nil {
			field.User = res.GetUser()
			field.Status = "Saved."
			h.me.Delete(h.sessionToken(r))
		}
	}
	if err != nil {
		field.Error = h.photoError(err)
	}
	h.fragment(w, r, http.StatusOK, "photo-field", field)
}

type teamSettingsData struct {
	Team *pb.Team
	// Photo is the team's photo, which its admins change.
	Photo *photoField
	Error string
}

// teamSettings shows every member how to leave the team, and its admins
// its photo.
func (h *Handler) teamSettings(w http.ResponseWriter, r *http.Request) {
	_, team, _, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	data := teamSettingsData{Team: team}
	if team.Role == pb.TeamRole_TEAM_ROLE_ADMIN {
		data.Photo = &photoField{ID: "team-photo", URL: "/t/" + team.Name + "/photo", Team: team}
	}
	h.fragment(w, r, http.StatusOK, "team-settings", data)
}

// setTeamPhoto answers with the photo field and the refreshed team rail.
func (h *Handler) setTeamPhoto(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	field := photoField{ID: "team-photo", URL: "/t/" + team.Name + "/photo", Team: team}
	remove := strings.HasSuffix(r.URL.Path, "/remove")
	var data []byte
	var err error
	if !remove {
		data, err = h.readPhoto(w, r)
	}
	if err == nil {
		var res interface{ GetTeam() *pb.Team }
		if remove {
			res, err = h.client.Teams.RemoveTeamPhoto(ctx, &pb.RemoveTeamPhotoRequest{TeamId: team.Id})
		} else {
			res, err = h.client.Teams.SetTeamPhoto(ctx, &pb.SetTeamPhotoRequest{TeamId: team.Id, Image: data})
		}
		if err == nil {
			field.Team = res.GetTeam()
			field.Status = "Saved."
		}
	}
	if err != nil {
		field.Error = h.photoError(err)
	}
	var out strings.Builder
	if rerr := h.render.Fragment(&out, "photo-field", field); rerr != nil {
		h.fail(w, r, ierrors.Internal(rerr))
		return
	}
	if err == nil {
		if teams, lerr := h.client.Teams.ListTeams(ctx, &pb.ListTeamsRequest{}); lerr == nil {
			rail := appPage{Teams: teams.Teams, Team: field.Team, OOB: true}
			if rerr := h.render.Fragment(&out, "team-rail", rail); rerr != nil {
				h.fail(w, r, ierrors.Internal(rerr))
				return
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, out.String())
}

// servePhoto answers /photos/{u|t}/{id}/{version}. A URL whose version is
// current never changes, so browsers keep it; an older one gets the current
// photo, briefly.
func (h *Handler) servePhoto(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	h.sendPhoto(w, r, h.call(r), func(err error) { h.fail(w, r, err) })
}

// sendPhoto answers the photo of the request's {id} and {version}, on
// behalf of call.
func (h *Handler) sendPhoto(w http.ResponseWriter, r *http.Request, call drpc.Call, fail func(error)) {
	kind, id, version := r.PathValue("kind"), r.PathValue("id"), r.PathValue("version")
	if (kind != "u" && kind != "t") || !uuidRe.MatchString(id) || !photo.ValidVersion(version) {
		fail(errNoPage)
		return
	}
	req := &pb.GetPhotoRequest{Version: version}
	if kind == "t" {
		req.Owner = &pb.GetPhotoRequest_TeamId{TeamId: id}
	} else {
		req.Owner = &pb.GetPhotoRequest_UserId{UserId: id}
	}
	ctx, cancel := h.client.Context(r.Context(), call)
	defer cancel()
	res, err := h.client.Files.GetPhoto(ctx, req)
	if err != nil {
		fail(err)
		return
	}
	if res.ContentType != "image/jpeg" && res.ContentType != "image/png" {
		fail(errNoPage)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", res.ContentType)
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if res.Version == version {
		hd.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "private, max-age=60")
	}
	_, _ = w.Write(res.Image)
}

var linkImageRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (h *Handler) serveLinkImage(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	h.sendLinkImage(w, r, h.call(r), func(err error) { h.fail(w, r, err) })
}

// sendLinkImage answers the picture of a link preview, which the server
// fetched and made safe, named by its content: it never changes.
func (h *Handler) sendLinkImage(w http.ResponseWriter, r *http.Request, call drpc.Call, fail func(error)) {
	id := r.PathValue("id")
	if !linkImageRe.MatchString(id) {
		fail(errNoPage)
		return
	}
	ctx, cancel := h.client.Context(r.Context(), call)
	defer cancel()
	res, err := h.client.Files.GetLinkImage(ctx, &pb.GetLinkImageRequest{Id: id})
	if err != nil {
		fail(err)
		return
	}
	if res.ContentType != "image/jpeg" && res.ContentType != "image/png" {
		fail(errNoPage)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", res.ContentType)
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hd.Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(res.Image)
}
