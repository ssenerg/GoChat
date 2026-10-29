package web

import (
	"net/http"

	"golang.org/x/sync/errgroup"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/web/render"
)

type keptView struct {
	Team    *pb.Team
	Channel *pb.Channel
	Title   string
	Hits    []render.Hit
}

// pinMessage pins a message, or with pinned=0 unpins it, and answers with
// the message as it now shows. mod only keeps the delete button the message
// showed: deleting checks again.
func (h *Handler) pinMessage(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Messages.PinMessage(ctx, &pb.PinMessageRequest{
		ChannelId: chID, MessageId: msgID, RootId: rootID, Pinned: r.PostFormValue("pinned") == "1",
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.refold(w, r, res.Message, u)
}

// refold answers with m as it now shows, folded when the reader blocked
// its author.
func (h *Handler) refold(w http.ResponseWriter, r *http.Request, m *pb.Message, u *pb.User) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	view := h.render.Message(m, u, r.PostFormValue("mod") == "1")
	render.Fold([]*render.Message{view}, h.blockedOrNone(ctx))
	h.fragment(w, r, http.StatusOK, "message", view)
}

// saveMessage saves a message for the caller, or with saved=0 drops it.
func (h *Handler) saveMessage(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Messages.SaveMessage(ctx, &pb.SaveMessageRequest{
		ChannelId: chID, MessageId: msgID, RootId: rootID, Saved: r.PostFormValue("saved") == "1",
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.refold(w, r, res.Message, u)
}

// pinsPanel shows a channel's pinned messages in the side pane, or with
// ?list=1 just the list.
func (h *Handler) pinsPanel(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID := r.PathValue("channel")
	if !uuidRe.MatchString(chID) {
		h.notFound(w, r)
		return
	}
	var (
		ch   *pb.Channel
		pins []*pb.Message
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		res, err := h.client.Channels.GetChannel(gctx, &pb.GetChannelRequest{ChannelId: chID})
		if err == nil {
			ch = res.Channel
		}
		return err
	})
	g.Go(func() error {
		res, err := h.client.Messages.ListPinned(gctx, &pb.ListPinnedRequest{ChannelId: chID})
		if err == nil {
			pins = res.Messages
		}
		return err
	})
	if err := g.Wait(); err != nil {
		h.fail(w, r, err)
		return
	}
	hits := make([]*pb.SearchHit, len(pins))
	for i, m := range pins {
		hits[i] = &pb.SearchHit{Message: m, Channel: ch}
	}
	view := keptView{Team: team, Channel: ch, Title: render.Title(ch), Hits: render.Hits(team, hits, nil)}
	h.fragment(w, r, http.StatusOK, listOr(r, "pins"), view)
}

// savedPanel shows what the caller saved in the team and their
// conversations.
func (h *Handler) savedPanel(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	res, err := h.client.Messages.ListSaved(ctx, &pb.ListSavedRequest{TeamId: team.Id})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, listOr(r, "saved"), keptView{Team: team, Hits: render.Hits(team, res.Hits, nil)})
}

func listOr(r *http.Request, pane string) string {
	if r.URL.Query().Get("list") == "1" {
		return pane + "-list"
	}
	return pane
}
