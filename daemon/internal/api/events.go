package api

import (
	"context"
	"errors"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

var deadlineEvents = map[fencing.Event]v1alpha1.DeadlineEvent_Kind{
	fencing.EventArmed:   v1alpha1.DeadlineEvent_KIND_ARMED,
	fencing.EventRenewed: v1alpha1.DeadlineEvent_KIND_RENEWED,
	fencing.EventExpired: v1alpha1.DeadlineEvent_KIND_EXPIRED,
}

// WatchEvents streams the node's events after the sequence, then each new one,
// until the client goes or the server stops. When the events after the
// sequence are not held it sends ResyncRequired and ends.
func (s *Server) WatchEvents(req *v1alpha1.WatchEventsRequest, stream grpc.ServerStreamingServer[v1alpha1.WatchEventsResponse]) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	defer context.AfterFunc(s.stopping, cancel)()

	w := s.events.Watch(req.GetAfterSequence())
	for {
		e, err := w.Next(ctx)
		switch {
		case errors.Is(err, events.ErrResync):
			return stream.Send(&v1alpha1.WatchEventsResponse{
				Event: &v1alpha1.WatchEventsResponse_ResyncRequired{ResyncRequired: &v1alpha1.ResyncRequired{}},
			})
		case err != nil:
			// The client went, or the server is stopping: the client resumes
			// from the last sequence it holds.
			return nil
		}
		if err := stream.Send(eventToWire(e)); err != nil {
			return err
		}
	}
}

func eventToWire(e events.Event) *v1alpha1.WatchEventsResponse {
	out := &v1alpha1.WatchEventsResponse{Sequence: e.Sequence, MachineId: string(e.Machine)}
	switch body := e.Body.(type) {
	case events.StateChanged:
		out.Event = &v1alpha1.WatchEventsResponse_StateChanged{
			StateChanged: &v1alpha1.StateChanged{State: states[body.State]},
		}
	case events.Deleted:
		out.Event = &v1alpha1.WatchEventsResponse_MachineDeleted{MachineDeleted: &v1alpha1.MachineDeleted{}}
	case events.Deadline:
		out.Event = &v1alpha1.WatchEventsResponse_Deadline{
			Deadline: &v1alpha1.DeadlineEvent{Kind: deadlineEvents[body.Event]},
		}
	case events.Driver:
		out.Event = &v1alpha1.WatchEventsResponse_Driver{
			Driver: &v1alpha1.DriverEvent{Name: body.Event.Name, Detail: body.Event.Detail},
		}
	}
	return out
}
