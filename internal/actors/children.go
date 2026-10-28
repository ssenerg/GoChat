package actors

import (
	"context"
	"slices"
	"strings"

	"github.com/anthdm/hollywood/actor"
)

// Children are the actors one actor starts and stops. To hollywood they
// are no children: as an actor stops, hollywood poisons each child again,
// even one already stopping, and loses that second pill, so the stop would
// wait on the child forever. Each of these gets one pill, and Stop, called
// on actor.Stopped, waits for them all.
type Children struct {
	live map[string]*actor.PID
	// stopping are the children poisoned, each done once it has stopped.
	stopping []context.Context
}

// Spawn starts a child of c, named as SpawnChild would. c.Parent() is nil
// in the child.
func (k *Children) Spawn(c *actor.Context, p actor.Producer, kind string, opts ...actor.OptFunc) *actor.PID {
	pid := c.Engine().Spawn(p, c.PID().ID+"/"+kind, opts...)
	if k.live == nil {
		k.live = make(map[string]*actor.PID)
	}
	k.live[pid.ID] = pid
	return pid
}

// Alive reports whether pid is a child not poisoned and still running.
func (k *Children) Alive(c *actor.Context, pid *actor.PID) bool {
	// GetPID joins the two halves back with a slash.
	kind, id, _ := strings.Cut(pid.ID, "/")
	return k.live[pid.ID] != nil && c.Engine().Registry.GetPID(kind, id) != nil
}

// Poison stops a child, once.
func (k *Children) Poison(c *actor.Context, pid *actor.PID) {
	if k.live[pid.ID] == nil {
		return
	}
	delete(k.live, pid.ID)
	k.stopping = slices.DeleteFunc(k.stopping, func(done context.Context) bool { return done.Err() != nil })
	k.stopping = append(k.stopping, c.Engine().Poison(pid))
}

// Stop stops every child and waits until all have.
func (k *Children) Stop(c *actor.Context) {
	for _, pid := range k.live {
		k.Poison(c, pid)
	}
	for _, done := range k.stopping {
		<-done.Done()
	}
	k.stopping = nil
}
