// Command echobot is a small GoChat bot: /echo repeats what follows it under
// a Thanks button, /ping answers pong, and /remindme 10m stand up writes
// "stand up" to whoever asked in a direct message ten minutes later.
//
//	GOCHAT_URL=https://chat.example.com GOCHAT_BOT_TOKEN=b_... go run ./examples/echobot
//
// With -state it keeps the last update it handled in a file, and after a
// restart it gets the messages it missed.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/pkg/gochat"
)

func main() {
	state := flag.String("state", "", "a file to keep the last update handled in")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv("GOCHAT_URL"), os.Getenv("GOCHAT_BOT_TOKEN"), *state); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, url, token, state string) error {
	if url == "" || token == "" {
		return errors.New("set GOCHAT_URL and GOCHAT_BOT_TOKEN")
	}
	c, err := gochat.Dial(ctx, gochat.Options{URL: url, Token: token, Client: "echobot/1", Pts: load(state)})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	me := c.Me()
	log.Printf("signed in as @%s", me.GetUsername())

	err = c.Call(ctx, "gochat.v1.BotService/SetCommands", &pb.SetCommandsRequest{Commands: []*pb.BotCommand{
		{Name: "echo", Description: "Repeat what follows"},
		{Name: "ping", Description: "Answer pong"},
		{Name: "remindme", Description: "Remind you in a direct message: /remindme 10m stand up"},
	}}, &pb.SetCommandsResponse{})
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case u, ok := <-c.Updates():
			if !ok {
				return c.Err()
			}
			if err := handle(ctx, c, me, u); err != nil {
				log.Printf("handling an update: %v", err)
			}
			if u.Event.GetPts() > 0 && u.Event.GetPtsChannelId() == "" {
				save(state, c.Pts())
			}
		}
	}
}

func handle(ctx context.Context, c *gochat.Client, me *pb.User, u gochat.Update) error {
	if u.Reload {
		log.Print("updates were lost while the bot was away")
		return nil
	}
	posted := u.Event.GetMessagePosted()
	if m := posted.GetMessage(); m != nil && m.GetAuthor().GetId() != me.GetId() {
		name, args, ok := gochat.Command(m.Content, me.GetUsername())
		if !ok {
			return nil
		}
		reply := &pb.PostMessageRequest{ChannelId: m.ChannelId, RootId: m.RootId}
		switch name {
		case "echo":
			if args == "" {
				args = "Say something after /echo."
			}
			reply.Content = args
			reply.Markup = &pb.Markup{Rows: []*pb.ButtonRow{{Buttons: []*pb.Button{
				{Text: "Thanks", Action: &pb.Button_CallbackData{CallbackData: "thanks"}},
			}}}}
		case "ping":
			reply.Content = "pong"
		case "remindme":
			reply.Content = remind(ctx, c, posted.GetTeamId(), m.GetAuthor().GetId(), args)
		default:
			return nil
		}
		return c.Call(ctx, "gochat.v1.MessageService/PostMessage", reply, &pb.PostMessageResponse{})
	}
	if q := u.Event.GetCallbackQuery(); q != nil {
		text := "You're welcome, " + strings.TrimSpace(q.GetFrom().GetDisplayName()) + "."
		return c.Call(ctx, "gochat.v1.BotService/AnswerCallback", &pb.AnswerCallbackRequest{QueryId: q.Id, Text: text}, &pb.AnswerCallbackResponse{})
	}
	return nil
}

// maxRemind bounds how far off a reminder is: the bot keeps reminders in
// memory, and a restart forgets them.
const maxRemind = 24 * time.Hour

// remind writes text to userID in their direct conversation in teamID once
// the duration args start with is over, and answers what it set.
func remind(ctx context.Context, c *gochat.Client, teamID, userID, args string) string {
	when, text, _ := strings.Cut(args, " ")
	d, err := time.ParseDuration(when)
	text = strings.TrimSpace(text)
	if err != nil || d <= 0 || d > maxRemind || text == "" {
		return "Say when, up to 24h, and what: /remindme 10m stand up."
	}
	time.AfterFunc(d, func() {
		if ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		dm, err := c.OpenDirect(ctx, teamID, userID)
		if err == nil {
			err = c.Call(ctx, "gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{ChannelId: dm.GetId(), Content: text}, &pb.PostMessageResponse{})
		}
		if err != nil {
			log.Printf("sending a reminder: %v", err)
		}
	})
	return "I'll remind you in a direct message in " + d.String() + "."
}

func load(path string) int64 {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file
	if err != nil {
		return 0
	}
	pts, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return pts
}

func save(path string, pts int64) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(pts, 10)), 0o600); err != nil {
		log.Printf("saving the state: %v", err)
	}
}
