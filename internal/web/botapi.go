package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/bots"
	"github.com/ssenerg/gochat/internal/web/gateway"
)

// The HTTP Bot API serves bots that would rather not keep a socket: each
// call of the GoChat protocol as a POST of JSON, answered in JSON. See
// docs/protocol.md.

// botMethods names the calls bots make most by shorter names.
var botMethods = map[string]string{
	"getMe":          "gochat.v1.AuthService/Me",
	"sendMessage":    "gochat.v1.MessageService/PostMessage",
	"editMessage":    "gochat.v1.MessageService/EditMessage",
	"deleteMessage":  "gochat.v1.MessageService/DeleteMessage",
	"getMessage":     "gochat.v1.MessageService/GetMessage",
	"sendTyping":     "gochat.v1.MessageService/SendTyping",
	"react":          "gochat.v1.MessageService/React",
	"answerCallback": "gochat.v1.BotService/AnswerCallback",
	"setCommands":    "gochat.v1.BotService/SetCommands",
	"getCommands":    "gochat.v1.BotService/GetCommands",
	"listTeams":      "gochat.v1.TeamService/ListTeams",
	"listChannels":   "gochat.v1.ChannelService/ListChannels",
	"getChannel":     "gochat.v1.ChannelService/GetChannel",
	"setWebhook":     "gochat.v1.BotService/SetWebhook",
	"deleteWebhook":  "gochat.v1.BotService/DeleteWebhook",
	"getWebhookInfo": "gochat.v1.BotService/GetWebhookInfo",
}

const (
	// maxPollWait bounds how long getUpdates waits for an update.
	maxPollWait = 50 * time.Second
	maxPollSize = 100
)

var jsonIn = protojson.UnmarshalOptions{DiscardUnknown: true}

// botPolls holds the getUpdates waiting, one per bot: a newer one ends the
// older.
type botPolls struct {
	mu    sync.Mutex
	polls map[string]*poll
}

type poll struct{ cancel context.CancelCauseFunc }

var errPollReplaced = errors.New("terminated by another getUpdates")

// begin registers a poll of bot, ending the one before.
func (b *botPolls) begin(ctx context.Context, bot string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	p := &poll{cancel: cancel}
	b.mu.Lock()
	if old := b.polls[bot]; old != nil {
		old.cancel(errPollReplaced)
	}
	b.polls[bot] = p
	b.mu.Unlock()
	return ctx, func() {
		b.mu.Lock()
		if b.polls[bot] == p {
			delete(b.polls, bot)
		}
		b.mu.Unlock()
		cancel(nil)
	}
}

type botReply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
}

func (h *Handler) botWrite(w http.ResponseWriter, status int, r botReply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func (h *Handler) botFail(w http.ResponseWriter, r *http.Request, err error) {
	e := drpc.Err(err)
	status := statusOf(e.Code)
	switch {
	case e.Code == ierrors.CodeUnauthenticated:
		w.Header().Set("WWW-Authenticate", "Bearer")
	case e.Code == ierrors.CodeUnimplemented:
		status = http.StatusNotFound
	case status >= 500:
		h.logger.Error("bot API request failed", zap.String("path", r.URL.Path), zap.Error(err))
	}
	h.botWrite(w, status, botReply{ErrorCode: status, Description: e.Message})
}

func (h *Handler) botOK(w http.ResponseWriter, m proto.Message) {
	b, err := bots.JSON.Marshal(m)
	if err != nil {
		h.botWrite(w, http.StatusInternalServerError, botReply{ErrorCode: http.StatusInternalServerError, Description: "internal error"})
		return
	}
	h.botWrite(w, http.StatusOK, botReply{OK: true, Result: b})
}

// botAPI makes one call of the GoChat protocol for a bot.
func (h *Handler) botAPI(w http.ResponseWriter, r *http.Request) {
	token := apiToken(r)
	if !secure.IsBotToken(token) {
		h.botFail(w, r, ierrors.Unauthenticated())
		return
	}
	method := r.PathValue("method")
	full, ok := botMethods[method]
	if !ok {
		full = method
	}
	m, ok := apiMethods[full]
	if method != "getUpdates" && !ok {
		h.botFail(w, r, ierrors.New(ierrors.CodeUnimplemented, "no such method"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRPCBody))
	if err != nil {
		h.botFail(w, r, ierrors.InvalidArgument("the request is too large"))
		return
	}
	call := h.apiCall(r)
	if method == "getUpdates" {
		h.getUpdates(w, r, call, body)
		return
	}
	in := m.in.New().Interface()
	if len(body) > 0 {
		if err := jsonIn.Unmarshal(body, in); err != nil {
			h.botFail(w, r, ierrors.InvalidArgument("the request is not a valid "+string(m.in.Descriptor().Name())))
			return
		}
	}
	out := m.out.New().Interface()
	ctx, cancel := h.client.Context(r.Context(), call)
	defer cancel()
	if err := h.client.Invoke(ctx, "/"+full, in, out); err != nil {
		h.botFail(w, r, err)
		return
	}
	h.botOK(w, out)
}

type getUpdatesRequest struct {
	Offset  json.Number `json:"offset"`
	Limit   int         `json:"limit"`
	Timeout int         `json:"timeout"`
}

// getUpdates answers the bot's updates from offset, confirming those
// before it, and waits up to timeout seconds for one when none are there.
func (h *Handler) getUpdates(w http.ResponseWriter, r *http.Request, call drpc.Call, body []byte) {
	var req getUpdatesRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			h.botFail(w, r, ierrors.InvalidArgument("getUpdates takes offset, limit and timeout"))
			return
		}
	}
	var offset int64
	if req.Offset != "" {
		n, err := strconv.ParseInt(string(req.Offset), 10, 64)
		if err != nil || n < 0 {
			h.botFail(w, r, ierrors.InvalidArgument("invalid offset"))
			return
		}
		offset = n
	}
	limit := req.Limit
	if limit <= 0 || limit > maxPollSize {
		limit = maxPollSize
	}
	wait := min(time.Duration(max(req.Timeout, 0))*time.Second, maxPollWait)

	ctx, cancel := h.client.Context(r.Context(), call)
	info := &pb.GetWebhookInfoResponse{}
	err := h.client.Invoke(ctx, "/gochat.v1.BotService/GetWebhookInfo", &pb.GetWebhookInfoRequest{}, info)
	cancel()
	if err != nil {
		h.botFail(w, r, err)
		return
	}
	if info.Url != "" {
		h.botWrite(w, http.StatusConflict, botReply{ErrorCode: http.StatusConflict, Description: "a webhook is set: delete it to use getUpdates"})
		return
	}
	from := info.DeliveredPts
	if offset > 0 {
		from = offset - 1
		if from > info.DeliveredPts {
			ctx, cancel := h.client.Context(r.Context(), call)
			err := h.client.Invoke(ctx, "/gochat.v1.BotService/ConfirmUpdates", &pb.ConfirmUpdatesRequest{Pts: from}, &pb.ConfirmUpdatesResponse{})
			cancel()
			if err != nil {
				h.botFail(w, r, err)
				return
			}
		}
	}

	// The answer may come later than other requests' may.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + h.cfg.HTTP.WriteTimeout))
	pctx, done := h.botPolls.begin(r.Context(), secure.PublicID(secure.Hash(call.Session)))
	defer done()
	// Attached before the first read, the poll misses no update that
	// lands after it.
	var events chan any
	if wait > 0 {
		events = make(chan any, 8)
		pid := h.engine.Spawn(gateway.NewWaiter(h.gw, h.connSeq.Add(1), call.Session, events), "poll")
		defer h.engine.Poison(pid)
		if err := awaitReady(pctx, events); err != nil {
			h.pollEnded(w, r, pctx, err)
			return
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		d, err := h.difference(pctx, call, from, limit)
		if err != nil {
			h.pollEnded(w, r, pctx, err)
			return
		}
		// Updates about messages deleted since are passed over.
		if len(d.Updates) == 0 && !d.TooLong && d.Pts > from {
			from = d.Pts
			continue
		}
		if len(d.Updates) > 0 || d.TooLong || wait == 0 {
			h.botOK(w, d)
			return
		}
		if err := awaitUpdate(pctx, events, deadline.C); err != nil {
			if errors.Is(err, errPollTimeout) {
				h.botOK(w, d)
				return
			}
			h.pollEnded(w, r, pctx, err)
			return
		}
	}
}

func (h *Handler) difference(ctx context.Context, call drpc.Call, from int64, limit int) (*pb.GetDifferenceResponse, error) {
	ctx, cancel := h.client.Context(ctx, call)
	defer cancel()
	out := &pb.GetDifferenceResponse{}
	err := h.client.Invoke(ctx, "/gochat.v1.UpdatesService/GetDifference", &pb.GetDifferenceRequest{Pts: from, Limit: uint32(limit)}, out) //nolint:gosec // at most maxPollSize
	return out, err
}

// pollEnded answers a poll that stopped: replaced by a newer one, or
// failed.
func (h *Handler) pollEnded(w http.ResponseWriter, r *http.Request, ctx context.Context, err error) {
	if errors.Is(context.Cause(ctx), errPollReplaced) {
		h.botWrite(w, http.StatusConflict, botReply{ErrorCode: http.StatusConflict, Description: errPollReplaced.Error()})
		return
	}
	if r.Context().Err() != nil {
		return
	}
	h.botFail(w, r, err)
}

var errPollTimeout = errors.New("no update in time")

// awaitReady waits until the waiter is attached.
func awaitReady(ctx context.Context, events <-chan any) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-events:
			switch ev.(type) {
			case gateway.Ready:
				return nil
			case gateway.Rejected, gateway.Revoked:
				return ierrors.Unauthenticated()
			}
		}
	}
}

// awaitUpdate waits for a numbered update of the account, or until the
// deadline.
func awaitUpdate(ctx context.Context, events <-chan any, deadline <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errPollTimeout
		case ev := <-events:
			switch e := ev.(type) {
			case gateway.Event:
				if e.Event.Pts > 0 && e.Event.PtsChannelId == "" {
					return nil
				}
			case gateway.Rejected, gateway.Revoked:
				return ierrors.Unauthenticated()
			}
		}
	}
}
