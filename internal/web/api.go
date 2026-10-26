package web

import (
	"context"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/coder/websocket"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/photo"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/gateway"
)

// The client API serves native apps; client.proto describes it. Its
// requests carry the session token in the Authorization header and never
// read a cookie, so no page can make a browser call it for a signed-in
// person: a browser sends that header across origins only after a CORS
// preflight, which the API never approves.

const (
	maxRPCBody = 1 << 20
	// apiReadLimit bounds one AppFrame an app sends over its event socket:
	// the most people it may watch fit.
	apiReadLimit = 64 << 10
)

// apiCalls are the calls of the chat API apps may make: the unary calls of
// the services people use. Photos go through /api/v1/photo, which turns any
// picture into the square one the server stores; the streams belong to
// gateways and to the file endpoints.
var apiCalls = map[string][]string{
	"gochat.v1.AuthService": {
		"GetSignupInfo", "SignUp", "Login", "VerifyMFA", "Logout", "Me",
		"ChangePassword", "BeginTOTPSetup", "ConfirmTOTPSetup", "DisableTOTP",
		"ListSessions", "RevokeSession", "RevokeSessions",
	},
	"gochat.v1.UserService": {
		"GetUser", "UpdateProfile", "GetPresence", "RemovePhoto", "SetStatus", "SetCustomStatus",
	},
	"gochat.v1.TeamService": {
		"CreateTeam", "ListTeams", "GetTeam", "ListTeamMembers", "RemoveTeamMember",
		"UpdateTeamMemberRole", "CreateInvite", "GetInvite", "AcceptInvite", "ListInvites",
		"RevokeInvite", "RemoveTeamPhoto",
	},
	"gochat.v1.ChannelService": {
		"ListChannels", "BrowseChannels", "CreateChannel", "GetChannel", "JoinChannel",
		"LeaveChannel", "AddChannelMember", "RemoveChannelMember", "ListChannelMembers",
		"OpenDirectChannel", "OpenGroupChannel", "MarkChannelRead", "SetChannelMuted",
		"ListMutedChannels", "UpdateChannel", "SetChannelPrivacy", "ArchiveChannel", "SetChannelVoice",
		"DeleteConversation",
	},
	"gochat.v1.MessageService": {
		"PostMessage", "EditMessage", "DeleteMessage", "ListMessages", "GetMessage", "SendTyping",
		"ListThread", "React", "SearchMessages", "ListThreads", "MarkThreadRead", "FollowThread",
		"PinMessage", "ListPinned", "SaveMessage", "ListSaved", "PressButton",
	},
	"gochat.v1.UpdatesService": {"GetState", "GetDifference", "GetChannelDifference"},
	"gochat.v1.BotService": {
		"CreateBot", "ListBots", "UpdateBot", "ResetBotToken", "DeleteBot", "AddBotToTeam", "ListChannelCommands",
		"SetCommands", "GetCommands", "AnswerCallback", "SetWebhook", "DeleteWebhook", "GetWebhookInfo", "ConfirmUpdates",
	},
	"gochat.v1.VoiceService": {"JoinVoice", "LeaveVoice", "UpdateVoiceState", "DisconnectVoiceMember", "ListVoiceStates"},
}

// apiLimited are the calls on the stricter budget of the sign-in pages:
// those that guess at a password, a code or an invite.
var apiLimited = map[string]bool{
	"gochat.v1.AuthService/Login":            true,
	"gochat.v1.AuthService/VerifyMFA":        true,
	"gochat.v1.AuthService/SignUp":           true,
	"gochat.v1.AuthService/ChangePassword":   true,
	"gochat.v1.AuthService/ConfirmTOTPSetup": true,
	"gochat.v1.AuthService/DisableTOTP":      true,
	"gochat.v1.TeamService/GetInvite":        true,
	"gochat.v1.TeamService/AcceptInvite":     true,
}

type apiMethod struct {
	in, out protoreflect.MessageType
	limited bool
}

// apiMethods holds the request and response types of each call, by
// "<service>/<method>".
var apiMethods = func() map[string]apiMethod {
	m := make(map[string]apiMethod)
	for svc, names := range apiCalls {
		for _, name := range names {
			full := svc + "/" + name
			d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc + "." + name))
			md, ok := d.(protoreflect.MethodDescriptor)
			if err != nil || !ok || md.IsStreamingClient() || md.IsStreamingServer() {
				panic("client API: no unary call " + full)
			}
			in, err1 := protoregistry.GlobalTypes.FindMessageByName(md.Input().FullName())
			out, err2 := protoregistry.GlobalTypes.FindMessageByName(md.Output().FullName())
			if err1 != nil || err2 != nil {
				panic("client API: no types for " + full)
			}
			m[full] = apiMethod{in: in, out: out, limited: apiLimited[full]}
		}
	}
	return m
}()

// apiEncoding is how a request's or a response's body is written.
type apiEncoding struct{ json bool }

var (
	encProto = apiEncoding{}
	encJSON  = apiEncoding{json: true}
)

func (e apiEncoding) contentType() string {
	if e.json {
		return "application/json"
	}
	return "application/proto"
}

func (e apiEncoding) unmarshal(b []byte, m proto.Message) error {
	if !e.json {
		return proto.Unmarshal(b, m)
	}
	if len(b) == 0 {
		return nil
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
}

func (e apiEncoding) marshal(m proto.Message) ([]byte, error) {
	if e.json {
		return protojson.Marshal(m)
	}
	return proto.Marshal(m)
}

// encodingOf reads a media type: protobuf, JSON, or neither.
func encodingOf(mediaType string) (apiEncoding, bool) {
	mt, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return apiEncoding{}, false
	}
	switch mt {
	case "application/proto", "application/protobuf", "application/x-protobuf":
		return encProto, true
	case "application/json":
		return encJSON, true
	}
	return apiEncoding{}, false
}

// answerEncoding is how to answer a request without a body: protobuf when
// the app accepts it, JSON otherwise.
func answerEncoding(r *http.Request) apiEncoding {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if enc, ok := encodingOf(strings.TrimSpace(part)); ok && !enc.json {
			return encProto
		}
	}
	return encJSON
}

// apiToken is the request's bearer token, a session's or a bot's, or "" without
// a well-formed one.
func apiToken(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !secure.ValidCredential(token) {
		return ""
	}
	return token
}

// apiCall is who a client API request is made for: never a cookie's
// session.
func (h *Handler) apiCall(r *http.Request) drpc.Call {
	c := h.call(r)
	c.Session = apiToken(r)
	return c
}

func (h *Handler) apiWrite(w http.ResponseWriter, enc apiEncoding, status int, m proto.Message) {
	b, err := enc.marshal(m)
	if err != nil {
		h.logger.Error("failed to encode an API answer", zap.Error(err))
		status, b = http.StatusInternalServerError, nil
	}
	w.Header().Set("Content-Type", enc.contentType())
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (h *Handler) apiFail(w http.ResponseWriter, r *http.Request, enc apiEncoding, err error) {
	e := drpc.Err(err)
	status := statusOf(e.Code)
	switch {
	case e.Code == ierrors.CodeUnauthenticated:
		w.Header().Set("WWW-Authenticate", "Bearer")
	case status >= 500:
		h.logger.Error("API request failed", zap.String("path", r.URL.Path), zap.Error(err))
	}
	h.apiWrite(w, enc, status, &pb.ApiError{Code: apiCode(e.Code), Message: e.Message})
}

// apiCode is an error code as ApiError carries it; every code is small.
func apiCode(c ierrors.Code) uint32 {
	if c > math.MaxUint32 {
		return uint32(ierrors.CodeUnknown)
	}
	return uint32(c)
}

// apiSignedIn refuses a request without a bearer token; the server checks
// that it is a valid session.
func (h *Handler) apiSignedIn(w http.ResponseWriter, r *http.Request, enc apiEncoding) bool {
	if apiToken(r) == "" {
		h.apiFail(w, r, enc, ierrors.Unauthenticated())
		return false
	}
	return true
}

// apiRPC makes one call of the chat API for an app.
func (h *Handler) apiRPC(w http.ResponseWriter, r *http.Request) {
	enc, ok := encodingOf(r.Header.Get("Content-Type"))
	if !ok {
		h.apiWrite(w, encJSON, http.StatusUnsupportedMediaType, &pb.ApiError{
			Code: apiCode(ierrors.CodeInvalidArgument), Message: "send application/proto or application/json",
		})
		return
	}
	full := r.PathValue("service") + "/" + r.PathValue("method")
	m, ok := apiMethods[full]
	if !ok {
		h.apiFail(w, r, enc, ierrors.NotFound("no such call"))
		return
	}
	if r.Header.Get("Authorization") != "" && apiToken(r) == "" {
		h.apiFail(w, r, enc, ierrors.Unauthenticated())
		return
	}
	if m.limited {
		if ok, wait := h.auth.Allow(h.clientIP(r)); !ok {
			retryAfter(w, wait)
			h.apiFail(w, r, enc, ierrors.ResourceExhausted("too many attempts, wait a minute"))
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRPCBody))
	if err != nil {
		h.apiFail(w, r, enc, ierrors.InvalidArgument("the request is too large"))
		return
	}
	in := m.in.New().Interface()
	if err := enc.unmarshal(body, in); err != nil {
		h.apiFail(w, r, enc, ierrors.InvalidArgument("the request is not a valid "+string(m.in.Descriptor().Name())))
		return
	}
	out := m.out.New().Interface()
	ctx, cancel := h.client.Context(r.Context(), h.apiCall(r))
	defer cancel()
	if err := h.client.Invoke(ctx, "/"+full, in, out); err != nil {
		h.apiFail(w, r, enc, err)
		return
	}
	h.apiWrite(w, enc, http.StatusOK, out)
}

// apiUpload streams the body to the server as one attachment of a channel.
func (h *Handler) apiUpload(w http.ResponseWriter, r *http.Request) {
	enc := answerEncoding(r)
	if !h.apiSignedIn(w, r, enc) {
		return
	}
	q := r.URL.Query()
	header := &pb.UploadHeader{ChannelId: q.Get("channel_id"), Name: q.Get("name"), Voice: q.Get("voice") == "1"}
	if !uuidRe.MatchString(header.ChannelId) || header.Name == "" {
		h.apiFail(w, r, enc, ierrors.InvalidArgument("name the channel and the file"))
		return
	}
	if header.Voice {
		if d, err := strconv.ParseUint(q.Get("duration_ms"), 10, 32); err == nil {
			header.DurationMs = uint32(d)
		}
	}
	rc := http.NewResponseController(w)
	end := time.Now().Add(h.cfg.HTTP.TransferTimeout)
	_ = rc.SetWriteDeadline(end)
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.UploadLimit+1)
	r.Body = &idleBody{ReadCloser: r.Body, rc: rc, idle: h.cfg.HTTP.TransferIdleTimeout, end: end}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.HTTP.TransferTimeout)
	defer cancel()
	att, err := h.sendFile(h.client.Stream(ctx, h.apiCall(r)), header, r.Body, make([]byte, chunkSize))
	if err != nil {
		h.apiFail(w, r, enc, err)
		return
	}
	h.apiWrite(w, enc, http.StatusOK, att)
}

func (h *Handler) apiDownload(w http.ResponseWriter, r *http.Request) {
	enc := answerEncoding(r)
	if !h.apiSignedIn(w, r, enc) {
		return
	}
	h.sendDownload(w, r, h.apiCall(r), downloadAPI, func(err error) { h.apiFail(w, r, enc, err) })
}

func (h *Handler) apiPhoto(w http.ResponseWriter, r *http.Request) {
	enc := answerEncoding(r)
	if !h.apiSignedIn(w, r, enc) {
		return
	}
	h.sendPhoto(w, r, h.apiCall(r), func(err error) { h.apiFail(w, r, enc, err) })
}

// apiSetPhoto makes the body, any picture, the caller's profile photo.
func (h *Handler) apiSetPhoto(w http.ResponseWriter, r *http.Request) {
	enc := answerEncoding(r)
	if !h.apiSignedIn(w, r, enc) {
		return
	}
	data, err := h.readRawPhoto(w, r)
	if err != nil {
		h.apiFail(w, r, enc, err)
		return
	}
	ctx, cancel := h.client.Context(r.Context(), h.apiCall(r))
	defer cancel()
	res, err := h.client.Users.SetPhoto(ctx, &pb.SetPhotoRequest{Image: data})
	if err != nil {
		h.apiFail(w, r, enc, err)
		return
	}
	h.apiWrite(w, enc, http.StatusOK, res.User)
}

// readRawPhoto reads a picture sent as the whole body and turns it into the
// small square photo the server stores.
func (h *Handler) readRawPhoto(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	select {
	case h.photoReads <- struct{}{}:
		defer func() { <-h.photoReads }()
	default:
		return nil, errBusy
	}
	rc := http.NewResponseController(w)
	end := time.Now().Add(h.cfg.HTTP.TransferTimeout)
	_ = rc.SetWriteDeadline(end)
	body := &idleBody{ReadCloser: http.MaxBytesReader(w, r.Body, photo.MaxUpload), rc: rc, idle: h.cfg.HTTP.TransferIdleTimeout, end: end}
	raw, err := io.ReadAll(body)
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return nil, ierrors.InvalidArgument(photo.ErrTooLarge.Error())
	}
	if err != nil {
		return nil, ierrors.InvalidArgument("the upload was cut short")
	}
	if len(raw) == 0 {
		return nil, ierrors.InvalidArgument("choose a picture")
	}
	return h.processPhoto(r.Context(), raw)
}

// apiInfo tells an app the server's version and the oldest app it takes.
func (h *Handler) apiInfo(w http.ResponseWriter, r *http.Request) {
	h.apiWrite(w, answerEncoding(r), http.StatusOK, &pb.ServerInfo{Version: internal.Version(), MinAppVersion: h.cfg.MinAppVersion})
}

// apiGateway opens an app's event socket.
func (h *Handler) apiGateway(w http.ResponseWriter, r *http.Request) {
	if !h.apiSignedIn(w, r, encJSON) {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// Apps send no Origin; the library refuses a cross-origin one, though no
	// page can send the Authorization header with a websocket anyway.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.logger.Debug("API websocket upgrade failed", zap.Error(err))
		return
	}
	ws.SetReadLimit(apiReadLimit)
	id := h.connSeq.Add(1)
	in := gateway.NewInflow(h.gw.Cfg)
	pid := h.engine.Spawn(gateway.NewClientSession(h.gw, id, h.apiCall(r), ws, in), "client", actor.WithID(strconv.FormatUint(id, 36)),
		actor.WithMiddleware(actors.Recover(h.logger)))
	defer h.engine.Poison(pid)
	// The app says what it shows in binary AppFrames; reading also answers
	// its pings and sees it go.
	for {
		typ, data, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if !in.Admit(len(data)) {
			_ = ws.Close(websocket.StatusPolicyViolation, "too fast")
			return
		}
		h.engine.Send(pid, gateway.Frame{Data: data})
	}
}
