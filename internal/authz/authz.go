// Package authz decides what a role may do. Who holds which role is data
// (the user's system role, team and channel memberships); casbin holds what
// each role is allowed, with the role hierarchy, stored in scylla.
package authz

import (
	_ "embed"
	"fmt"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
	"go.uber.org/zap"
)

//go:embed model.conf
var modelText string

type Object string

const (
	ObjTeam          Object = "team"
	ObjInvite        Object = "invite"
	ObjTeamMember    Object = "team_member"
	ObjChannel       Object = "channel"
	ObjChannelMember Object = "channel_member"
	ObjDirect        Object = "direct"
	ObjMessage       Object = "message"
	ObjFile          Object = "file"
	ObjUser          Object = "user"
	ObjSession       Object = "session"
	ObjStorage       Object = "storage"
	// ObjVoice is the talk in voice channels.
	ObjVoice Object = "voice"
	// ObjReport is a report of a message or a person to admins.
	ObjReport Object = "report"
)

type Action string

const (
	ActCreate        Action = "create"
	ActRead          Action = "read"
	ActBrowse        Action = "browse"
	ActJoin          Action = "join"
	ActCreatePublic  Action = "create_public"
	ActCreatePrivate Action = "create_private"
	ActRemove        Action = "remove"
	ActAdd           Action = "add"
	ActUpdateRole    Action = "update_role"
	ActEditOwn       Action = "edit_own"
	ActDeleteOwn     Action = "delete_own"
	ActDeleteAny     Action = "delete_any"
	ActUpload        Action = "upload"
	ActUpdate        Action = "update"
	ActArchive       Action = "archive"
	ActMakePrivate   Action = "make_private"
	ActMakePublic    Action = "make_public"
	ActFind          Action = "find"
	ActDeactivate    Action = "deactivate"
	// ActManageAny acts on what belongs to another user.
	ActManageAny   Action = "manage_any"
	ActCreateVoice Action = "create_voice"
	// ActConnect joins the talk of a voice channel; ActMove disconnects
	// others from it.
	ActConnect Action = "connect"
	ActMove    Action = "move"
	// ActResolve acts on a report.
	ActResolve Action = "resolve"
)

type Authorizer struct {
	logger   *zap.Logger
	enforcer *casbin.SyncedCachedEnforcer
}

func New(logger *zap.Logger, adapter persist.Adapter) (*Authorizer, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("failed to parse casbin model: %w", err)
	}
	e, err := casbin.NewSyncedCachedEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("failed to create casbin enforcer: %w", err)
	}
	if err := e.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("failed to load casbin policy: %w", err)
	}
	if policy, err := e.GetPolicy(); err != nil || len(policy) == 0 {
		logger.Warn("casbin policy is empty: every action will be denied; run the scylla migrations")
	}
	return &Authorizer{logger: logger, enforcer: e}, nil
}

// Can reports whether any of roles may act on obj. Empty roles are skipped.
func (a *Authorizer) Can(obj Object, act Action, roles ...string) bool {
	for _, role := range roles {
		if role == "" {
			continue
		}
		ok, err := a.enforcer.Enforce(role, string(obj), string(act))
		if err != nil {
			a.logger.Error("casbin enforce failed", zap.String("role", role), zap.Error(err))
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// Reload rereads the policy from storage.
func (a *Authorizer) Reload() error {
	if err := a.enforcer.InvalidateCache(); err != nil {
		return err
	}
	return a.enforcer.LoadPolicy()
}
