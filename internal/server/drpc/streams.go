package drpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/audio"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/files"
	"github.com/ssenerg/gochat/internal/services/hub"
)

const (
	chunkSize = 64 << 10
	sniffSize = 512
)

var errTooLarge = ierrors.InvalidArgument("file is too large")

type fileService struct{ *base }

func (s *fileService) Upload(stream pb.DRPCFileService_UploadStream) error {
	ctx := stream.Context()
	p, err := Principal(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return ierrors.InvalidArgument("the first message must be the header")
	}
	up, err := actors.Ask[*files.Upload](ctx, s.sys.Engine, s.sys.Files, files.BeginUpload{
		P: p, ChannelID: header.ChannelId, Name: header.Name, Voice: header.Voice, DurationMS: header.DurationMs,
	})
	if err != nil {
		return err
	}
	if !s.startUpload(p.UserID) {
		return ierrors.ResourceExhausted("too many uploads at once, wait for one to finish")
	}
	defer s.endUpload(p.UserID)
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		return ierrors.ResourceExhausted("the server is busy, try again")
	}

	maxSize, tooLarge := s.cfg.Files.MaxSize, errTooLarge
	if up.Voice {
		maxSize, tooLarge = min(s.cfg.Files.MaxVoiceSize, maxSize), ierrors.InvalidArgument("the voice message is too large")
	}
	if up.Room >= 0 && up.Room < maxSize {
		maxSize, tooLarge = up.Room, files.ErrQuota
	}
	var head bytes.Buffer
	size := int64(0)
	eof := false
	for head.Len() < sniffSize && !eof {
		chunk, err := s.nextChunk(stream)
		if errors.Is(err, io.EOF) {
			eof = true
			break
		}
		if err != nil {
			return err
		}
		size += int64(len(chunk))
		if size > maxSize {
			return tooLarge
		}
		head.Write(chunk)
	}
	if size == 0 {
		return ierrors.InvalidArgument("file is empty")
	}
	contentType := http.DetectContentType(head.Bytes())
	if up.Voice {
		var ok bool
		if contentType, ok = audio.Sniff(head.Bytes()); !ok {
			return ierrors.InvalidArgument("a voice message must be an audio recording")
		}
	}

	pr, pw := io.Pipe()
	stored := make(chan struct{})
	var (
		n      int64
		putErr error
	)
	go func() {
		defer close(stored)
		n, putErr = s.storage.Put(ctx, up.ObjectKey, pr, contentType)
		_ = pr.CloseWithError(putErr)
	}()
	abort := func(cause error) error {
		_ = pw.CloseWithError(cause)
		<-stored
		c, cancel := context.WithTimeout(context.Background(), s.cfg.Files.OpTimeout)
		defer cancel()
		_ = s.storage.Delete(c, up.ObjectKey)
		return cause
	}
	if _, err := pw.Write(head.Bytes()); err != nil {
		return abort(err)
	}
	for !eof {
		chunk, err := s.nextChunk(stream)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return abort(err)
		}
		size += int64(len(chunk))
		if size > maxSize {
			return abort(tooLarge)
		}
		if _, err := pw.Write(chunk); err != nil {
			return abort(err)
		}
	}
	_ = pw.Close()
	<-stored
	if putErr != nil {
		return abort(ierrors.Internal(putErr))
	}
	if n != size {
		return abort(ierrors.Internal(errors.New("stored size differs from received size")))
	}
	att, err := actors.Ask[*pb.Attachment](ctx, s.sys.Engine, s.sys.Files, files.CompleteUpload{P: p, Upload: up, Size: size, ContentType: contentType})
	if err != nil {
		return abort(err)
	}
	return stream.SendAndClose(&pb.UploadResponse{Attachment: att})
}

func (s *fileService) startUpload(userID gocql.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploading[userID] >= s.cfg.Files.MaxUserUploads {
		return false
	}
	s.uploading[userID]++
	return true
}

func (s *fileService) endUpload(userID gocql.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploading[userID]--; s.uploading[userID] <= 0 {
		delete(s.uploading, userID)
	}
}

func (s *fileService) nextChunk(stream pb.DRPCFileService_UploadStream) ([]byte, error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		chunk := msg.GetChunk()
		if msg.GetHeader() != nil {
			return nil, ierrors.InvalidArgument("only one header is allowed")
		}
		if len(chunk) > 0 {
			return chunk, nil
		}
	}
}

func (s *fileService) GetPhoto(ctx context.Context, req *pb.GetPhotoRequest) (*pb.GetPhotoResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	ph, err := actors.Ask[*directory.Photo](ctx, s.sys.Engine, s.sys.Directory, directory.GetPhoto{
		P: p, UserID: req.GetUserId(), TeamID: req.GetTeamId(), Version: req.Version,
	})
	if err != nil {
		return nil, err
	}
	return &pb.GetPhotoResponse{Image: ph.Data, ContentType: ph.ContentType, Version: ph.Version}, nil
}

func (s *fileService) GetStorageUsage(ctx context.Context, req *pb.GetStorageUsageRequest) (*pb.GetStorageUsageResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := actors.Ask[*files.StorageUsage](ctx, s.sys.Engine, s.sys.Files, files.Usage{P: p, UserID: req.UserId})
	if err != nil {
		return nil, err
	}
	return &pb.GetStorageUsageResponse{Used: u.Used, Quota: u.Quota, Custom: u.Custom}, nil
}

func (s *fileService) DiscardUploads(ctx context.Context, req *pb.DiscardUploadsRequest) (*pb.DiscardUploadsResponse, error) {
	p, err := Principal(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := actors.Ask[files.Done](ctx, s.sys.Engine, s.sys.Files, files.Discard{P: p, AttachmentIDs: req.AttachmentIds}); err != nil {
		return nil, err
	}
	return &pb.DiscardUploadsResponse{}, nil
}

func (s *fileService) Download(req *pb.DownloadRequest, stream pb.DRPCFileService_DownloadStream) error {
	ctx := stream.Context()
	p, err := Principal(ctx)
	if err != nil {
		return err
	}
	att, err := actors.Ask[*models.Attachment](ctx, s.sys.Engine, s.sys.Files, files.OpenDownload{P: p, AttachmentID: req.AttachmentId})
	if err != nil {
		return err
	}
	first, last, ok, err := files.Span(req.Range, att.Size)
	if err != nil {
		return err
	}
	header := &pb.DownloadResponse{Part: &pb.DownloadResponse_Header{Header: convert.Attachment(att)}}
	if !ok {
		header.Unsatisfiable = true
		return stream.Send(header)
	}
	var body io.ReadCloser
	if req.Range == nil {
		body, err = s.storage.Get(ctx, att.ObjectKey)
	} else {
		header.Served = &pb.ByteRange{First: first, Last: last}
		body, err = s.storage.GetRange(ctx, att.ObjectKey, first, last)
	}
	if err != nil {
		return ierrors.Internal(err)
	}
	defer func() { _ = body.Close() }()
	if err := stream.Send(header); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.DownloadResponse{Part: &pb.DownloadResponse_Chunk{Chunk: buf[:n]}}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ierrors.Internal(err)
		}
	}
}

var gatewayIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var streamSeq atomic.Uint64

type gatewayService struct{ *base }

// Connect serves one gateway: it attaches the connections the gateway opens
// for its sessions, and the hub streams back their events.
func (s *gatewayService) Connect(stream pb.DRPCGatewayService_ConnectStream) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || !gatewayIDRe.MatchString(hello.GatewayId) {
		return ierrors.InvalidArgument("the first frame must be a valid hello")
	}
	// Each stream is its own hub entry, so a reconnecting gateway never
	// races the teardown of its previous stream.
	key := hello.GatewayId + "#" + strconv.FormatUint(streamSeq.Add(1), 36)
	// Unregistering also covers a registration the hub handles after the Ask
	// gave up on it.
	defer s.sys.Engine.Send(s.sys.Hub, hub.UnregisterGateway{ID: key})
	if _, err := actors.Ask[hub.Registered](ctx, s.sys.Engine, s.sys.Hub, hub.RegisterGateway{ID: key, Stream: stream}); err != nil {
		return err
	}

	checks := newWatchChecks(watchSlots, watchGap)
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch f := req.Frame.(type) {
		case *pb.ConnectRequest_Attach:
			s.attach(ctx, key, f.Attach)
		case *pb.ConnectRequest_Detach:
			s.sys.Engine.Send(s.sys.Hub, hub.Detach{GatewayID: key, ConnID: f.Detach.ConnId})
		case *pb.ConnectRequest_View:
			var ch string
			if id, ok := ids.Parse(f.View.ChannelId); ok {
				ch = id.String()
			}
			s.sys.Engine.Send(s.sys.Hub, hub.View{GatewayID: key, ConnID: f.View.ConnId, ChannelID: ch})
		case *pb.ConnectRequest_Watch:
			// The check reads the people's teams: it runs off the stream.
			checks.add(ctx, f.Watch, func(ctx context.Context, w *pb.Watch, seq uint64) { s.watch(ctx, key, w, seq) })
		}
	}
}

// watch keeps those of a connection's watch list its user shares a team
// with, as presence answers, so a gateway or an app can follow no one else.
// Only the people it does not watch yet need checking.
func (s *gatewayService) watch(ctx context.Context, key string, w *pb.Watch, seq uint64) {
	todo, err := actors.Ask[hub.Unchecked](ctx, s.sys.Engine, s.sys.Hub, hub.ToCheck{GatewayID: key, ConnID: w.ConnId, UserIDs: w.UserIds})
	if err != nil || todo.UserID == "" {
		return
	}
	var known []string
	if len(todo.UserIDs) > 0 {
		known, err = actors.Ask[[]string](ctx, s.sys.Engine, s.sys.Directory, directory.Acquainted{UserID: todo.UserID, UserIDs: todo.UserIDs})
		if err != nil {
			return
		}
	}
	s.sys.Engine.Send(s.sys.Hub, hub.Watch{GatewayID: key, ConnID: w.ConnId, Seq: seq, UserIDs: w.UserIds, Checked: known})
}

// A gateway's stream runs watchSlots checks at once, and a connection's
// one after another, at least watchGap apart: one that keeps sending lists
// has its latest checked.
const (
	watchSlots = 8
	watchGap   = time.Second
)

type watchChecks struct {
	gap   time.Duration
	slots chan struct{}
	mu    sync.Mutex
	seq   uint64
	// latest holds each connection's newest list not yet checked, and
	// running the connections whose checks are under way.
	latest  map[uint64]*pb.Watch
	running map[uint64]bool
}

func newWatchChecks(slots int, gap time.Duration) *watchChecks {
	return &watchChecks{gap: gap, slots: make(chan struct{}, slots), latest: map[uint64]*pb.Watch{}, running: map[uint64]bool{}}
}

// add takes a connection's newest list, starting its checks unless they run.
func (w *watchChecks) add(ctx context.Context, f *pb.Watch, check func(context.Context, *pb.Watch, uint64)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.latest[f.ConnId] = f
	if !w.running[f.ConnId] {
		w.running[f.ConnId] = true
		go w.run(ctx, f.ConnId, check)
	}
}

// run checks a connection's latest list until none is left.
func (w *watchChecks) run(ctx context.Context, conn uint64, check func(context.Context, *pb.Watch, uint64)) {
	for {
		w.mu.Lock()
		f := w.latest[conn]
		delete(w.latest, conn)
		if f == nil {
			delete(w.running, conn)
			w.mu.Unlock()
			return
		}
		w.seq++
		seq := w.seq
		w.mu.Unlock()
		select {
		case w.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		check(ctx, f, seq)
		<-w.slots
		select {
		case <-time.After(w.gap):
		case <-ctx.Done():
			return
		}
	}
}

func (s *gatewayService) attach(ctx context.Context, key string, a *pb.Attach) {
	// A failure that is not about the session asks the browser to retry
	// rather than to sign in again.
	reject := func(err error) {
		retry := err != nil && !ierrors.Is(err, ierrors.CodeUnauthenticated) && !ierrors.Is(err, ierrors.CodeNotFound)
		s.sys.Engine.Send(s.sys.Hub, hub.Reject{GatewayID: key, ConnID: a.ConnId, Retry: retry})
	}
	if !secure.ValidCredential(a.SessionToken) {
		reject(nil)
		return
	}
	p, ok := s.sys.Sessions.Cached(a.SessionToken)
	if !ok {
		var err error
		if p, err = actors.Ask[*models.Principal](ctx, s.sys.Engine, s.sys.Auth, accounts.Authenticate{Token: a.SessionToken}); err != nil {
			reject(err)
			return
		}
	}
	u, err := s.sys.Users.Get(ctx, p.UserID)
	if err != nil {
		reject(err)
		return
	}
	s.sys.Engine.Send(s.sys.Hub, hub.Attach{GatewayID: key, ConnID: a.ConnId, User: convert.User(u), SessionID: p.SessionID, ExpiresAt: p.ExpiresAt})
}
