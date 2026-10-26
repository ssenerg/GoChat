package bots

import (
	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/models"
)

// Create makes a bot P owns; it replies with *Created.
type Create struct {
	P                                  *models.Principal
	Username, DisplayName, Description string
}

// Created is a new bot and its token, shown once.
type Created struct {
	Bot   *pb.Bot
	Token string
}

// List replies with P's bots.
type List struct{ P *models.Principal }

type Update struct {
	P           *models.Principal
	BotID       string
	DisplayName string
	Description string
	SeesAll     bool
}

// ResetToken replies with the bot's new token.
type ResetToken struct {
	P     *models.Principal
	BotID string
}

type Delete struct {
	P     *models.Principal
	BotID string
}

type AddToTeam struct {
	P             *models.Principal
	BotID, TeamID string
}

// ChannelCommands replies with the commands of a channel's bots.
type ChannelCommands struct {
	P         *models.Principal
	ChannelID string
}

// SetCommands replaces the commands of the bot P.
type SetCommands struct {
	P        *models.Principal
	Commands []*pb.BotCommand
}

type GetCommands struct{ P *models.Principal }

// Answer answers a press of the bot P's button.
type Answer struct {
	P       *models.Principal
	QueryID string
	Text    string
	Alert   bool
}

// SetWebhook sets the bot P's webhook, or with no URL removes it.
type SetWebhook struct {
	P           *models.Principal
	URL, Secret string
}

// Confirm records that the bot P handled its updates up to Pts.
type Confirm struct {
	P   *models.Principal
	Pts int64
}

// WebhookInfo replies with *pb.GetWebhookInfoResponse.
type WebhookInfo struct{ P *models.Principal }

// Hooked tells the webhooks a bot's webhook changed.
type Hooked struct{ BotID gocql.UUID }

type Done struct{}
