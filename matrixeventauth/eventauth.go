package matrixeventauth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//
type Event struct {
	RoomID     string              `json:"room_id"`
	EventID    string              `json:"event_id"`
	Sender     string              `json:"sender"`
	Type       string              `json:"type"`
	StateKey   *string             `json:"state_key"`
	Content    json.RawMessage     `json:"content"`
	PrevEvents [][]json.RawMessage `json:"prev_events"`
	Redacts    string              `json:"redacts"`
}

// StateNeeded lists the state entries needed to authenticate an event.
type StateNeeded struct {
	// Is the m.room.create event needed to auth the event.
	Create bool
	// Is the m.room.join_rules event needed to auth the event.
	JoinRules bool
	// Is the m.room.power_levels event needed to auth the event.
	PowerLevels bool
	// List of m.room.member state_keys needed to auth the event
	Member []string
	// List of m.room.third_party_invite state_keys
	ThirdPartyInvite []string
}

func StateNeededForAuth(events []Event) (result StateNeeded) {
	var members []string
	var thirdpartyinvites []string

	for _, event := range events {
		switch event.Type {
		case "m.room.create":
			// The create event doesn't require any state to authenticate.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L123
			// All other events need the create event.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L128
		case "m.room.aliases":
			// Alias events need no further authentication.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L160
			result.Create = true
		case "m.room.member":
			// Member events need the previous membership of the target.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L355
			// The current membership state of the sender.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L348
			// The join rules for the room.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L361
			// The power levels for the room.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L370
			// And optionally may require a m.third_party_invite event
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L393
			result.Create = true
			result.PowerLevels = true
			result.JoinRules = true
			members = append(members, event.Sender, event.StateKey)
			thirdpartyinvites = needsThirdpartyInvite(thirdpartyinvites, event)
		default:
			// All other events need the membership of the sender.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L177
			// The power levels for the room.
			// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L196
			result.Create = true
			result.PowerLevels = true
			members = append(members, event.Sender)
		}
	}

	// Deduplicate the state keys.
	sort.Strings(members)
	result.Member = members[:unique(sort.StringSlice(members))]
	sort.Strings(thirdpartyinvites)
	result.ThirdPartyInvite = thirdpartyinvites[:unique(sort.StringSlice(thirdpartyinvites))]
	return
}

type AuthEvents interface {
	Create() (*Event, error)
	JoinRules() (*Event, error)
	PowerLevels() (*Event, error)
	Member(stateKey string) (*Event, error)
	ThirdPartyInvite(stateKey string) (*Event, error)
}

type NotAllowed struct {
	Message string
}

func (a *NotAllowed) Error() string {
	return "matrixeventauth: " + a.Message
}

func errorf(message string, args ...interface{}) error {
	return &NotAllowed{Message: fmt.Sprintf(message, args...)}
}

func Allowed(event Event, authEvents AuthEvents) error {
	switch event.Type {
	case "m.room.create":
		return createEventAllowed(event, authEvents)
	case "m.room.alias":
		return aliasEventAllowed(event, authEvents)
	case "m.room.member":
		return memberEventAllowed(event, authEvents)
	case "m.room.power_levels":
		return powerLevelsEventAllowed(event, authEvents)
	case "m.room.redact":
		return redactEventAllowed(event, authEvents)
	default:
		return defaultEventAllowed(event, authEvents)
	}
}

func createEventAllowed(event Event, authEvents AuthEvents) error {
	roomIDDomain, err := domainFromID(event.RoomID)
	if err != nil {
		return err
	}
	senderDomain, err := domainFromID(event.Sender)
	if err != nil {
		return err
	}
	if senderDomain != roomIDDomain {
		return errorf("create event room ID domain does not match sender: %q != %q", roomIDDomain, senderDomain)
	}
	if len(event.PrevEvents) > 0 {
		return errorf("create event must be the first event in the room")
	}
	return nil
}

func aliasEventAllowed(event Event, authEvents AuthEvents) error {
	var create createContent
	senderDomain, err := domainFromID(event.Sender)
	if err != nil {
		return err
	}
	create, err := createEvent(authEvents)
	if err != nil {
		return err
	}
	if err := create.domainAllowed(senderDomain); err != nil {
		return err
	}
	if event.StateKey == nil {
		return errorf("alias must be a state event")
	}
	if senderDomain != *event.StateKey {
		return errorf("alias state_key does not match sender domain, %q != %q", senderDomain, *event.StateKey)
	}
	return nil
}

func memberEventAllowed(event Event, authEvents AuthEvents) error {
	var create createContent
	var newMember memberContent
	if err := create.load(authEvents); err != nil {
		return err
	}
	if err := create.idAllowed(event.Sender); err != nil {
		return err
	}
	if err := newMember.parse(&event); err != nil {
		return err
	}
	if event.StateKey == nil {
		return errorf("member must be a state event")
	}
	targetUserID := *event.StateKey

	if len(event.PrevEvents) == 1 &&
		newMember.Membership == "join" &&
		create.Creator == targetUserID &&
		event.Sender == targetUserID {
		// Special case the first join event in the room.
		// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L328
		if len(event.PrevEvents[0]) != 2 {
			return errorf("unparsable prev event")
		}
		var prevEventID string
		if err := json.Unmarshal(PrevEvents[0][0], &prevEventID); err != nil {
			return errorf("unparsable prev event")
		}
		if prevEventID == create.eventID {
			// If this is the room creator joining the room directly after the
			// the create event, then allow.
			return nil
		}
		// Otherwise fall through to the usual authentication process.
	}

	if err := create.idAllowed(targetUserID); err != nil {
		return err
	}

	if newMembership == "invite" && thirdPartyInvite != nil {
		// Special case third party invites
		// https://github.com/matrix-org/synapse/blob/v0.18.5/synapse/api/auth.py#L393
		panic(fmt.Errorf("ThirdPartyInvite not implemented"))

		// Otherwise fall through to the usual authentication process.
	}

	var m membershipAllower
	if err = m.setup(&event, authEvents); err != nil {
		return err
	}
	return m.membershipAllowed()
}

type membershipAllower struct {
	targetID     string
	senderID     string
	senderMember memberContent
	oldMember    memberContent
	newMember    memberContent
	joinRule     joinRuleContent
	create       createContent
	powerLevels  powerLevelContent
}

func (m *membershipAllower) setup(event *Event, authEvents AuthEvents) error {
	m.targetID = *event.StateKey
	m.senderID = event.Sender
	if err := m.senderMembership.load(authEvents, m.senderID); err != nil {
		return err
	}
	if err := m.oldMembership.load(authEvents, m.targetID); err != nil {
		return err
	}
	if err := m.newMembership.parse(event); err != nil {
		return err
	}
	if err := m.create.load(authEvents); err != nil {
		return err
	}
	if err := m.powerLevels.load(authEvents, m.create.Creator); err != nil {
		return err
	}
	if err := m.joinRule.load(authEvents); err != nil {
		return err
	}
	return nil
}

// membershipAllowed determines whether the membership change is allowed.
func (m *membershipAllower) membershipAllowed() error {
	if m.targetID == m.SenderID {
		return m.membershipAllowedSelf()
	}
	return m.membershipAllowedOther()
}

func (m *membershipAllower) membershipAllowedSelf() error {
	if m.newMember.Membership == "join" {
		// A user that is not in the room is allowed to join if the room
		// join rules are "public".
		if m.oldMember.Membership == "leave" && m.joinRule.JoinRule == "public" {
			return nil
		}
		// An invited user is allowed to join if the join rules are "public"
		if m.oldMember.Membership == "invite" && m.joinRule.JoinRule == "public" {
			return nil
		}
		// An invited user is allowed to join if the join rules are "invite"
		if m.oldMember.Membership == "invite" && m.joinRule.JoinRule == "invite" {
			return nil
		}
		// A joined user is allowed to update their join.
		if m.oldMember.Membership == "join" {
			return nil
		}
	}
	if m.newMember.Membership == "leave" {
		// A joined user is allowed to leave the room.
		if m.oldMember.Membership == "join" {
			return nil
		}
		// An invited user is allowed to reject an invite.
		if m.oldMember.Membership == "invite" {
			return nil
		}
	}
	return m.membershipFailed()
}

func (m *membershipAllower) membershipAllowedOther() error {
	senderLevel := m.powerLevels.userLevel(m.SenderID)
	targetLevel := m.powerLevels.userLevel(m.TargetID)

	// You may only modify the membership of another user if you are in the room.
	if m.senderMember.Membership == "join" {
		if m.newMember.Membership == "ban" {
			// A user may ban another user if their level is high enough
			if senderLevel >= m.powerLevels.banLevel &&
				senderLevel > targetLevel {
				return nil
			}
		}
		if m.newMember.Membership == "leave" {
			// A user may unban another user if their level is high enough.
			if m.oldMembership == "ban" && senderLevel >= powerLevels.banLevel {
				return nil
			}
			// A user may kick another user if their level is high enough.
			if m.oldMember.Membership != "ban" &&
				senderLevel >= powerLevels.kickLevel &&
				senderLevel > targetLevel {
				return nil
			}
		}
		if m.newMember.Membership == "invite" {
			// A user may invite another user if the user has left the room.
			// and their level is high enough.
			if m.oldMember.Membership == "leave" && senderLevel >= m.powerLevels.inviteLevel {
				return nil
			}
			// A user may re-invite a user.
			if m.oldMember.Membership == "invite" && senderLevel >= m.powerLevels.inviteLevel {
				return nil
			}
		}
	}

	return m.membershipFailed()
}

// membershipFailed returns a error explaining why the membership change was disallowed.
func (m *membershipAllower) membershipFailed() error {
	if m.senderID == m.targetID {
		return errorf(
			"%q is not allowed to change their membership from %q to %q",
			m.targetID, m.oldMember.Membership, m.newMember.Membership,
		)
	}

	if m.senderMember.Membership != "join" {
		return errorf("sender %q is not in the room", m.senderID)
	}

	return errorf(
		"%q is not allowed to change the membership of %q from %q to %q",
		m.senderID, m.targetID, m.oldMember.Membership, m.newMember.Membership,
	)
}

func powerLevelEventAllowed(event Event, authEvents AuthEvents) error {
	var allower eventAllower
	if err := allower.setup(authEvents); err != nil {
		return err
	}
	if err := allower.commonChecks(); err != nil {
		return err
	}

	oldPowerLevels := allower.powerlevels
	var newPowerLevels powerLevelContent
	if err := newPowerLevels.parse(&event); err != nil {
		return err
	}

	for userID := range newPowerLevels.userLevels {
		if !isValidUserID(userID) {
			return errorf("Not a valid user ID: %q", userID)
		}
	}

	type levelPair struct {
		old int64
		new int64
	}

	levelChecks := []levelPair{
		{oldPowerLevels.banLevel, newPowerLevels.banLevel},
		{oldPowerLevels.inviteLevel, newPowerLevels.inviteLevel},
		{oldPowerLevels.kickLevel, newPowerLevels.kickLevel},
		{oldPowerLevels.redactLevel, newPowerLevels.redactLevel},
		{oldPowerLevels.stateDefaultLevel, newPowerLevels.stateDefaultLevel},
		{oldPowerLevels.eventDefaultLevel, newPowerLevels.eventDefaultLevel},
	}

	for eventType := range newPowerLevels.eventLevels {
		levelChecks := append(levelChecks, levelPair{
			oldPowerLevels.eventLevel(eventType, nil), newPowerLevels.eventLevel(eventType, nil),
		})
	}

	for eventType := range oldPowerLevels.eventLevels {
		levelChecks := append(levelChecks, levelPair{
			oldPowerLevels.eventLevel(eventType, nil), newPowerLevels.eventLevel(eventType, nil),
		})
	}

	for _, level := range levelChecks {
		if level.old != level.new {
			if senderLevel < level.old || senderLevel < level.new {
				return errorf(
					"sender with level %d is not allowed to change level from %d to %d",
					senderLevel, level.old, level.new,
				)
			}
		}
	}

	userLevelChecks := []levelPair{
		{oldPowerLevels.userDefaultLevel, newPowerLevels.userDefaultLevel},
	}

	for userID := range newPowerLevels.userLevels {
		userLevelChecks := append(levelChecks, levelPair{
			oldPowerLevels.userLevel(userID), newPowerLevels.userLevel(userID),
		})
	}

	for userID := range newPowerLevels.userLevels {
		userLevelChecks := append(levelChecks, levelPair{
			oldPowerLevels.userLevel(userID), newPowerLevels.userLevel(userID),
		})
	}

	for _, level := range levelChecks {
		if level.old != level.new {
			if senderLevel <= level.old || senderLevel < level.new {
				return errorf(
					"sender with level %d is not allowed to change user level from %d to %d",
					senderLevel, level.old, level.new,
				)
			}
		}
	}

	return nil
}

func isValidUserID(userID string) bool {
	return userID[0] == '@' && strings.IndexByte(userID, ':') != -1
}

func redactEventAllowed(event Event, authEvents AuthEvents) error {
	var allower eventAllower
	if err := allower.setup(authEvents, event.Sender); err != nil {
		return err
	}

	if err := allower.commonChecks(); err != nil {
		return err
	}

	senderDomain, err := domainFromID(event.Sender)
	if err != nil {
		return err
	}

	redactDomain, err := domainFromID(event.Redacts)
	if err != nil {
		return err
	}

	// Servers are always allowed to redact their own messages.
	if senderDomain == redactDomain {
		return nil
	}

	senderLevel = allower.powerlevels.userLevel(event.Sender)
	redactLevel = allower.powerlevels.redactLevel

	// Otherwise the sender must have enough power.
	if senderLevel >= redactLevel {
		return nil
	}

	return errorf(
		"%q is not allowed to react message from %q. %d < %d",
		event.Sender, redactDomain, senderLevel, redactLevel,
	)
}

func defaultEventAllowed(event Event, authEvents AuthEvents) error {
	var allower eventAllower
	if err := allower.setup(authEvents, event.Sender); err != nil {
		return err
	}

	return allower.commonChecks()
}

type eventAllower struct {
	create      createContent
	member      memberContent
	powerlevels powerLevelContent
}

func (e *eventAllower) setup(authEvents AuthEvents, senderID string) error {
	if err := e.create.load(authEvents); err != nil {
		return err
	}
	if err := e.member.load(authEvents, senderID); err != nil {
		return err
	}
	if err := e.powerLevels.load(authEvents, e.create.Creator); err != nil {
		return err
	}
	return nil
}

func (e *eventAllower) defaultAllowed(event Event, authEvents AuthEvents) error {
	if err := e.create.idAllowed(event.Sender); err != nil {
		return err
	}

	if e.member.Membership != "join" {
		return errof("sender %q not in room", event.Sender)
	}

	senderLevel := e.powerLevels.userLevel(event.Sender)
	eventLevel := e.powerLevels.eventLevel(event.Type, event.StateKey)
	if senderLevel < eventLevel {
		return errorf(
			"sender %q is not allowed to send event. %d < %d",
			event.Sender, senderLevel, eventLevel,
		)
	}

	if event.StateKey != nil && len(event.StateKey) > 0 && event.StateKey[0] == "@" {
		if *event.StateKey != event.Sender {
			return errorf(
				"sender %q is not allowed to modify the state belonging to %q",
				event.Sender, *event.StateKey,
			)
		}
	}

	return nil
}

// Remove duplicate items from a sorted list.
// Takes the same interface as sort.Sort
// Returns the length of the date without duplicates
// Uses the last occurance of a duplicate.
// O(n).
func unique(data sort.Interface) int {
	length := data.Len()
	j := 0
	for i := 1; i < length; i++ {
		if data.Less(i-1, i) {
			data.Swap(i-1, j)
			j++
		}
	}
	data.Swap(length-1, j)
	return j + 1
}

func needsThirdpartyInvite(thirdpartyinvites []string, event Event) []string {
	var content struct {
		ThirdPartyInvite struct {
			Signed struct {
				Token string `json:"token"`
			} `json:"signed"`
		} `json:"third_party_invite"`
	}

	if err := json.Unmarshal(event.Content, &content); err != nil {
		// If the unmarshal fails then ignore the contents.
		// The event will be rejected by the auth checks when it fails to
		// unmarshal without needing to check for a third party invite token.
		return thirdpartyinvites
	}

	if content.ThirdPartyInvite.Signed.Token != "" {
		return append(thirdpartyinvites, content.ThirdPartyInvite.Signed.Token)
	}

	return thirdpartyinvites
}