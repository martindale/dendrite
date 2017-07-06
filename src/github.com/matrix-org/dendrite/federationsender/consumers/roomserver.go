// Copyright 2017 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package consumers

import (
	"encoding/json"
	"fmt"

	log "github.com/Sirupsen/logrus"
	"github.com/matrix-org/dendrite/common"
	"github.com/matrix-org/dendrite/common/config"
	"github.com/matrix-org/dendrite/federationsender/queue"
	"github.com/matrix-org/dendrite/federationsender/storage"
	"github.com/matrix-org/dendrite/federationsender/types"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/gomatrixserverlib"
	sarama "gopkg.in/Shopify/sarama.v1"
)

// OutputRoomEvent consumes events that originated in the room server.
type OutputRoomEvent struct {
	roomServerConsumer *common.ContinualConsumer
	db                 *storage.Database
	queues             *queue.OutgoingQueues
	query              api.RoomserverQueryAPI
}

// NewOutputRoomEvent creates a new OutputRoomEvent consumer. Call Start() to begin consuming from room servers.
func NewOutputRoomEvent(cfg *config.Dendrite, queues *queue.OutgoingQueues, store *storage.Database) (*OutputRoomEvent, error) {
	kafkaConsumer, err := sarama.NewConsumer(cfg.Kafka.Addresses, nil)
	if err != nil {
		return nil, err
	}
	roomServerURL := cfg.RoomServerURL()

	consumer := common.ContinualConsumer{
		Topic:          string(cfg.Kafka.Topics.OutputRoomEvent),
		Consumer:       kafkaConsumer,
		PartitionStore: store,
	}
	s := &OutputRoomEvent{
		roomServerConsumer: &consumer,
		db:                 store,
		queues:             queues,
		query:              api.NewRoomserverQueryAPIHTTP(roomServerURL, nil),
	}
	consumer.ProcessMessage = s.onMessage

	return s, nil
}

// Start consuming from room servers
func (s *OutputRoomEvent) Start() error {
	return s.roomServerConsumer.Start()
}

// onMessage is called when the federation server receives a new event from the room server output log.
// It is unsafe to call this with messages for the same room in multiple gorountines
// because updates it will likely fail with a types.EventIDMismatchError when it
// realises that it cannot update the room state using the deltas.
func (s *OutputRoomEvent) onMessage(msg *sarama.ConsumerMessage) error {
	// Parse out the event JSON
	var output api.OutputRoomEvent
	if err := json.Unmarshal(msg.Value, &output); err != nil {
		// If the message was invalid, log it and move on to the next message in the stream
		log.WithError(err).Errorf("roomserver output log: message parse failure")
		return nil
	}

	ev, err := gomatrixserverlib.NewEventFromTrustedJSON(output.Event, false)
	if err != nil {
		log.WithError(err).Errorf("roomserver output log: event parse failure")
		return nil
	}
	log.WithFields(log.Fields{
		"event_id":       ev.EventID(),
		"room_id":        ev.RoomID(),
		"send_as_server": output.SendAsServer,
	}).Info("received event from roomserver")

	if err = s.processMessage(output, ev); err != nil {
		// panic rather than continue with an inconsistent database
		log.WithFields(log.Fields{
			"event":      string(ev.JSON()),
			log.ErrorKey: err,
			"add":        output.AddsStateEventIDs,
			"del":        output.RemovesStateEventIDs,
		}).Panicf("roomserver output log: write event failure")
		return nil
	}

	return nil
}

// processMessage updates the list of currently joined hosts in the room
// and then sends the event to the hosts that were joined before the event.
func (s *OutputRoomEvent) processMessage(ore api.OutputRoomEvent, ev gomatrixserverlib.Event) error {
	addsStateEvents, err := s.lookupStateEvents(ore.AddsStateEventIDs, ev)
	if err != nil {
		return err
	}
	addsJoinedHosts, err := joinedHostsFromEvents(addsStateEvents)
	if err != nil {
		return err
	}
	// Update our copy of the current state.
	// We keep a copy of the current state because the state at each event is
	// expressed as a delta against the current state.
	// TODO: handle EventIDMismatchError and recover the current state by talking
	// to the roomserver
	oldJoinedHosts, err := s.db.UpdateRoom(
		ev.RoomID(), ore.LastSentEventID, ev.EventID(),
		addsJoinedHosts, ore.RemovesStateEventIDs,
	)
	if err != nil {
		return err
	}

	if ore.SendAsServer == api.DoNotSendToOtherServers {
		// Ignore event that we don't need to send anywhere.
		return nil
	}

	// Work out which hosts were joined at the event itself.
	joinedHostsAtEvent, err := s.joinedHostsAtEvent(ore, ev, oldJoinedHosts)
	if err != nil {
		return err
	}

	// Send the event.
	if err = s.queues.SendEvent(
		&ev, gomatrixserverlib.ServerName(ore.SendAsServer), joinedHostsAtEvent,
	); err != nil {
		return err
	}

	return nil
}

// joinedHostsAtEvent works out a list of matrix servers that were joined to
// the room at the event.
// It is important to use the state at the event for sending messages because:
//   1) We shouldn't send messages to servers that weren't in the room.
//   2) If a server is kicked from the rooms it should still be told about the
//      kick event,
// Usually the list can be calculated locally, but sometimes it will need fetch
// events from the room server.
// Returns an error if there was a problem talking to the room server.
func (s *OutputRoomEvent) joinedHostsAtEvent(
	ore api.OutputRoomEvent, ev gomatrixserverlib.Event, oldJoinedHosts []types.JoinedHost,
) ([]gomatrixserverlib.ServerName, error) {
	// Combine the delta into a single delta so that the adds and removes can
	// cancel each other out. This should reduce the number of times we need
	// to fetch a state event from the room server.
	combinedAdds, combinedRemoves := combineDeltas(
		ore.AddsStateEventIDs, ore.RemovesStateEventIDs,
		ore.StateBeforeAddsEventIDs, ore.StateBeforeRemovesEventIDs,
	)
	combinedAddsEvents, err := s.lookupStateEvents(combinedAdds, ev)
	if err != nil {
		return nil, err
	}

	combinedAddsJoinedHosts, err := joinedHostsFromEvents(combinedAddsEvents)
	if err != nil {
		return nil, err
	}

	removed := map[string]bool{}
	for _, eventID := range combinedRemoves {
		removed[eventID] = true
	}

	joined := map[gomatrixserverlib.ServerName]bool{}
	for _, joinedHost := range oldJoinedHosts {
		if removed[joinedHost.MemberEventID] {
			// This m.room.member event is part of the current state of the
			// room, but not part of the state at the event we are processing
			// Therefore we can't use it to tell whether the server was in
			// the room at the event.
			continue
		}
		joined[joinedHost.ServerName] = true
	}

	for _, joinedHost := range combinedAddsJoinedHosts {
		// This m.room.member event was part of the state of the room at the
		// event, but isn't part of the current state of the room now.
		joined[joinedHost.ServerName] = true
	}

	var result []gomatrixserverlib.ServerName
	for serverName, include := range joined {
		if include {
			result = append(result, serverName)
		}
	}
	return result, nil
}

// joinedHostsFromEvents turns a list of state events into a list of joined hosts.
// This errors if one of the events was invalid.
// It should be impossible for an invalid event to get this far in the pipeline.
func joinedHostsFromEvents(evs []gomatrixserverlib.Event) ([]types.JoinedHost, error) {
	var joinedHosts []types.JoinedHost
	for _, ev := range evs {
		if ev.Type() != "m.room.member" || ev.StateKey() == nil {
			continue
		}
		membership, err := ev.Membership()
		if err != nil {
			return nil, err
		}
		if *membership != "join" {
			continue
		}
		_, serverName, err := gomatrixserverlib.ParseID('@', *ev.StateKey())
		if err != nil {
			return nil, err
		}
		joinedHosts = append(joinedHosts, types.JoinedHost{
			MemberEventID: ev.EventID(), ServerName: serverName,
		})
	}
	return joinedHosts, nil
}

// combineDeltas combines two deltas into a single delta.
// Assumes that the order of operations is add(1), remove(1), add(2), remove(2).
// Removes duplicate entries and redundant operations from each delta.
func combineDeltas(adds1, removes1, adds2, removes2 []string) (adds, removes []string) {
	addSet := map[string]bool{}
	removeSet := map[string]bool{}

	// combine processes each unique value in a list.
	// If the value is in the removeFrom set then it is removed from that set.
	// Otherwise it is added to the addTo set.
	combine := func(values []string, removeFrom, addTo map[string]bool) {
		processed := map[string]bool{}
		for _, value := range values {
			if processed[value] {
				continue
			}
			processed[value] = true
			if removeFrom[value] {
				delete(removeFrom, value)
			} else {
				addTo[value] = true
			}
		}
	}

	combine(adds1, nil, addSet)
	combine(removes1, addSet, removeSet)
	combine(adds2, removeSet, addSet)
	combine(removes2, addSet, removeSet)

	for value := range addSet {
		adds = append(adds, value)
	}
	for value := range removeSet {
		removes = append(removes, value)
	}
	return
}

// lookupStateEvents looks up the state events that are added by a new event.
func (s *OutputRoomEvent) lookupStateEvents(
	addsStateEventIDs []string, event gomatrixserverlib.Event,
) ([]gomatrixserverlib.Event, error) {
	// Fast path if there aren't any new state events.
	if len(addsStateEventIDs) == 0 {
		return nil, nil
	}

	// Fast path if the only state event added is the event itself.
	if len(addsStateEventIDs) == 1 && addsStateEventIDs[0] == event.EventID() {
		return []gomatrixserverlib.Event{event}, nil
	}

	missing := addsStateEventIDs
	var result []gomatrixserverlib.Event

	// Check if event itself is being added.
	for _, eventID := range missing {
		if eventID == event.EventID() {
			result = append(result, event)
			break
		}
	}
	missing = missingEventsFrom(result, addsStateEventIDs)

	if len(missing) == 0 {
		return result, nil
	}

	// At this point the missing events are neither the event itself nor are
	// they present in our local database. Our only option is to fetch them
	// from the roomserver using the query API.
	eventReq := api.QueryEventsByIDRequest{EventIDs: missing}
	var eventResp api.QueryEventsByIDResponse
	if err := s.query.QueryEventsByID(&eventReq, &eventResp); err != nil {
		return nil, err
	}

	result = append(result, eventResp.Events...)
	missing = missingEventsFrom(result, addsStateEventIDs)

	if len(missing) != 0 {
		return nil, fmt.Errorf(
			"missing %d state events IDs at event %q", len(missing), event.EventID(),
		)
	}

	return result, nil
}

func missingEventsFrom(events []gomatrixserverlib.Event, required []string) []string {
	have := map[string]bool{}
	for _, event := range events {
		have[event.EventID()] = true
	}
	var missing []string
	for _, eventID := range required {
		if !have[eventID] {
			missing = append(missing, eventID)
		}
	}
	return missing
}
