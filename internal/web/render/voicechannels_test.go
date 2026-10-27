package render

import (
	"bytes"
	"strings"
	"testing"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

func TestSplitSeparatesVoiceChannels(t *testing.T) {
	text := &pb.Channel{Id: "t", Type: pb.ChannelType_CHANNEL_TYPE_PUBLIC}
	priv := &pb.Channel{Id: "p", Type: pb.ChannelType_CHANNEL_TYPE_PRIVATE}
	voice := &pb.Channel{Id: "v", Type: pb.ChannelType_CHANNEL_TYPE_VOICE}
	dm := &pb.Channel{Id: "d", Type: pb.ChannelType_CHANNEL_TYPE_DIRECT}
	channels, voices, direct := Split([]*pb.Channel{text, voice, dm, priv})
	if len(channels) != 2 || len(voices) != 1 || voices[0] != voice || len(direct) != 1 {
		t.Fatalf("split: %v %v %v", channels, voices, direct)
	}
}

func TestVoiceTemplates(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	state := &pb.VoiceChannelState{ChannelId: "c1", Version: 7, States: []*pb.VoiceState{
		{User: &pb.User{Id: "u1", Username: "ada", DisplayName: "<Ada>"}, ChannelId: "c1", SelfVideo: true, SelfStream: true},
		{User: &pb.User{Id: "u2", Username: "bob"}, ChannelId: "c1", SelfMute: true},
		{User: &pb.User{Id: "u3", Username: "cy"}, ChannelId: "c1", SelfMute: true, SelfDeaf: true},
	}}
	render := func(name string, data any) string {
		var buf bytes.Buffer
		if err := r.Fragment(&buf, name, data); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	out := render("voice-members", VoiceRoom{ChannelID: "c1", State: state, OOB: true})
	for _, want := range []string{`id="voice-members-c1"`, `data-version="7"`, `hx-swap-oob="outerHTML"`, `data-user="u2"`, `title="Muted"`, `title="Deafened"`} {
		if !strings.Contains(out, want) {
			t.Errorf("members lack %s: %s", want, out)
		}
	}
	if strings.Contains(out, "<Ada>") {
		t.Errorf("unescaped name: %s", out)
	}
	if strings.Count(out, "Deafened") != 1 || strings.Count(out, "Muted") != 1 {
		t.Errorf("a deafened person shows as deafened only: %s", out)
	}
	// Who sends a camera or a screen shows so to everyone, in the room or not.
	if strings.Count(out, `title="Camera on"`) != 1 || strings.Count(out, `class="vm-live"`) != 1 {
		t.Errorf("ada's camera and screen do not show: %s", out)
	}

	// Nobody talking renders an empty, versionless list.
	empty := render("voice-members", map[string]any{"ChannelID": "c2", "State": (*pb.VoiceChannelState)(nil), "OOB": false})
	if !strings.Contains(empty, `data-version="0"`) || strings.Contains(empty, "voice-member\"") {
		t.Errorf("empty members: %s", empty)
	}

	tiles := render("voice-tiles", VoiceRoom{TeamName: "acme", ChannelID: "c1", State: state, Moderator: true, MeID: "u1"})
	if strings.Count(tiles, "vt-kick") != 2 || strings.Contains(tiles, "/voice/u1/disconnect") || !strings.Contains(tiles, "/t/acme/ch/c1/voice/u2/disconnect") {
		t.Errorf("a moderator disconnects the others only: %s", tiles)
	}
	if strings.Count(tiles, `title="Camera on"`) != 1 || strings.Count(tiles, `class="vt-live"`) != 1 || !strings.Contains(tiles, `data-name="&lt;Ada&gt;"`) {
		t.Errorf("tiles lack ada's camera, screen or name: %s", tiles)
	}
	if plain := render("voice-tiles", VoiceRoom{ChannelID: "c1", State: state, MeID: "u2"}); strings.Contains(plain, "vt-kick") {
		t.Errorf("a member may disconnect others: %s", plain)
	}
	if none := render("voice-tiles", VoiceRoom{ChannelID: "c1"}); !strings.Contains(none, "Nobody is talking") {
		t.Errorf("empty room: %s", none)
	}

	// A voice channel's page is a room, with no composer.
	page := render("channel", &ChannelView{
		Team: &pb.Team{Id: "t", Name: "acme"}, Channel: &pb.Channel{Id: "c1", Type: pb.ChannelType_CHANNEL_TYPE_VOICE},
		Title: "Lobby", Member: true, Voice: state,
	})
	if !strings.Contains(page, `data-voice-room="c1"`) || strings.Contains(page, `id="composer"`) || !strings.Contains(page, "data-voice-join") {
		t.Errorf("voice page: %s", page)
	}
	for _, want := range []string{"data-voice-camera", "data-voice-screen", "data-voice-screens", "data-voice-screen-template"} {
		if !strings.Contains(page, want) {
			t.Errorf("voice page lacks %s: %s", want, page)
		}
	}
}
