package drpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"

	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/audio"
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
	offset, length, some, err := byteRange(req.Offset, req.Length, att.Size)
	if err != nil {
		return err
	}
	header := &pb.DownloadResponse{Part: &pb.DownloadResponse_Header{Header: convert.Attachment(att)}}
	if !some {
		// Past the end: the caller answers from the size in the header.
		return stream.Send(header)
	}
	body, err := s.storage.GetRange(ctx, att.ObjectKey, offset, length)
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

// byteRange bounds a download's requested bytes to a file of size: the
// offset and length to read, with length 0 the rest, and whether there is
// anything to read at all.
func byteRange(offset, length, size int64) (int64, int64, bool, error) {
	if offset < 0 || length < 0 {
		return 0, 0, false, ierrors.InvalidArgument("invalid range")
	}
	if offset >= size {
		return 0, 0, false, nil
	}
	if length == 0 || length > size-offset {
		length = size - offset
	}
	if offset == 0 && length == size {
		length = 0
	}
	return offset, length, true, nil
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
	if !secure.ValidToken(a.SessionToken) {
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
