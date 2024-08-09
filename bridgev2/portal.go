// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exfmt"
	"go.mau.fi/util/exslices"
	"go.mau.fi/util/ptr"
	"go.mau.fi/util/variationselector"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type portalMatrixEvent struct {
	evt    *event.Event
	sender *User
}

type portalRemoteEvent struct {
	evt    RemoteEvent
	source *UserLogin
}

type portalCreateEvent struct {
	ctx    context.Context
	source *UserLogin
	info   *ChatInfo
	cb     func(error)
}

func (pme *portalMatrixEvent) isPortalEvent() {}
func (pre *portalRemoteEvent) isPortalEvent() {}
func (pre *portalCreateEvent) isPortalEvent() {}

type portalEvent interface {
	isPortalEvent()
}

type outgoingMessage struct {
	db     *database.Message
	evt    *event.Event
	handle func(RemoteMessage, *database.Message) (bool, error)
}

type Portal struct {
	*database.Portal
	Bridge *Bridge
	Log    zerolog.Logger
	Parent *Portal
	Relay  *UserLogin

	currentlyTyping       []id.UserID
	currentlyTypingLogins map[id.UserID]*UserLogin
	currentlyTypingLock   sync.Mutex

	outgoingMessages     map[networkid.TransactionID]outgoingMessage
	outgoingMessagesLock sync.Mutex

	roomCreateLock sync.Mutex

	events chan portalEvent
}

const PortalEventBuffer = 64

func (br *Bridge) loadPortal(ctx context.Context, dbPortal *database.Portal, queryErr error, key *networkid.PortalKey) (*Portal, error) {
	if queryErr != nil {
		return nil, fmt.Errorf("failed to query db: %w", queryErr)
	}
	if dbPortal == nil {
		if key == nil {
			return nil, nil
		}
		dbPortal = &database.Portal{
			BridgeID:  br.ID,
			PortalKey: *key,
		}
		err := br.DB.Portal.Insert(ctx, dbPortal)
		if err != nil {
			return nil, fmt.Errorf("failed to insert new portal: %w", err)
		}
	}
	portal := &Portal{
		Portal: dbPortal,
		Bridge: br,

		events:                make(chan portalEvent, PortalEventBuffer),
		currentlyTypingLogins: make(map[id.UserID]*UserLogin),
		outgoingMessages:      make(map[networkid.TransactionID]outgoingMessage),
	}
	br.portalsByKey[portal.PortalKey] = portal
	if portal.MXID != "" {
		br.portalsByMXID[portal.MXID] = portal
	}
	var err error
	if portal.ParentID != "" {
		portal.Parent, err = br.UnlockedGetPortalByKey(ctx, networkid.PortalKey{ID: portal.ParentID}, false)
		if err != nil {
			return nil, fmt.Errorf("failed to load parent portal (%s): %w", portal.ParentID, err)
		}
	}
	if portal.RelayLoginID != "" {
		portal.Relay, err = br.unlockedGetExistingUserLoginByID(ctx, portal.RelayLoginID)
		if err != nil {
			return nil, fmt.Errorf("failed to load relay login (%s): %w", portal.RelayLoginID, err)
		}
	}
	portal.updateLogger()
	go portal.eventLoop()
	return portal, nil
}

func (portal *Portal) updateLogger() {
	logWith := portal.Bridge.Log.With().Str("portal_id", string(portal.ID))
	if portal.MXID != "" {
		logWith = logWith.Stringer("portal_mxid", portal.MXID)
	}
	portal.Log = logWith.Logger()
}

func (br *Bridge) loadManyPortals(ctx context.Context, portals []*database.Portal) ([]*Portal, error) {
	output := make([]*Portal, 0, len(portals))
	for _, dbPortal := range portals {
		if cached, ok := br.portalsByKey[dbPortal.PortalKey]; ok {
			output = append(output, cached)
		} else {
			loaded, err := br.loadPortal(ctx, dbPortal, nil, nil)
			if err != nil {
				return nil, err
			} else if loaded != nil {
				output = append(output, loaded)
			}
		}
	}
	return output, nil
}

func (br *Bridge) UnlockedGetPortalByKey(ctx context.Context, key networkid.PortalKey, onlyIfExists bool) (*Portal, error) {
	cached, ok := br.portalsByKey[key]
	if ok {
		return cached, nil
	}
	keyPtr := &key
	if onlyIfExists {
		keyPtr = nil
	}
	db, err := br.DB.Portal.GetByKey(ctx, key)
	return br.loadPortal(ctx, db, err, keyPtr)
}

func (br *Bridge) FindPortalReceiver(ctx context.Context, id networkid.PortalID, maybeReceiver networkid.UserLoginID) (networkid.PortalKey, error) {
	key := br.FindCachedPortalReceiver(id, maybeReceiver)
	if !key.IsEmpty() {
		return key, nil
	}
	key, err := br.DB.Portal.FindReceiver(ctx, id, maybeReceiver)
	if err != nil {
		return networkid.PortalKey{}, err
	}
	return key, nil
}

func (br *Bridge) FindCachedPortalReceiver(id networkid.PortalID, maybeReceiver networkid.UserLoginID) networkid.PortalKey {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	portal, ok := br.portalsByKey[networkid.PortalKey{
		ID:       id,
		Receiver: maybeReceiver,
	}]
	if ok {
		return portal.PortalKey
	}
	portal, ok = br.portalsByKey[networkid.PortalKey{ID: id}]
	if ok {
		return portal.PortalKey
	}
	return networkid.PortalKey{}
}

func (br *Bridge) GetPortalByMXID(ctx context.Context, mxid id.RoomID) (*Portal, error) {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	cached, ok := br.portalsByMXID[mxid]
	if ok {
		return cached, nil
	}
	db, err := br.DB.Portal.GetByMXID(ctx, mxid)
	return br.loadPortal(ctx, db, err, nil)
}

func (br *Bridge) GetAllPortalsWithMXID(ctx context.Context) ([]*Portal, error) {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	rows, err := br.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return nil, err
	}
	return br.loadManyPortals(ctx, rows)
}

func (br *Bridge) GetAllPortals(ctx context.Context) ([]*Portal, error) {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	rows, err := br.DB.Portal.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	return br.loadManyPortals(ctx, rows)
}

func (br *Bridge) GetDMPortalsWith(ctx context.Context, otherUserID networkid.UserID) ([]*Portal, error) {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	rows, err := br.DB.Portal.GetAllDMsWith(ctx, otherUserID)
	if err != nil {
		return nil, err
	}
	return br.loadManyPortals(ctx, rows)
}

func (br *Bridge) GetPortalByKey(ctx context.Context, key networkid.PortalKey) (*Portal, error) {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	return br.UnlockedGetPortalByKey(ctx, key, false)
}

func (br *Bridge) GetExistingPortalByKey(ctx context.Context, key networkid.PortalKey) (*Portal, error) {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	if key.Receiver == "" {
		return br.UnlockedGetPortalByKey(ctx, key, true)
	}
	cached, ok := br.portalsByKey[key]
	if ok {
		return cached, nil
	}
	cached, ok = br.portalsByKey[networkid.PortalKey{ID: key.ID}]
	if ok {
		return cached, nil
	}
	db, err := br.DB.Portal.GetByIDWithUncertainReceiver(ctx, key)
	return br.loadPortal(ctx, db, err, nil)
}

func (portal *Portal) queueEvent(ctx context.Context, evt portalEvent) {
	select {
	case portal.events <- evt:
	default:
		zerolog.Ctx(ctx).Error().
			Str("portal_id", string(portal.ID)).
			Msg("Portal event channel is full")
	}
}

func (portal *Portal) eventLoop() {
	for rawEvt := range portal.events {
		switch evt := rawEvt.(type) {
		case *portalMatrixEvent:
			portal.handleMatrixEvent(evt.sender, evt.evt)
		case *portalRemoteEvent:
			portal.handleRemoteEvent(evt.source, evt.evt)
		case *portalCreateEvent:
			portal.handleCreateEvent(evt)
		default:
			panic(fmt.Errorf("illegal type %T in eventLoop", evt))
		}
	}
}

func (portal *Portal) handleCreateEvent(evt *portalCreateEvent) {
	defer func() {
		if err := recover(); err != nil {
			logEvt := zerolog.Ctx(evt.ctx).Error()
			if realErr, ok := err.(error); ok {
				logEvt = logEvt.Err(realErr)
			} else {
				logEvt = logEvt.Any(zerolog.ErrorFieldName, err)
			}
			logEvt.
				Bytes("stack", debug.Stack()).
				Msg("Portal creation panicked")
			evt.cb(fmt.Errorf("portal creation panicked"))
		}
	}()
	evt.cb(portal.createMatrixRoomInLoop(evt.ctx, evt.source, evt.info))
}

func (portal *Portal) FindPreferredLogin(ctx context.Context, user *User, allowRelay bool) (*UserLogin, *database.UserPortal, error) {
	if portal.Receiver != "" {
		login, err := portal.Bridge.GetExistingUserLoginByID(ctx, portal.Receiver)
		if err != nil {
			return nil, nil, err
		}
		if login.UserMXID != user.MXID {
			if allowRelay && portal.Relay != nil {
				return nil, nil, nil
			}
			// TODO different error for this case?
			return nil, nil, ErrNotLoggedIn
		}
		up, err := portal.Bridge.DB.UserPortal.Get(ctx, login.UserLogin, portal.PortalKey)
		return login, up, err
	}
	logins, err := portal.Bridge.DB.UserPortal.GetAllForUserInPortal(ctx, user.MXID, portal.PortalKey)
	if err != nil {
		return nil, nil, err
	}
	portal.Bridge.cacheLock.Lock()
	defer portal.Bridge.cacheLock.Unlock()
	for i, up := range logins {
		login, ok := user.logins[up.LoginID]
		if ok && login.Client != nil && (len(logins) == i-1 || login.Client.IsLoggedIn()) {
			return login, up, nil
		}
	}
	if !allowRelay {
		return nil, nil, ErrNotLoggedIn
	}
	// Portal has relay, use it
	if portal.Relay != nil {
		return nil, nil, nil
	}
	var firstLogin *UserLogin
	for _, login := range user.logins {
		firstLogin = login
		break
	}
	if firstLogin != nil {
		zerolog.Ctx(ctx).Warn().
			Str("chosen_login_id", string(firstLogin.ID)).
			Msg("No usable user portal rows found, returning random login")
		return firstLogin, nil, nil
	} else {
		return nil, nil, ErrNotLoggedIn
	}
}

func (portal *Portal) sendSuccessStatus(ctx context.Context, evt *event.Event) {
	portal.Bridge.Matrix.SendMessageStatus(ctx, &MessageStatus{Status: event.MessageStatusSuccess}, StatusEventInfoFromEvent(evt))
}

func (portal *Portal) sendErrorStatus(ctx context.Context, evt *event.Event, err error) {
	status := WrapErrorInStatus(err)
	if status.Status == "" {
		status.Status = event.MessageStatusRetriable
	}
	if status.ErrorReason == "" {
		status.ErrorReason = event.MessageStatusGenericError
	}
	if status.InternalError == nil {
		status.InternalError = err
	}
	portal.Bridge.Matrix.SendMessageStatus(ctx, &status, StatusEventInfoFromEvent(evt))
}

func (portal *Portal) checkConfusableName(ctx context.Context, userID id.UserID, name string) bool {
	conn, ok := portal.Bridge.Matrix.(MatrixConnectorWithNameDisambiguation)
	if !ok {
		return false
	}
	confusableWith, err := conn.IsConfusableName(ctx, portal.MXID, userID, name)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to check if name is confusable")
		return true
	}
	for _, confusable := range confusableWith {
		// Don't disambiguate names that only conflict with ghosts of this bridge
		if !portal.Bridge.IsGhostMXID(confusable) {
			return true
		}
	}
	return false
}

func (portal *Portal) handleMatrixEvent(sender *User, evt *event.Event) {
	log := portal.Log.With().
		Str("action", "handle matrix event").
		Str("event_type", evt.Type.Type).
		Logger()
	ctx := log.WithContext(context.TODO())
	defer func() {
		if err := recover(); err != nil {
			logEvt := log.Error()
			if realErr, ok := err.(error); ok {
				logEvt = logEvt.Err(realErr)
			} else {
				logEvt = logEvt.Any(zerolog.ErrorFieldName, err)
			}
			logEvt.
				Bytes("stack", debug.Stack()).
				Msg("Matrix event handler panicked")
			if evt.ID != "" {
				go portal.sendErrorStatus(ctx, evt, ErrPanicInEventHandler)
			}
		}
	}()
	if evt.Mautrix.EventSource&event.SourceEphemeral != 0 {
		switch evt.Type {
		case event.EphemeralEventReceipt:
			portal.handleMatrixReceipts(ctx, evt)
		case event.EphemeralEventTyping:
			portal.handleMatrixTyping(ctx, evt)
		}
		return
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.
			Stringer("event_id", evt.ID).
			Stringer("sender", sender.MXID)
	})
	login, _, err := portal.FindPreferredLogin(ctx, sender, true)
	if err != nil {
		log.Err(err).Msg("Failed to get user login to handle Matrix event")
		if errors.Is(err, ErrNotLoggedIn) {
			portal.sendErrorStatus(ctx, evt, WrapErrorInStatus(err).WithMessage("You're not logged in").WithIsCertain(true).WithSendNotice(true))
		} else {
			portal.sendErrorStatus(ctx, evt, WrapErrorInStatus(err).WithMessage("Failed to get login to handle event").WithIsCertain(true).WithSendNotice(true))
		}
		return
	}
	var origSender *OrigSender
	if login == nil {
		login = portal.Relay
		origSender = &OrigSender{
			User:   sender,
			UserID: sender.MXID,
		}

		memberInfo, err := portal.Bridge.Matrix.GetMemberInfo(ctx, portal.MXID, sender.MXID)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to get member info for user being relayed")
		} else if memberInfo != nil {
			origSender.MemberEventContent = *memberInfo
			if memberInfo.Displayname == "" {
				origSender.DisambiguatedName = sender.MXID.String()
			} else if origSender.RequiresDisambiguation = portal.checkConfusableName(ctx, sender.MXID, memberInfo.Displayname); origSender.RequiresDisambiguation {
				origSender.DisambiguatedName = fmt.Sprintf("%s (%s)", memberInfo.Displayname, sender.MXID)
			} else {
				origSender.DisambiguatedName = memberInfo.Displayname
			}
		} else {
			origSender.DisambiguatedName = sender.MXID.String()
		}
		origSender.FormattedName = portal.Bridge.Config.Relay.FormatName(origSender)
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("login_id", string(login.ID))
	})
	switch evt.Type {
	case event.EventMessage, event.EventSticker:
		portal.handleMatrixMessage(ctx, login, origSender, evt)
	case event.EventReaction:
		if origSender != nil {
			log.Debug().Msg("Ignoring reaction event from relayed user")
			portal.sendErrorStatus(ctx, evt, ErrIgnoringReactionFromRelayedUser)
			return
		}
		portal.handleMatrixReaction(ctx, login, evt)
	case event.EventRedaction:
		portal.handleMatrixRedaction(ctx, login, origSender, evt)
	case event.StateRoomName:
		handleMatrixRoomMeta(portal, ctx, login, origSender, evt, RoomNameHandlingNetworkAPI.HandleMatrixRoomName)
	case event.StateTopic:
		handleMatrixRoomMeta(portal, ctx, login, origSender, evt, RoomTopicHandlingNetworkAPI.HandleMatrixRoomTopic)
	case event.StateRoomAvatar:
		handleMatrixRoomMeta(portal, ctx, login, origSender, evt, RoomAvatarHandlingNetworkAPI.HandleMatrixRoomAvatar)
	case event.StateEncryption:
		// TODO?
	case event.AccountDataMarkedUnread:
		handleMatrixAccountData(portal, ctx, login, evt, MarkedUnreadHandlingNetworkAPI.HandleMarkedUnread)
	case event.AccountDataRoomTags:
		handleMatrixAccountData(portal, ctx, login, evt, TagHandlingNetworkAPI.HandleRoomTag)
	case event.AccountDataBeeperMute:
		handleMatrixAccountData(portal, ctx, login, evt, MuteHandlingNetworkAPI.HandleMute)
	case event.StateMember:
		portal.handleMatrixMembership(ctx, login, origSender, evt)
	}
}

func (portal *Portal) handleMatrixReceipts(ctx context.Context, evt *event.Event) {
	content, ok := evt.Content.Parsed.(*event.ReceiptEventContent)
	if !ok {
		return
	}
	for evtID, receipts := range *content {
		readReceipts, ok := receipts[event.ReceiptTypeRead]
		if !ok {
			continue
		}
		for userID, receipt := range readReceipts {
			sender, err := portal.Bridge.GetUserByMXID(ctx, userID)
			if err != nil {
				// TODO log
				return
			}
			portal.handleMatrixReadReceipt(ctx, sender, evtID, receipt)
		}
	}
}

func (portal *Portal) handleMatrixReadReceipt(ctx context.Context, user *User, eventID id.EventID, receipt event.ReadReceipt) {
	log := zerolog.Ctx(ctx)
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.
			Stringer("event_id", eventID).
			Stringer("user_id", user.MXID).
			Stringer("receipt_ts", receipt.Timestamp)
	})
	login, userPortal, err := portal.FindPreferredLogin(ctx, user, false)
	if err != nil {
		if !errors.Is(err, ErrNotLoggedIn) {
			log.Err(err).Msg("Failed to get preferred login for user")
		}
		return
	} else if login == nil {
		return
	}
	rrClient, ok := login.Client.(ReadReceiptHandlingNetworkAPI)
	if !ok {
		return
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("user_login_id", string(login.ID))
	})
	evt := &MatrixReadReceipt{
		Portal:  portal,
		EventID: eventID,
		Receipt: receipt,
	}
	if userPortal == nil {
		userPortal = database.UserPortalFor(login.UserLogin, portal.PortalKey)
	} else {
		userPortal = userPortal.CopyWithoutValues()
		evt.LastRead = userPortal.LastRead
	}
	evt.ExactMessage, err = portal.Bridge.DB.Message.GetPartByMXID(ctx, eventID)
	if err != nil {
		log.Err(err).Msg("Failed to get exact message from database")
	} else if evt.ExactMessage != nil {
		log.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("exact_message_id", string(evt.ExactMessage.ID)).Time("exact_message_ts", evt.ExactMessage.Timestamp)
		})
		evt.ReadUpTo = evt.ExactMessage.Timestamp
	} else {
		evt.ReadUpTo = receipt.Timestamp
	}
	err = rrClient.HandleMatrixReadReceipt(ctx, evt)
	if err != nil {
		log.Err(err).Msg("Failed to handle read receipt")
		return
	}
	if evt.ExactMessage != nil {
		userPortal.LastRead = evt.ExactMessage.Timestamp
	} else {
		userPortal.LastRead = receipt.Timestamp
	}
	err = portal.Bridge.DB.UserPortal.Put(ctx, userPortal)
	if err != nil {
		log.Err(err).Msg("Failed to save user portal metadata")
	}
	portal.Bridge.DisappearLoop.StartAll(ctx, portal.MXID)
}

func (portal *Portal) handleMatrixTyping(ctx context.Context, evt *event.Event) {
	content, ok := evt.Content.Parsed.(*event.TypingEventContent)
	if !ok {
		return
	}
	portal.currentlyTypingLock.Lock()
	defer portal.currentlyTypingLock.Unlock()
	slices.Sort(content.UserIDs)
	stoppedTyping, startedTyping := exslices.SortedDiff(portal.currentlyTyping, content.UserIDs, func(a, b id.UserID) int {
		return strings.Compare(string(a), string(b))
	})
	portal.sendTypings(ctx, stoppedTyping, false)
	portal.sendTypings(ctx, startedTyping, true)
	portal.currentlyTyping = content.UserIDs
}

func (portal *Portal) sendTypings(ctx context.Context, userIDs []id.UserID, typing bool) {
	for _, userID := range userIDs {
		login, ok := portal.currentlyTypingLogins[userID]
		if !ok && !typing {
			continue
		} else if !ok {
			user, err := portal.Bridge.GetUserByMXID(ctx, userID)
			if err != nil {
				zerolog.Ctx(ctx).Err(err).Stringer("user_id", userID).Msg("Failed to get user to send typing event")
				continue
			} else if user == nil {
				continue
			}
			login, _, err = portal.FindPreferredLogin(ctx, user, false)
			if err != nil {
				if !errors.Is(err, ErrNotLoggedIn) {
					zerolog.Ctx(ctx).Err(err).Stringer("user_id", userID).Msg("Failed to get user login to send typing event")
				}
				continue
			} else if login == nil {
				continue
			} else if _, ok = login.Client.(TypingHandlingNetworkAPI); !ok {
				continue
			}
			portal.currentlyTypingLogins[userID] = login
		}
		if !typing {
			delete(portal.currentlyTypingLogins, userID)
		}
		typingAPI, ok := login.Client.(TypingHandlingNetworkAPI)
		if !ok {
			continue
		}
		err := typingAPI.HandleMatrixTyping(ctx, &MatrixTyping{
			Portal:   portal,
			IsTyping: typing,
			Type:     TypingTypeText,
		})
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Stringer("user_id", userID).Msg("Failed to bridge Matrix typing event")
		} else {
			zerolog.Ctx(ctx).Debug().
				Stringer("user_id", userID).
				Bool("typing", typing).
				Msg("Sent typing event")
		}
	}
}

func (portal *Portal) periodicTypingUpdater() {
	// TODO actually call this function
	log := portal.Log.With().Str("component", "typing updater").Logger()
	ctx := log.WithContext(context.Background())
	for {
		// TODO make delay configurable by network connector
		time.Sleep(5 * time.Second)
		portal.currentlyTypingLock.Lock()
		if len(portal.currentlyTyping) == 0 {
			portal.currentlyTypingLock.Unlock()
			continue
		}
		for _, userID := range portal.currentlyTyping {
			login, ok := portal.currentlyTypingLogins[userID]
			if !ok {
				continue
			}
			typingAPI, ok := login.Client.(TypingHandlingNetworkAPI)
			if !ok {
				continue
			}
			err := typingAPI.HandleMatrixTyping(ctx, &MatrixTyping{
				Portal:   portal,
				IsTyping: true,
				Type:     TypingTypeText,
			})
			if err != nil {
				log.Err(err).Stringer("user_id", userID).Msg("Failed to repeat Matrix typing event")
			} else {
				log.Debug().
					Stringer("user_id", userID).
					Bool("typing", true).
					Msg("Sent repeated typing event")
			}
		}
		portal.currentlyTypingLock.Unlock()
	}
}

func (portal *Portal) checkMessageContentCaps(ctx context.Context, caps *NetworkRoomCapabilities, content *event.MessageEventContent, evt *event.Event) bool {
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		// No checks for now, message length is safer to check after conversion inside connector
	case event.MsgLocation:
		if !caps.LocationMessages {
			portal.sendErrorStatus(ctx, evt, ErrLocationMessagesNotAllowed)
			return false
		}
	case event.MsgImage, event.MsgAudio, event.MsgVideo, event.MsgFile:
		if content.FileName != "" && content.Body != content.FileName {
			if !caps.Captions {
				portal.sendErrorStatus(ctx, evt, ErrCaptionsNotAllowed)
				return false
			}
		}
	default:
	}
	return true
}

func (portal *Portal) handleMatrixMessage(ctx context.Context, sender *UserLogin, origSender *OrigSender, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		log.Error().Type("content_type", evt.Content.Parsed).Msg("Unexpected parsed content type")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: %T", ErrUnexpectedParsedContentType, evt.Content.Parsed))
		return
	}
	caps := sender.Client.GetCapabilities(ctx, portal)

	if content.RelatesTo.GetReplaceID() != "" {
		portal.handleMatrixEdit(ctx, sender, origSender, evt, content, caps)
		return
	}
	var err error
	if origSender != nil {
		content, err = portal.Bridge.Config.Relay.FormatMessage(content, origSender)
		if err != nil {
			log.Err(err).Msg("Failed to format message for relaying")
			portal.sendErrorStatus(ctx, evt, err)
			return
		}
	}
	if !portal.checkMessageContentCaps(ctx, caps, content, evt) {
		return
	}

	var threadRoot, replyTo *database.Message
	var replyToID id.EventID
	if caps.Threads {
		replyToID = content.RelatesTo.GetNonFallbackReplyTo()
	} else {
		replyToID = content.RelatesTo.GetReplyTo()
	}
	threadRootID := content.RelatesTo.GetThreadParent()
	if caps.Threads && threadRootID != "" {
		threadRoot, err = portal.Bridge.DB.Message.GetPartByMXID(ctx, threadRootID)
		if err != nil {
			log.Err(err).Msg("Failed to get thread root message from database")
		}
	}
	if replyToID != "" && (caps.Replies || caps.Threads) {
		replyTo, err = portal.Bridge.DB.Message.GetPartByMXID(ctx, replyToID)
		if err != nil {
			log.Err(err).Msg("Failed to get reply target message from database")
		} else {
			// Support replying to threads from non-thread-capable clients.
			// The fallback happens if the message is not a Matrix thread and either
			// * the replied-to message is in a thread, or
			// * the network only supports threads (assume the user wants to start a new thread)
			if caps.Threads && threadRoot == nil && (replyTo.ThreadRoot != "" || !caps.Replies) {
				threadRootRemoteID := replyTo.ThreadRoot
				if threadRootRemoteID == "" {
					threadRootRemoteID = replyTo.ID
				}
				threadRoot, err = portal.Bridge.DB.Message.GetFirstThreadMessage(ctx, portal.PortalKey, threadRootRemoteID)
				if err != nil {
					log.Err(err).Msg("Failed to get thread root message from database (via reply fallback)")
				}
			}
			if !caps.Replies {
				replyTo = nil
			}
		}
	}

	resp, err := sender.Client.HandleMatrixMessage(ctx, &MatrixMessage{
		MatrixEventBase: MatrixEventBase[*event.MessageEventContent]{
			Event:      evt,
			Content:    content,
			OrigSender: origSender,
			Portal:     portal,
		},
		ThreadRoot: threadRoot,
		ReplyTo:    replyTo,
	})
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix message")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
	message := resp.DB
	if message.MXID == "" {
		message.MXID = evt.ID
	}
	if message.Room.ID == "" {
		message.Room = portal.PortalKey
	}
	if message.Timestamp.IsZero() {
		message.Timestamp = time.UnixMilli(evt.Timestamp)
	}
	if message.ReplyTo.MessageID == "" && replyTo != nil {
		message.ReplyTo.MessageID = replyTo.ID
		message.ReplyTo.PartID = &replyTo.PartID
	}
	if message.ThreadRoot == "" && threadRoot != nil {
		message.ThreadRoot = threadRoot.ID
		if threadRoot.ThreadRoot != "" {
			message.ThreadRoot = threadRoot.ThreadRoot
		}
	}
	if message.SenderMXID == "" {
		message.SenderMXID = evt.Sender
	}
	if resp.Pending != "" {
		// TODO if the event queue is ever removed, this will have to be done by the network connector before sending the request
		//      (for now this is fine because incoming messages will wait in the queue for this function to return)
		portal.outgoingMessagesLock.Lock()
		portal.outgoingMessages[resp.Pending] = outgoingMessage{
			db:     message,
			evt:    evt,
			handle: resp.HandleEcho,
		}
		portal.outgoingMessagesLock.Unlock()
	} else {
		// Hack to ensure the ghost row exists
		// TODO move to better place (like login)
		portal.Bridge.GetGhostByID(ctx, message.SenderID)
		err = portal.Bridge.DB.Message.Insert(ctx, message)
		if err != nil {
			log.Err(err).Msg("Failed to save message to database")
		}
		portal.sendSuccessStatus(ctx, evt)
	}
	if portal.Disappear.Type != database.DisappearingTypeNone {
		go portal.Bridge.DisappearLoop.Add(ctx, &database.DisappearingMessage{
			RoomID:  portal.MXID,
			EventID: message.MXID,
			DisappearingSetting: database.DisappearingSetting{
				Type:        portal.Disappear.Type,
				Timer:       portal.Disappear.Timer,
				DisappearAt: message.Timestamp.Add(portal.Disappear.Timer),
			},
		})
	}
}

func (portal *Portal) handleMatrixEdit(ctx context.Context, sender *UserLogin, origSender *OrigSender, evt *event.Event, content *event.MessageEventContent, caps *NetworkRoomCapabilities) {
	log := zerolog.Ctx(ctx)
	editTargetID := content.RelatesTo.GetReplaceID()
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Stringer("edit_target_mxid", editTargetID)
	})
	if content.NewContent != nil {
		content = content.NewContent
	}
	if origSender != nil {
		var err error
		content, err = portal.Bridge.Config.Relay.FormatMessage(content, origSender)
		if err != nil {
			log.Err(err).Msg("Failed to format message for relaying")
			portal.sendErrorStatus(ctx, evt, err)
			return
		}
	}

	editingAPI, ok := sender.Client.(EditHandlingNetworkAPI)
	if !ok {
		log.Debug().Msg("Ignoring edit as network connector doesn't implement EditHandlingNetworkAPI")
		portal.sendErrorStatus(ctx, evt, ErrEditsNotSupported)
		return
	} else if !caps.Edits {
		log.Debug().Msg("Ignoring edit as room doesn't support edits")
		portal.sendErrorStatus(ctx, evt, ErrEditsNotSupportedInPortal)
		return
	} else if !portal.checkMessageContentCaps(ctx, caps, content, evt) {
		return
	}
	editTarget, err := portal.Bridge.DB.Message.GetPartByMXID(ctx, editTargetID)
	if err != nil {
		log.Err(err).Msg("Failed to get edit target message from database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: failed to get edit target: %w", ErrDatabaseError, err))
		return
	} else if editTarget == nil {
		log.Warn().Msg("Edit target message not found in database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("edit %w", ErrTargetMessageNotFound))
		return
	} else if caps.EditMaxAge > 0 && time.Since(editTarget.Timestamp) > caps.EditMaxAge {
		portal.sendErrorStatus(ctx, evt, ErrEditTargetTooOld)
		return
	} else if caps.EditMaxCount > 0 && editTarget.EditCount >= caps.EditMaxCount {
		portal.sendErrorStatus(ctx, evt, ErrEditTargetTooManyEdits)
		return
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("edit_target_remote_id", string(editTarget.ID))
	})
	err = editingAPI.HandleMatrixEdit(ctx, &MatrixEdit{
		MatrixEventBase: MatrixEventBase[*event.MessageEventContent]{
			Event:      evt,
			Content:    content,
			OrigSender: origSender,
			Portal:     portal,
		},
		EditTarget: editTarget,
	})
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix edit")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
	err = portal.Bridge.DB.Message.Update(ctx, editTarget)
	if err != nil {
		log.Err(err).Msg("Failed to save message to database after editing")
	}
	portal.sendSuccessStatus(ctx, evt)
}

func (portal *Portal) handleMatrixReaction(ctx context.Context, sender *UserLogin, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	reactingAPI, ok := sender.Client.(ReactionHandlingNetworkAPI)
	if !ok {
		log.Debug().Msg("Ignoring reaction as network connector doesn't implement ReactionHandlingNetworkAPI")
		portal.sendErrorStatus(ctx, evt, ErrReactionsNotSupported)
		return
	}
	content, ok := evt.Content.Parsed.(*event.ReactionEventContent)
	if !ok {
		log.Error().Type("content_type", evt.Content.Parsed).Msg("Unexpected parsed content type")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: %T", ErrUnexpectedParsedContentType, evt.Content.Parsed))
		return
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Stringer("reaction_target_mxid", content.RelatesTo.EventID)
	})
	reactionTarget, err := portal.Bridge.DB.Message.GetPartByMXID(ctx, content.RelatesTo.EventID)
	if err != nil {
		log.Err(err).Msg("Failed to get reaction target message from database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: failed to get reaction target: %w", ErrDatabaseError, err))
		return
	} else if reactionTarget == nil {
		log.Warn().Msg("Reaction target message not found in database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("reaction %w", ErrTargetMessageNotFound))
		return
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("reaction_target_remote_id", string(reactionTarget.ID))
	})
	react := &MatrixReaction{
		MatrixEventBase: MatrixEventBase[*event.ReactionEventContent]{
			Event:   evt,
			Content: content,
			Portal:  portal,
		},
		TargetMessage: reactionTarget,
	}
	preResp, err := reactingAPI.PreHandleMatrixReaction(ctx, react)
	if err != nil {
		log.Err(err).Msg("Failed to pre-handle Matrix reaction")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
	existing, err := portal.Bridge.DB.Reaction.GetByID(ctx, reactionTarget.ID, reactionTarget.PartID, preResp.SenderID, preResp.EmojiID)
	if err != nil {
		log.Err(err).Msg("Failed to check if reaction is a duplicate")
		return
	} else if existing != nil {
		if existing.EmojiID != "" || existing.Emoji == preResp.Emoji {
			log.Debug().Msg("Ignoring duplicate reaction")
			portal.sendSuccessStatus(ctx, evt)
			return
		}
		react.ReactionToOverride = existing
		_, err = portal.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: existing.MXID,
			},
		}, nil)
		if err != nil {
			log.Err(err).Msg("Failed to remove old reaction")
		}
	}
	react.PreHandleResp = &preResp
	if preResp.MaxReactions > 0 {
		allReactions, err := portal.Bridge.DB.Reaction.GetAllToMessageBySender(ctx, reactionTarget.ID, preResp.SenderID)
		if err != nil {
			log.Err(err).Msg("Failed to get all reactions to message by sender")
			portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: failed to get previous reactions: %w", ErrDatabaseError, err))
			return
		}
		if len(allReactions) < preResp.MaxReactions {
			react.ExistingReactionsToKeep = allReactions
		} else {
			// Keep n-1 previous reactions and remove the rest
			react.ExistingReactionsToKeep = allReactions[:preResp.MaxReactions-1]
			for _, oldReaction := range allReactions[preResp.MaxReactions-1:] {
				_, err = portal.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
					Parsed: &event.RedactionEventContent{
						Redacts: oldReaction.MXID,
					},
				}, nil)
				if err != nil {
					log.Err(err).Msg("Failed to remove previous reaction after limit was exceeded")
				}
				err = portal.Bridge.DB.Reaction.Delete(ctx, oldReaction)
				if err != nil {
					log.Err(err).Msg("Failed to delete previous reaction from database after limit was exceeded")
				}
			}
		}
	}
	dbReaction, err := reactingAPI.HandleMatrixReaction(ctx, react)
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix reaction")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
	if dbReaction == nil {
		dbReaction = &database.Reaction{}
	}
	// Fill all fields that are known to allow omitting them in connector code
	if dbReaction.Room.ID == "" {
		dbReaction.Room = portal.PortalKey
	}
	if dbReaction.MessageID == "" {
		dbReaction.MessageID = reactionTarget.ID
		dbReaction.MessagePartID = reactionTarget.PartID
	}
	if dbReaction.MXID == "" {
		dbReaction.MXID = evt.ID
	}
	if dbReaction.Timestamp.IsZero() {
		dbReaction.Timestamp = time.UnixMilli(evt.Timestamp)
	}
	if preResp.EmojiID == "" && dbReaction.EmojiID == "" {
		if dbReaction.Emoji == "" {
			dbReaction.Emoji = preResp.Emoji
		}
	} else if dbReaction.EmojiID == "" {
		dbReaction.EmojiID = preResp.EmojiID
	}
	if dbReaction.SenderID == "" {
		dbReaction.SenderID = preResp.SenderID
	}
	if dbReaction.SenderMXID == "" {
		dbReaction.SenderMXID = evt.Sender
	}
	err = portal.Bridge.DB.Reaction.Upsert(ctx, dbReaction)
	if err != nil {
		log.Err(err).Msg("Failed to save reaction to database")
	}
	portal.sendSuccessStatus(ctx, evt)
}

func handleMatrixRoomMeta[APIType any, ContentType any](
	portal *Portal,
	ctx context.Context,
	sender *UserLogin,
	origSender *OrigSender,
	evt *event.Event,
	fn func(APIType, context.Context, *MatrixRoomMeta[ContentType]) (bool, error),
) {
	api, ok := sender.Client.(APIType)
	if !ok {
		portal.sendErrorStatus(ctx, evt, ErrRoomMetadataNotSupported)
		return
	}
	log := zerolog.Ctx(ctx)
	content, ok := evt.Content.Parsed.(ContentType)
	if !ok {
		log.Error().Type("content_type", evt.Content.Parsed).Msg("Unexpected parsed content type")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: %T", ErrUnexpectedParsedContentType, evt.Content.Parsed))
		return
	}
	switch typedContent := evt.Content.Parsed.(type) {
	case *event.RoomNameEventContent:
		if typedContent.Name == portal.Name {
			portal.sendSuccessStatus(ctx, evt)
			return
		}
	case *event.TopicEventContent:
		if typedContent.Topic == portal.Topic {
			portal.sendSuccessStatus(ctx, evt)
			return
		}
	case *event.RoomAvatarEventContent:
		if typedContent.URL == portal.AvatarMXC {
			portal.sendSuccessStatus(ctx, evt)
			return
		}
	}
	var prevContent ContentType
	if evt.Unsigned.PrevContent != nil {
		_ = evt.Unsigned.PrevContent.ParseRaw(evt.Type)
		prevContent, _ = evt.Unsigned.PrevContent.Parsed.(ContentType)
	}

	changed, err := fn(api, ctx, &MatrixRoomMeta[ContentType]{
		MatrixEventBase: MatrixEventBase[ContentType]{
			Event:      evt,
			Content:    content,
			Portal:     portal,
			OrigSender: origSender,
		},
		PrevContent: prevContent,
	})
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix room metadata")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
	if changed {
		portal.UpdateBridgeInfo(ctx)
		err = portal.Save(ctx)
		if err != nil {
			log.Err(err).Msg("Failed to save portal after updating room metadata")
		}
	}
	portal.sendSuccessStatus(ctx, evt)
}

func handleMatrixAccountData[APIType any, ContentType any](
	portal *Portal, ctx context.Context, sender *UserLogin, evt *event.Event,
	fn func(APIType, context.Context, *MatrixRoomMeta[ContentType]) error,
) {
	api, ok := sender.Client.(APIType)
	if !ok {
		return
	}
	log := zerolog.Ctx(ctx)
	content, ok := evt.Content.Parsed.(ContentType)
	if !ok {
		log.Error().Type("content_type", evt.Content.Parsed).Msg("Unexpected parsed content type")
		return
	}
	var prevContent ContentType
	if evt.Unsigned.PrevContent != nil {
		_ = evt.Unsigned.PrevContent.ParseRaw(evt.Type)
		prevContent, _ = evt.Unsigned.PrevContent.Parsed.(ContentType)
	}

	err := fn(api, ctx, &MatrixRoomMeta[ContentType]{
		MatrixEventBase: MatrixEventBase[ContentType]{
			Event:   evt,
			Content: content,
			Portal:  portal,
		},
		PrevContent: prevContent,
	})
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix room account data")
	}
}

func (portal *Portal) handleMatrixRedaction(ctx context.Context, sender *UserLogin, origSender *OrigSender, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	content, ok := evt.Content.Parsed.(*event.RedactionEventContent)
	if !ok {
		log.Error().Type("content_type", evt.Content.Parsed).Msg("Unexpected parsed content type")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: %T", ErrUnexpectedParsedContentType, evt.Content.Parsed))
		return
	}
	if evt.Redacts != "" && content.Redacts != evt.Redacts {
		content.Redacts = evt.Redacts
	}
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Stringer("redaction_target_mxid", content.Redacts)
	})
	deletingAPI, deleteOK := sender.Client.(RedactionHandlingNetworkAPI)
	reactingAPI, reactOK := sender.Client.(ReactionHandlingNetworkAPI)
	if !deleteOK && !reactOK {
		log.Debug().Msg("Ignoring redaction without checking target as network connector doesn't implement RedactionHandlingNetworkAPI nor ReactionHandlingNetworkAPI")
		portal.sendErrorStatus(ctx, evt, ErrRedactionsNotSupported)
		return
	}
	var redactionTargetReaction *database.Reaction
	redactionTargetMsg, err := portal.Bridge.DB.Message.GetPartByMXID(ctx, content.Redacts)
	if err != nil {
		log.Err(err).Msg("Failed to get redaction target message from database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: failed to get redaction target message: %w", ErrDatabaseError, err))
		return
	} else if redactionTargetMsg != nil {
		if !deleteOK {
			log.Debug().Msg("Ignoring message redaction event as network connector doesn't implement RedactionHandlingNetworkAPI")
			portal.sendErrorStatus(ctx, evt, ErrRedactionsNotSupported)
			return
		}
		err = deletingAPI.HandleMatrixMessageRemove(ctx, &MatrixMessageRemove{
			MatrixEventBase: MatrixEventBase[*event.RedactionEventContent]{
				Event:      evt,
				Content:    content,
				Portal:     portal,
				OrigSender: origSender,
			},
			TargetMessage: redactionTargetMsg,
		})
	} else if redactionTargetReaction, err = portal.Bridge.DB.Reaction.GetByMXID(ctx, content.Redacts); err != nil {
		log.Err(err).Msg("Failed to get redaction target reaction from database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("%w: failed to get redaction target message reaction: %w", ErrDatabaseError, err))
		return
	} else if redactionTargetReaction != nil {
		if !reactOK {
			log.Debug().Msg("Ignoring reaction redaction event as network connector doesn't implement ReactionHandlingNetworkAPI")
			portal.sendErrorStatus(ctx, evt, ErrReactionsNotSupported)
			return
		}
		// TODO ignore if sender doesn't match?
		err = reactingAPI.HandleMatrixReactionRemove(ctx, &MatrixReactionRemove{
			MatrixEventBase: MatrixEventBase[*event.RedactionEventContent]{
				Event:      evt,
				Content:    content,
				Portal:     portal,
				OrigSender: origSender,
			},
			TargetReaction: redactionTargetReaction,
		})
	} else {
		log.Debug().Msg("Redaction target message not found in database")
		portal.sendErrorStatus(ctx, evt, fmt.Errorf("redaction %w", ErrTargetMessageNotFound))
		return
	}
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix redaction")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
	// TODO delete msg/reaction db row
	portal.sendSuccessStatus(ctx, evt)
}

func (portal *Portal) handleRemoteEvent(source *UserLogin, evt RemoteEvent) {
	log := portal.Log.With().
		Str("source_id", string(source.ID)).
		Str("action", "handle remote event").
		Logger()
	defer func() {
		if err := recover(); err != nil {
			logEvt := log.Error()
			if realErr, ok := err.(error); ok {
				logEvt = logEvt.Err(realErr)
			} else {
				logEvt = logEvt.Any(zerolog.ErrorFieldName, err)
			}
			logEvt.
				Bytes("stack", debug.Stack()).
				Msg("Remote event handler panicked")
		}
	}()
	evtType := evt.GetType()
	log.UpdateContext(func(c zerolog.Context) zerolog.Context {
		c = c.Stringer("bridge_evt_type", evtType)
		return evt.AddLogContext(c)
	})
	ctx := log.WithContext(context.TODO())
	if portal.MXID == "" {
		mcp, ok := evt.(RemoteEventThatMayCreatePortal)
		if !ok || !mcp.ShouldCreatePortal() {
			log.Debug().Msg("Dropping event as portal doesn't exist")
			return
		}
		infoProvider, ok := mcp.(RemoteChatResyncWithInfo)
		var info *ChatInfo
		var err error
		if ok {
			info, err = infoProvider.GetChatInfo(ctx, portal)
			if err != nil {
				log.Err(err).Msg("Failed to get chat info for portal creation from chat resync event")
			}
		}
		err = portal.createMatrixRoomInLoop(ctx, source, info)
		if err != nil {
			log.Err(err).Msg("Failed to create portal to handle event")
			// TODO error
			return
		}
		if evtType == RemoteEventChatResync {
			log.Debug().Msg("Not handling chat resync event further as portal was created by it")
			return
		}
	}
	preHandler, ok := evt.(RemotePreHandler)
	if ok {
		preHandler.PreHandle(ctx, portal)
	}
	log.Debug().Msg("Handling remote event")
	switch evtType {
	case RemoteEventUnknown:
		log.Debug().Msg("Ignoring remote event with type unknown")
	case RemoteEventMessage, RemoteEventMessageUpsert:
		portal.handleRemoteMessage(ctx, source, evt.(RemoteMessage))
	case RemoteEventEdit:
		portal.handleRemoteEdit(ctx, source, evt.(RemoteEdit))
	case RemoteEventReaction:
		portal.handleRemoteReaction(ctx, source, evt.(RemoteReaction))
	case RemoteEventReactionRemove:
		portal.handleRemoteReactionRemove(ctx, source, evt.(RemoteReactionRemove))
	case RemoteEventReactionSync:
		portal.handleRemoteReactionSync(ctx, source, evt.(RemoteReactionSync))
	case RemoteEventMessageRemove:
		portal.handleRemoteMessageRemove(ctx, source, evt.(RemoteMessageRemove))
	case RemoteEventReadReceipt:
		portal.handleRemoteReadReceipt(ctx, source, evt.(RemoteReadReceipt))
	case RemoteEventMarkUnread:
		portal.handleRemoteMarkUnread(ctx, source, evt.(RemoteMarkUnread))
	case RemoteEventDeliveryReceipt:
		portal.handleRemoteDeliveryReceipt(ctx, source, evt.(RemoteDeliveryReceipt))
	case RemoteEventTyping:
		portal.handleRemoteTyping(ctx, source, evt.(RemoteTyping))
	case RemoteEventChatInfoChange:
		portal.handleRemoteChatInfoChange(ctx, source, evt.(RemoteChatInfoChange))
	case RemoteEventChatResync:
		portal.handleRemoteChatResync(ctx, source, evt.(RemoteChatResync))
	case RemoteEventChatDelete:
		portal.handleRemoteChatDelete(ctx, source, evt.(RemoteChatDelete))
	case RemoteEventBackfill:
		portal.handleRemoteBackfill(ctx, source, evt.(RemoteBackfill))
	default:
		log.Warn().Msg("Got remote event with unknown type")
	}
}

func (portal *Portal) getIntentAndUserMXIDFor(ctx context.Context, sender EventSender, source *UserLogin, otherLogins []*UserLogin, evtType RemoteEventType) (intent MatrixAPI, extraUserID id.UserID) {
	var ghost *Ghost
	if !sender.IsFromMe && sender.ForceDMUser && portal.OtherUserID != "" && sender.Sender != portal.OtherUserID {
		zerolog.Ctx(ctx).Warn().
			Str("original_id", string(sender.Sender)).
			Str("default_other_user", string(portal.OtherUserID)).
			Msg("Overriding event sender with primary other user in DM portal")
		sender.Sender = portal.OtherUserID
	}
	if sender.Sender != "" {
		var err error
		ghost, err = portal.Bridge.GetGhostByID(ctx, sender.Sender)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to get ghost for message sender")
			return
		} else {
			ghost.UpdateInfoIfNecessary(ctx, source, evtType)
		}
	}
	if sender.IsFromMe {
		intent = source.User.DoublePuppet(ctx)
		if intent != nil {
			return
		}
		extraUserID = source.UserMXID
	} else if sender.SenderLogin != "" && portal.Receiver == "" {
		senderLogin := portal.Bridge.GetCachedUserLoginByID(sender.SenderLogin)
		if senderLogin != nil {
			intent = senderLogin.User.DoublePuppet(ctx)
			if intent != nil {
				return
			}
			extraUserID = senderLogin.UserMXID
		}
	}
	if sender.Sender != "" && portal.Receiver == "" && otherLogins != nil {
		for _, login := range otherLogins {
			if login.Client.IsThisUser(ctx, sender.Sender) {
				intent = login.User.DoublePuppet(ctx)
				if intent != nil {
					return
				}
				extraUserID = login.UserMXID
			}
		}
	}
	if ghost != nil {
		intent = ghost.Intent
	}
	return
}

func (portal *Portal) GetIntentFor(ctx context.Context, sender EventSender, source *UserLogin, evtType RemoteEventType) MatrixAPI {
	intent, _ := portal.getIntentAndUserMXIDFor(ctx, sender, source, nil, evtType)
	if intent == nil {
		intent = portal.Bridge.Bot
	}
	return intent
}

func (portal *Portal) getRelationMeta(ctx context.Context, currentMsg networkid.MessageID, replyToPtr *networkid.MessageOptionalPartID, threadRootPtr *networkid.MessageID, isBatchSend bool) (replyTo, threadRoot, prevThreadEvent *database.Message) {
	log := zerolog.Ctx(ctx)
	var err error
	if replyToPtr != nil {
		replyTo, err = portal.Bridge.DB.Message.GetFirstOrSpecificPartByID(ctx, portal.Receiver, *replyToPtr)
		if err != nil {
			log.Err(err).Msg("Failed to get reply target message from database")
		} else if replyTo == nil {
			if isBatchSend {
				// This is somewhat evil
				replyTo = &database.Message{
					MXID: portal.Bridge.Matrix.GenerateDeterministicEventID(portal.MXID, portal.PortalKey, replyToPtr.MessageID, ptr.Val(replyToPtr.PartID)),
				}
			} else {
				log.Warn().Any("reply_to", *replyToPtr).Msg("Reply target message not found in database")
			}
		}
	}
	if threadRootPtr != nil && *threadRootPtr != currentMsg {
		threadRoot, err = portal.Bridge.DB.Message.GetFirstThreadMessage(ctx, portal.PortalKey, *threadRootPtr)
		if err != nil {
			log.Err(err).Msg("Failed to get thread root message from database")
		} else if threadRoot == nil {
			if isBatchSend {
				threadRoot = &database.Message{
					MXID: portal.Bridge.Matrix.GenerateDeterministicEventID(portal.MXID, portal.PortalKey, *threadRootPtr, ""),
				}
			} else {
				log.Warn().Str("thread_root", string(*threadRootPtr)).Msg("Thread root message not found in database")
			}
		} else if prevThreadEvent, err = portal.Bridge.DB.Message.GetLastThreadMessage(ctx, portal.PortalKey, *threadRootPtr); err != nil {
			log.Err(err).Msg("Failed to get last thread message from database")
		}
		if prevThreadEvent == nil {
			prevThreadEvent = threadRoot
		}
	}
	return
}

func (portal *Portal) applyRelationMeta(content *event.MessageEventContent, replyTo, threadRoot, prevThreadEvent *database.Message) {
	if threadRoot != nil && prevThreadEvent != nil {
		content.GetRelatesTo().SetThread(threadRoot.MXID, prevThreadEvent.MXID)
	}
	if replyTo != nil {
		content.GetRelatesTo().SetReplyTo(replyTo.MXID)
		if content.Mentions == nil {
			content.Mentions = &event.Mentions{}
		}
		content.Mentions.Add(replyTo.SenderMXID)
	}
}

func (portal *Portal) sendConvertedMessage(ctx context.Context, id networkid.MessageID, intent MatrixAPI, senderID networkid.UserID, converted *ConvertedMessage, ts time.Time, logContext func(*zerolog.Event) *zerolog.Event) []*database.Message {
	if logContext == nil {
		logContext = func(e *zerolog.Event) *zerolog.Event {
			return e
		}
	}
	log := zerolog.Ctx(ctx)
	replyTo, threadRoot, prevThreadEvent := portal.getRelationMeta(ctx, id, converted.ReplyTo, converted.ThreadRoot, false)
	output := make([]*database.Message, 0, len(converted.Parts))
	for _, part := range converted.Parts {
		portal.applyRelationMeta(part.Content, replyTo, threadRoot, prevThreadEvent)
		dbMessage := &database.Message{
			ID:         id,
			PartID:     part.ID,
			Room:       portal.PortalKey,
			SenderID:   senderID,
			SenderMXID: intent.GetMXID(),
			Timestamp:  ts,
			ThreadRoot: ptr.Val(converted.ThreadRoot),
			ReplyTo:    ptr.Val(converted.ReplyTo),
			Metadata:   part.DBMetadata,
		}
		if part.DontBridge {
			dbMessage.SetFakeMXID()
		} else {
			resp, err := intent.SendMessage(ctx, portal.MXID, part.Type, &event.Content{
				Parsed: part.Content,
				Raw:    part.Extra,
			}, &MatrixSendExtra{
				Timestamp:   ts,
				MessageMeta: dbMessage,
			})
			if err != nil {
				logContext(log.Err(err)).Str("part_id", string(part.ID)).Msg("Failed to send message part to Matrix")
				continue
			}
			logContext(log.Debug()).
				Stringer("event_id", resp.EventID).
				Str("part_id", string(part.ID)).
				Msg("Sent message part to Matrix")
			dbMessage.MXID = resp.EventID
		}
		err := portal.Bridge.DB.Message.Insert(ctx, dbMessage)
		if err != nil {
			logContext(log.Err(err)).Str("part_id", string(part.ID)).Msg("Failed to save message part to database")
		}
		if converted.Disappear.Type != database.DisappearingTypeNone && !dbMessage.HasFakeMXID() {
			if converted.Disappear.Type == database.DisappearingTypeAfterSend && converted.Disappear.DisappearAt.IsZero() {
				converted.Disappear.DisappearAt = dbMessage.Timestamp.Add(converted.Disappear.Timer)
			}
			go portal.Bridge.DisappearLoop.Add(ctx, &database.DisappearingMessage{
				RoomID:              portal.MXID,
				EventID:             dbMessage.MXID,
				DisappearingSetting: converted.Disappear,
			})
		}
		if prevThreadEvent != nil && !dbMessage.HasFakeMXID() {
			prevThreadEvent = dbMessage
		}
		output = append(output, dbMessage)
	}
	return output
}

func (portal *Portal) checkPendingMessage(ctx context.Context, evt RemoteMessage) (bool, *database.Message) {
	evtWithTxn, ok := evt.(RemoteMessageWithTransactionID)
	if !ok {
		return false, nil
	}
	txnID := evtWithTxn.GetTransactionID()
	if txnID == "" {
		return false, nil
	}
	portal.outgoingMessagesLock.Lock()
	defer portal.outgoingMessagesLock.Unlock()
	pending, ok := portal.outgoingMessages[txnID]
	if !ok {
		return false, nil
	}
	delete(portal.outgoingMessages, txnID)
	pending.db.ID = evt.GetID()
	if pending.db.SenderID == "" {
		pending.db.SenderID = evt.GetSender().Sender
	}
	evtWithTimestamp, ok := evt.(RemoteEventWithTimestamp)
	if ok {
		ts := evtWithTimestamp.GetTimestamp()
		if !ts.IsZero() {
			pending.db.Timestamp = ts
		}
	}
	var statusErr error
	saveMessage := true
	if pending.handle != nil {
		saveMessage, statusErr = pending.handle(evt, pending.db)
	}
	if saveMessage {
		// Hack to ensure the ghost row exists
		// TODO move to better place (like login)
		portal.Bridge.GetGhostByID(ctx, pending.db.SenderID)
		err := portal.Bridge.DB.Message.Insert(ctx, pending.db)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to save message to database after receiving remote echo")
		}
	}
	if !errors.Is(statusErr, ErrNoStatus) {
		if statusErr != nil {
			portal.sendErrorStatus(ctx, pending.evt, statusErr)
		} else {
			portal.sendSuccessStatus(ctx, pending.evt)
		}
	}
	zerolog.Ctx(ctx).Debug().Stringer("event_id", pending.evt.ID).Msg("Received remote echo for message")
	return true, pending.db
}

func (portal *Portal) handleRemoteUpsert(ctx context.Context, source *UserLogin, evt RemoteMessageUpsert, existing []*database.Message) bool {
	log := zerolog.Ctx(ctx)
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventMessageUpsert)
	if intent == nil {
		return false
	}
	res, err := evt.HandleExisting(ctx, portal, intent, existing)
	if err != nil {
		log.Err(err).Msg("Failed to handle existing message in upsert event after receiving remote echo")
	}
	if res.SaveParts {
		for _, part := range existing {
			err = portal.Bridge.DB.Message.Update(ctx, part)
			if err != nil {
				log.Err(err).Str("part_id", string(part.PartID)).Msg("Failed to update message part in database")
			}
		}
	}
	if len(res.SubEvents) > 0 {
		for _, subEvt := range res.SubEvents {
			portal.handleRemoteEvent(source, subEvt)
		}
	}
	return res.ContinueMessageHandling
}

func (portal *Portal) handleRemoteMessage(ctx context.Context, source *UserLogin, evt RemoteMessage) {
	log := zerolog.Ctx(ctx)
	upsertEvt, isUpsert := evt.(RemoteMessageUpsert)
	isUpsert = isUpsert && evt.GetType() == RemoteEventMessageUpsert
	if wasPending, dbMessage := portal.checkPendingMessage(ctx, evt); wasPending {
		if isUpsert {
			portal.handleRemoteUpsert(ctx, source, upsertEvt, []*database.Message{dbMessage})
		}
		return
	}
	existing, err := portal.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, evt.GetID())
	if err != nil {
		log.Err(err).Msg("Failed to check if message is a duplicate")
	} else if len(existing) > 0 {
		if isUpsert {
			if portal.handleRemoteUpsert(ctx, source, upsertEvt, existing) {
				log.Debug().Msg("Upsert handler said to continue message handling normally")
			} else {
				return
			}
		} else {
			log.Debug().Stringer("existing_mxid", existing[0].MXID).Msg("Ignoring duplicate message")
			return
		}
	}
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventMessage)
	if intent == nil {
		return
	}
	ts := getEventTS(evt)
	converted, err := evt.ConvertMessage(ctx, portal, intent)
	if err != nil {
		if errors.Is(err, ErrIgnoringRemoteEvent) {
			log.Debug().Err(err).Msg("Remote message handling was cancelled by convert function")
		} else {
			log.Err(err).Msg("Failed to convert remote message")
			portal.sendRemoteErrorNotice(ctx, intent, err, ts, "message")
		}
		return
	}
	portal.sendConvertedMessage(ctx, evt.GetID(), intent, evt.GetSender().Sender, converted, ts, nil)
}

func (portal *Portal) sendRemoteErrorNotice(ctx context.Context, intent MatrixAPI, err error, ts time.Time, evtTypeName string) {
	resp, sendErr := intent.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType:  event.MsgNotice,
			Body:     fmt.Sprintf("An error occurred while processing an incoming %s", evtTypeName),
			Mentions: &event.Mentions{},
		},
		Raw: map[string]any{
			"fi.mau.bridge.internal_error": err.Error(),
		},
	}, &MatrixSendExtra{
		Timestamp: ts,
	})
	if sendErr != nil {
		zerolog.Ctx(ctx).Err(sendErr).Msg("Failed to send error notice after remote event handling failed")
	} else {
		zerolog.Ctx(ctx).Debug().Stringer("event_id", resp.EventID).Msg("Sent error notice after remote event handling failed")
	}
}

func (portal *Portal) handleRemoteEdit(ctx context.Context, source *UserLogin, evt RemoteEdit) {
	log := zerolog.Ctx(ctx)
	var existing []*database.Message
	if bundledEvt, ok := evt.(RemoteEventWithBundledParts); ok {
		existing = bundledEvt.GetTargetDBMessage()
	}
	if existing == nil {
		targetID := evt.GetTargetMessage()
		var err error
		existing, err = portal.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, targetID)
		if err != nil {
			log.Err(err).Msg("Failed to get edit target message")
			return
		}
	}
	if existing == nil {
		log.Warn().Msg("Edit target message not found")
		return
	}
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventEdit)
	if intent == nil {
		return
	}
	ts := getEventTS(evt)
	converted, err := evt.ConvertEdit(ctx, portal, intent, existing)
	if errors.Is(err, ErrIgnoringRemoteEvent) {
		log.Debug().Err(err).Msg("Remote edit handling was cancelled by convert function")
		return
	} else if err != nil {
		log.Err(err).Msg("Failed to convert remote edit")
		portal.sendRemoteErrorNotice(ctx, intent, err, ts, "edit")
		return
	}
	portal.sendConvertedEdit(ctx, existing[0].ID, evt.GetSender().Sender, converted, intent, ts)
}

func (portal *Portal) sendConvertedEdit(ctx context.Context, targetID networkid.MessageID, senderID networkid.UserID, converted *ConvertedEdit, intent MatrixAPI, ts time.Time) {
	log := zerolog.Ctx(ctx)
	for _, part := range converted.ModifiedParts {
		overrideMXID := true
		if part.Part.Room != portal.PortalKey {
			part.Part.Room = portal.PortalKey
		} else if !part.Part.HasFakeMXID() {
			part.Content.SetEdit(part.Part.MXID)
			overrideMXID = false
		}
		if part.TopLevelExtra == nil {
			part.TopLevelExtra = make(map[string]any)
		}
		if part.Extra != nil {
			part.TopLevelExtra["m.new_content"] = part.Extra
		}
		wrappedContent := &event.Content{
			Parsed: part.Content,
			Raw:    part.TopLevelExtra,
		}
		if !part.DontBridge {
			resp, err := intent.SendMessage(ctx, portal.MXID, part.Type, wrappedContent, &MatrixSendExtra{
				Timestamp:   ts,
				MessageMeta: part.Part,
			})
			if err != nil {
				log.Err(err).Stringer("part_mxid", part.Part.MXID).Msg("Failed to edit message part")
				continue
			} else {
				log.Debug().
					Stringer("event_id", resp.EventID).
					Str("part_id", string(part.Part.ID)).
					Msg("Sent message part edit to Matrix")
				if overrideMXID {
					part.Part.MXID = resp.EventID
				}
			}
		}
		err := portal.Bridge.DB.Message.Update(ctx, part.Part)
		if err != nil {
			log.Err(err).Int64("part_rowid", part.Part.RowID).Msg("Failed to update message part in database")
		}
	}
	for _, part := range converted.DeletedParts {
		redactContent := &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: part.MXID,
			},
		}
		resp, err := intent.SendMessage(ctx, portal.MXID, event.EventRedaction, redactContent, &MatrixSendExtra{
			Timestamp: ts,
		})
		if err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to redact message part deleted in edit")
		} else {
			log.Debug().
				Stringer("redaction_event_id", resp.EventID).
				Stringer("redacted_event_id", part.MXID).
				Str("part_id", string(part.ID)).
				Msg("Sent redaction of message part to Matrix")
		}
		err = portal.Bridge.DB.Message.Delete(ctx, part.RowID)
		if err != nil {
			log.Err(err).Int64("part_rowid", part.RowID).Msg("Failed to delete message part from database")
		}
	}
	if converted.AddedParts != nil {
		portal.sendConvertedMessage(ctx, targetID, intent, senderID, converted.AddedParts, ts, nil)
	}
}

func (portal *Portal) getTargetMessagePart(ctx context.Context, evt RemoteEventWithTargetMessage) (*database.Message, error) {
	if partTargeter, ok := evt.(RemoteEventWithTargetPart); ok {
		return portal.Bridge.DB.Message.GetPartByID(ctx, portal.Receiver, evt.GetTargetMessage(), partTargeter.GetTargetMessagePart())
	} else {
		return portal.Bridge.DB.Message.GetFirstPartByID(ctx, portal.Receiver, evt.GetTargetMessage())
	}
}

func (portal *Portal) getTargetReaction(ctx context.Context, evt RemoteReactionRemove) (*database.Reaction, error) {
	if partTargeter, ok := evt.(RemoteEventWithTargetPart); ok {
		return portal.Bridge.DB.Reaction.GetByID(ctx, evt.GetTargetMessage(), partTargeter.GetTargetMessagePart(), evt.GetSender().Sender, evt.GetRemovedEmojiID())
	} else {
		return portal.Bridge.DB.Reaction.GetByIDWithoutMessagePart(ctx, evt.GetTargetMessage(), evt.GetSender().Sender, evt.GetRemovedEmojiID())
	}
}

func getEventTS(evt RemoteEvent) time.Time {
	if tsProvider, ok := evt.(RemoteEventWithTimestamp); ok {
		return tsProvider.GetTimestamp()
	}
	return time.Now()
}

func (portal *Portal) handleRemoteReactionSync(ctx context.Context, source *UserLogin, evt RemoteReactionSync) {
	log := zerolog.Ctx(ctx)
	eventTS := getEventTS(evt)
	targetMessage, err := portal.getTargetMessagePart(ctx, evt)
	if err != nil {
		log.Err(err).Msg("Failed to get target message for reaction")
		return
	} else if targetMessage == nil {
		// TODO use deterministic event ID as target if applicable?
		log.Warn().Msg("Target message for reaction not found")
		return
	}
	var existingReactions []*database.Reaction
	if partTargeter, ok := evt.(RemoteEventWithTargetPart); ok {
		existingReactions, err = portal.Bridge.DB.Reaction.GetAllToMessagePart(ctx, evt.GetTargetMessage(), partTargeter.GetTargetMessagePart())
	} else {
		existingReactions, err = portal.Bridge.DB.Reaction.GetAllToMessage(ctx, evt.GetTargetMessage())
	}
	existing := make(map[networkid.UserID]map[networkid.EmojiID]*database.Reaction)
	for _, existingReaction := range existingReactions {
		if existing[existingReaction.SenderID] == nil {
			existing[existingReaction.SenderID] = make(map[networkid.EmojiID]*database.Reaction)
		}
		existing[existingReaction.SenderID][existingReaction.EmojiID] = existingReaction
	}

	doAddReaction := func(new *BackfillReaction) MatrixAPI {
		intent := portal.GetIntentFor(ctx, new.Sender, source, RemoteEventReactionSync)
		portal.sendConvertedReaction(
			ctx, new.Sender.Sender, intent, targetMessage, new.EmojiID, new.Emoji,
			new.Timestamp, new.DBMetadata, new.ExtraContent,
			func(z *zerolog.Event) *zerolog.Event {
				return z.
					Any("reaction_sender_id", new.Sender).
					Time("reaction_ts", new.Timestamp)
			},
		)
		return intent
	}
	doRemoveReaction := func(old *database.Reaction, intent MatrixAPI, deleteRow bool) {
		if intent == nil && old.SenderMXID != "" {
			intent, err = portal.getIntentForMXID(ctx, old.SenderMXID)
			if err != nil {
				log.Err(err).
					Stringer("reaction_sender_mxid", old.SenderMXID).
					Msg("Failed to get intent for removing reaction")
			}
		}
		if intent == nil {
			log.Warn().
				Str("reaction_sender_id", string(old.SenderID)).
				Stringer("reaction_sender_mxid", old.SenderMXID).
				Msg("Didn't find intent for removing reaction, using bridge bot")
			intent = portal.Bridge.Bot
		}
		_, err = intent.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: old.MXID,
			},
		}, &MatrixSendExtra{Timestamp: eventTS})
		if err != nil {
			log.Err(err).Msg("Failed to redact old reaction")
		}
		if deleteRow {
			err = portal.Bridge.DB.Reaction.Delete(ctx, old)
			if err != nil {
				log.Err(err).Msg("Failed to delete old reaction row")
			}
		}
	}
	doOverwriteReaction := func(new *BackfillReaction, old *database.Reaction) {
		intent := doAddReaction(new)
		doRemoveReaction(old, intent, false)
	}

	newData := evt.GetReactions()
	for userID, reactions := range newData.Users {
		existingUserReactions := existing[userID]
		delete(existing, userID)
		for _, reaction := range reactions.Reactions {
			if reaction.Timestamp.IsZero() {
				reaction.Timestamp = eventTS
			}
			existingReaction, ok := existingUserReactions[reaction.EmojiID]
			if ok {
				delete(existingUserReactions, reaction.EmojiID)
				if reaction.EmojiID != "" {
					continue
				}
				doOverwriteReaction(reaction, existingReaction)
			} else {
				doAddReaction(reaction)
			}
		}
		totalReactionCount := len(existingUserReactions) + len(reactions.Reactions)
		if reactions.HasAllReactions {
			for _, existingReaction := range existingUserReactions {
				doRemoveReaction(existingReaction, nil, true)
			}
		} else if reactions.MaxCount > 0 && totalReactionCount > reactions.MaxCount {
			remainingReactionList := maps.Values(existingUserReactions)
			slices.SortFunc(remainingReactionList, func(a, b *database.Reaction) int {
				diff := a.Timestamp.Compare(b.Timestamp)
				if diff == 0 {
					return cmp.Compare(a.EmojiID, b.EmojiID)
				}
				return diff
			})
			numberToRemove := totalReactionCount - reactions.MaxCount
			for i := 0; i < numberToRemove && i < len(remainingReactionList); i++ {
				doRemoveReaction(remainingReactionList[i], nil, true)
			}
		}
	}
	if newData.HasAllUsers {
		for _, userReactions := range existing {
			for _, existingReaction := range userReactions {
				doRemoveReaction(existingReaction, nil, true)
			}
		}
	}
}

func (portal *Portal) handleRemoteReaction(ctx context.Context, source *UserLogin, evt RemoteReaction) {
	log := zerolog.Ctx(ctx)
	targetMessage, err := portal.getTargetMessagePart(ctx, evt)
	if err != nil {
		log.Err(err).Msg("Failed to get target message for reaction")
		return
	} else if targetMessage == nil {
		// TODO use deterministic event ID as target if applicable?
		log.Warn().Msg("Target message for reaction not found")
		return
	}
	emoji, emojiID := evt.GetReactionEmoji()
	existingReaction, err := portal.Bridge.DB.Reaction.GetByID(ctx, targetMessage.ID, targetMessage.PartID, evt.GetSender().Sender, emojiID)
	if err != nil {
		log.Err(err).Msg("Failed to check if reaction is a duplicate")
		return
	} else if existingReaction != nil && (emojiID != "" || existingReaction.Emoji == emoji) {
		log.Debug().Msg("Ignoring duplicate reaction")
		return
	}
	ts := getEventTS(evt)
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventReaction)
	var extra map[string]any
	if extraContentProvider, ok := evt.(RemoteReactionWithExtraContent); ok {
		extra = extraContentProvider.GetReactionExtraContent()
	}
	var dbMetadata any
	if metaProvider, ok := evt.(RemoteReactionWithMeta); ok {
		dbMetadata = metaProvider.GetReactionDBMetadata()
	}
	portal.sendConvertedReaction(ctx, evt.GetSender().Sender, intent, targetMessage, emojiID, emoji, ts, dbMetadata, extra, nil)
	if existingReaction != nil {
		_, err = intent.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: existingReaction.MXID,
			},
		}, &MatrixSendExtra{Timestamp: ts})
		if err != nil {
			log.Err(err).Msg("Failed to redact old reaction")
		}
	}
}

func (portal *Portal) sendConvertedReaction(
	ctx context.Context, senderID networkid.UserID, intent MatrixAPI, targetMessage *database.Message,
	emojiID networkid.EmojiID, emoji string, ts time.Time, dbMetadata any, extraContent map[string]any,
	logContext func(*zerolog.Event) *zerolog.Event,
) {
	if logContext == nil {
		logContext = func(e *zerolog.Event) *zerolog.Event {
			return e
		}
	}
	log := zerolog.Ctx(ctx)
	dbReaction := &database.Reaction{
		Room:          portal.PortalKey,
		MessageID:     targetMessage.ID,
		MessagePartID: targetMessage.PartID,
		SenderID:      senderID,
		SenderMXID:    intent.GetMXID(),
		EmojiID:       emojiID,
		Timestamp:     ts,
		Metadata:      dbMetadata,
	}
	if emojiID == "" {
		dbReaction.Emoji = emoji
	}
	resp, err := intent.SendMessage(ctx, portal.MXID, event.EventReaction, &event.Content{
		Parsed: &event.ReactionEventContent{
			RelatesTo: event.RelatesTo{
				Type:    event.RelAnnotation,
				EventID: targetMessage.MXID,
				Key:     variationselector.Add(emoji),
			},
		},
		Raw: extraContent,
	}, &MatrixSendExtra{
		Timestamp:    ts,
		ReactionMeta: dbReaction,
	})
	if err != nil {
		logContext(log.Err(err)).Msg("Failed to send reaction to Matrix")
		return
	}
	logContext(log.Debug()).
		Stringer("event_id", resp.EventID).
		Msg("Sent reaction to Matrix")
	dbReaction.MXID = resp.EventID
	err = portal.Bridge.DB.Reaction.Upsert(ctx, dbReaction)
	if err != nil {
		logContext(log.Err(err)).Msg("Failed to save reaction to database")
	}
}

func (portal *Portal) getIntentForMXID(ctx context.Context, userID id.UserID) (MatrixAPI, error) {
	if userID == "" {
		return nil, nil
	} else if ghost, err := portal.Bridge.GetGhostByMXID(ctx, userID); err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	} else if ghost != nil {
		return ghost.Intent, nil
	} else if user, err := portal.Bridge.GetExistingUserByMXID(ctx, userID); err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	} else if user != nil {
		return user.DoublePuppet(ctx), nil
	} else {
		return nil, nil
	}
}

func (portal *Portal) handleRemoteReactionRemove(ctx context.Context, source *UserLogin, evt RemoteReactionRemove) {
	log := zerolog.Ctx(ctx)
	targetReaction, err := portal.getTargetReaction(ctx, evt)
	if err != nil {
		log.Err(err).Msg("Failed to get target reaction for removal")
		return
	} else if targetReaction == nil {
		log.Warn().Msg("Target reaction not found")
		return
	}
	intent, err := portal.getIntentForMXID(ctx, targetReaction.SenderMXID)
	if err != nil {
		log.Err(err).Stringer("sender_mxid", targetReaction.SenderMXID).Msg("Failed to get intent for removing reaction")
	}
	if intent == nil {
		intent = portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventReactionRemove)
	}
	ts := getEventTS(evt)
	_, err = intent.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
		Parsed: &event.RedactionEventContent{
			Redacts: targetReaction.MXID,
		},
	}, &MatrixSendExtra{Timestamp: ts, ReactionMeta: targetReaction})
	if err != nil {
		log.Err(err).Stringer("reaction_mxid", targetReaction.MXID).Msg("Failed to redact reaction")
	}
	err = portal.Bridge.DB.Reaction.Delete(ctx, targetReaction)
	if err != nil {
		log.Err(err).Msg("Failed to delete target reaction from database")
	}
}

func (portal *Portal) handleRemoteMessageRemove(ctx context.Context, source *UserLogin, evt RemoteMessageRemove) {
	log := zerolog.Ctx(ctx)
	targetParts, err := portal.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, evt.GetTargetMessage())
	if err != nil {
		log.Err(err).Msg("Failed to get target message for removal")
		return
	} else if len(targetParts) == 0 {
		log.Debug().Msg("Target message not found")
		return
	}
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventMessageRemove)
	if intent == portal.Bridge.Bot && len(targetParts) > 0 {
		senderIntent, err := portal.getIntentForMXID(ctx, targetParts[0].SenderMXID)
		if err != nil {
			log.Err(err).Stringer("sender_mxid", targetParts[0].SenderMXID).Msg("Failed to get intent for removing message")
		} else if senderIntent != nil {
			intent = senderIntent
		}
	}
	portal.redactMessageParts(ctx, targetParts, intent, getEventTS(evt))
	err = portal.Bridge.DB.Message.DeleteAllParts(ctx, portal.Receiver, evt.GetTargetMessage())
	if err != nil {
		log.Err(err).Msg("Failed to delete target message from database")
	}
}

func (portal *Portal) redactMessageParts(ctx context.Context, parts []*database.Message, intent MatrixAPI, ts time.Time) {
	log := zerolog.Ctx(ctx)
	for _, part := range parts {
		if part.HasFakeMXID() {
			continue
		}
		resp, err := intent.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: part.MXID,
			},
		}, &MatrixSendExtra{Timestamp: ts, MessageMeta: part})
		if err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to redact message part")
		} else {
			log.Debug().
				Stringer("redaction_event_id", resp.EventID).
				Stringer("redacted_event_id", part.MXID).
				Str("part_id", string(part.ID)).
				Msg("Sent redaction of message part to Matrix")
		}
	}
}

func (portal *Portal) handleRemoteReadReceipt(ctx context.Context, source *UserLogin, evt RemoteReadReceipt) {
	// TODO exclude fake mxids
	log := zerolog.Ctx(ctx)
	var err error
	var lastTarget *database.Message
	if lastTargetID := evt.GetLastReceiptTarget(); lastTargetID != "" {
		lastTarget, err = portal.Bridge.DB.Message.GetLastPartByID(ctx, portal.Receiver, lastTargetID)
		if err != nil {
			log.Err(err).Str("last_target_id", string(lastTargetID)).
				Msg("Failed to get last target message for read receipt")
			return
		} else if lastTarget == nil {
			log.Debug().Str("last_target_id", string(lastTargetID)).
				Msg("Last target message not found")
		}
	}
	if lastTarget == nil {
		for _, targetID := range evt.GetReceiptTargets() {
			target, err := portal.Bridge.DB.Message.GetLastPartByID(ctx, portal.Receiver, targetID)
			if err != nil {
				log.Err(err).Str("target_id", string(targetID)).
					Msg("Failed to get target message for read receipt")
				return
			} else if target != nil && (lastTarget == nil || target.Timestamp.After(lastTarget.Timestamp)) {
				lastTarget = target
			}
		}
	}
	readUpTo := evt.GetReadUpTo()
	if lastTarget == nil && !readUpTo.IsZero() {
		lastTarget, err = portal.Bridge.DB.Message.GetLastPartAtOrBeforeTime(ctx, portal.PortalKey, readUpTo)
		if err != nil {
			log.Err(err).Time("read_up_to", readUpTo).Msg("Failed to get target message for read receipt")
		}
	}
	if lastTarget == nil {
		log.Warn().Msg("No target message found for read receipt")
		return
	}
	sender := evt.GetSender()
	intent := portal.GetIntentFor(ctx, sender, source, RemoteEventReadReceipt)
	err = intent.MarkRead(ctx, portal.MXID, lastTarget.MXID, getEventTS(evt))
	if err != nil {
		log.Err(err).Stringer("target_mxid", lastTarget.MXID).Msg("Failed to bridge read receipt")
	} else {
		log.Debug().Stringer("target_mxid", lastTarget.MXID).Msg("Bridged read receipt")
	}
	if sender.IsFromMe {
		portal.Bridge.DisappearLoop.StartAll(ctx, portal.MXID)
	}
}

func (portal *Portal) handleRemoteMarkUnread(ctx context.Context, source *UserLogin, evt RemoteMarkUnread) {
	if !evt.GetSender().IsFromMe {
		zerolog.Ctx(ctx).Warn().Msg("Ignoring mark unread event from non-self user")
		return
	}
	dp := source.User.DoublePuppet(ctx)
	if dp == nil {
		return
	}
	err := dp.MarkUnread(ctx, portal.MXID, evt.GetUnread())
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to bridge mark unread event")
	}
}

func (portal *Portal) handleRemoteDeliveryReceipt(ctx context.Context, source *UserLogin, evt RemoteDeliveryReceipt) {
	if portal.RoomType != database.RoomTypeDM || evt.GetSender().Sender != portal.OtherUserID {
		return
	}
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventDeliveryReceipt)
	log := zerolog.Ctx(ctx)
	for _, target := range evt.GetReceiptTargets() {
		targetParts, err := portal.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, target)
		if err != nil {
			log.Err(err).Str("target_id", string(target)).Msg("Failed to get target message for delivery receipt")
			continue
		} else if len(targetParts) == 0 {
			continue
		} else if _, sentByGhost := portal.Bridge.Matrix.ParseGhostMXID(targetParts[0].SenderMXID); sentByGhost {
			continue
		}
		for _, part := range targetParts {
			portal.Bridge.Matrix.SendMessageStatus(ctx, &MessageStatus{
				Status:      event.MessageStatusSuccess,
				DeliveredTo: []id.UserID{intent.GetMXID()},
			}, &MessageStatusEventInfo{
				RoomID:  portal.MXID,
				EventID: part.MXID,
				Sender:  part.SenderMXID,
			})
		}
	}
}

func (portal *Portal) handleRemoteTyping(ctx context.Context, source *UserLogin, evt RemoteTyping) {
	var typingType TypingType
	if typedEvt, ok := evt.(RemoteTypingWithType); ok {
		typingType = typedEvt.GetTypingType()
	}
	intent := portal.GetIntentFor(ctx, evt.GetSender(), source, RemoteEventTyping)
	err := intent.MarkTyping(ctx, portal.MXID, typingType, evt.GetTimeout())
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to bridge typing event")
	}
}

func (portal *Portal) handleRemoteChatInfoChange(ctx context.Context, source *UserLogin, evt RemoteChatInfoChange) {
	info, err := evt.GetChatInfoChange(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get chat info change")
		return
	}
	portal.ProcessChatInfoChange(ctx, evt.GetSender(), source, info, getEventTS(evt))
}

func (portal *Portal) handleRemoteChatResync(ctx context.Context, source *UserLogin, evt RemoteChatResync) {
	log := zerolog.Ctx(ctx)
	infoProvider, ok := evt.(RemoteChatResyncWithInfo)
	if ok {
		info, err := infoProvider.GetChatInfo(ctx, portal)
		if err != nil {
			log.Err(err).Msg("Failed to get chat info from resync event")
		} else if info != nil {
			portal.UpdateInfo(ctx, info, source, nil, time.Time{})
		}
	}
	backfillChecker, ok := evt.(RemoteChatResyncBackfill)
	if portal.Bridge.Config.Backfill.Enabled && ok {
		latestMessage, err := portal.Bridge.DB.Message.GetLastPartAtOrBeforeTime(ctx, portal.PortalKey, time.Now().Add(10*time.Second))
		if err != nil {
			log.Err(err).Msg("Failed to get last message in portal to check if backfill is necessary")
		} else if needsBackfill, err := backfillChecker.CheckNeedsBackfill(ctx, latestMessage); err != nil {
			log.Err(err).Msg("Failed to check if backfill is needed")
		} else if needsBackfill {
			portal.doForwardBackfill(ctx, source, latestMessage)
		}
	}
}

func (portal *Portal) handleRemoteChatDelete(ctx context.Context, source *UserLogin, evt RemoteChatDelete) {
	if portal.Receiver == "" && evt.DeleteOnlyForMe() {
		// TODO check if there are other users
	}
	err := portal.Delete(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to delete portal from database")
		return
	}
	err = portal.Bridge.Bot.DeleteRoom(ctx, portal.MXID, false)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to delete Matrix room")
	}
}

func (portal *Portal) handleRemoteBackfill(ctx context.Context, source *UserLogin, backfill RemoteBackfill) {
	//data, err := backfill.GetBackfillData(ctx, portal)
	//if err != nil {
	//	zerolog.Ctx(ctx).Err(err).Msg("Failed to get backfill data")
	//	return
	//}
}

type ChatInfoChange struct {
	// The chat info that changed. Any fields that did not change can be left as nil.
	ChatInfo *ChatInfo
	// A list of member changes.
	// This list should only include changes, not the whole member list.
	// To resync the whole list, use the field inside ChatInfo.
	MemberChanges *ChatMemberList
}

func (portal *Portal) ProcessChatInfoChange(ctx context.Context, sender EventSender, source *UserLogin, change *ChatInfoChange, ts time.Time) {
	intent := portal.GetIntentFor(ctx, sender, source, RemoteEventChatInfoChange)
	if change.ChatInfo != nil {
		portal.UpdateInfo(ctx, change.ChatInfo, source, intent, ts)
	}
	if change.MemberChanges != nil {
		err := portal.syncParticipants(ctx, change.MemberChanges, source, intent, ts)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to sync room members")
		}
	}
}

// Deprecated: Renamed to ChatInfo
type PortalInfo = ChatInfo

type ChatMember struct {
	EventSender
	Membership event.Membership
	Nickname   *string
	PowerLevel *int
	UserInfo   *UserInfo

	PrevMembership event.Membership
}

type ChatMemberList struct {
	// Whether this is the full member list.
	// If true, any extra members not listed here will be removed from the portal.
	IsFull bool
	// Should the bridge call IsThisUser for every member in the list?
	// This should be used when SenderLogin can't be filled accurately.
	CheckAllLogins bool

	// The total number of members in the chat, regardless of how many of those members are included in Members.
	TotalMemberCount int

	// For DM portals, the ID of the recipient user.
	// This field is optional and will be automatically filled from Members if there are only 2 entries in the list.
	OtherUserID networkid.UserID

	Members     []ChatMember
	PowerLevels *PowerLevelChanges
}

type PowerLevelChanges struct {
	Events        map[event.Type]int
	UsersDefault  *int
	EventsDefault *int
	StateDefault  *int
	Invite        *int
	Kick          *int
	Ban           *int
	Redact        *int

	Custom func(*event.PowerLevelsEventContent) bool
}

func allowChange(newLevel, oldLevel, actorLevel int) bool {
	return newLevel <= actorLevel && oldLevel <= actorLevel
}

func (plc *PowerLevelChanges) Apply(actor id.UserID, content *event.PowerLevelsEventContent) (changed bool) {
	if plc == nil || content == nil {
		return
	}
	for evtType, level := range plc.Events {
		changed = content.EnsureEventLevelAs(actor, evtType, level) || changed
	}
	var actorLevel int
	if actor != "" {
		actorLevel = content.GetUserLevel(actor)
	} else {
		actorLevel = (1 << 31) - 1
	}
	if plc.UsersDefault != nil && allowChange(*plc.UsersDefault, content.UsersDefault, actorLevel) {
		changed = content.UsersDefault != *plc.UsersDefault
		content.UsersDefault = *plc.UsersDefault
	}
	if plc.EventsDefault != nil && allowChange(*plc.EventsDefault, content.EventsDefault, actorLevel) {
		changed = content.EventsDefault != *plc.EventsDefault
		content.EventsDefault = *plc.EventsDefault
	}
	if plc.StateDefault != nil && allowChange(*plc.StateDefault, content.StateDefault(), actorLevel) {
		changed = content.StateDefault() != *plc.StateDefault
		content.StateDefaultPtr = plc.StateDefault
	}
	if plc.Invite != nil && allowChange(*plc.Invite, content.Invite(), actorLevel) {
		changed = content.Invite() != *plc.Invite
		content.InvitePtr = plc.Invite
	}
	if plc.Kick != nil && allowChange(*plc.Kick, content.Kick(), actorLevel) {
		changed = content.Kick() != *plc.Kick
		content.KickPtr = plc.Kick
	}
	if plc.Ban != nil && allowChange(*plc.Ban, content.Ban(), actorLevel) {
		changed = content.Ban() != *plc.Ban
		content.BanPtr = plc.Ban
	}
	if plc.Redact != nil && allowChange(*plc.Redact, content.Redact(), actorLevel) {
		changed = content.Redact() != *plc.Redact
		content.RedactPtr = plc.Redact
	}
	if plc.Custom != nil {
		changed = plc.Custom(content) || changed
	}
	return changed
}

type ChatInfo struct {
	Name   *string
	Topic  *string
	Avatar *Avatar

	Members  *ChatMemberList
	JoinRule *event.JoinRulesEventContent

	Type      *database.RoomType
	Disappear *database.DisappearingSetting
	ParentID  *networkid.PortalID

	UserLocal *UserLocalPortalInfo

	CanBackfill bool

	ExtraUpdates ExtraUpdater[*Portal]
}

type ExtraUpdater[T any] func(context.Context, T) bool

func MergeExtraUpdaters[T any](funcs ...ExtraUpdater[T]) ExtraUpdater[T] {
	funcs = slices.DeleteFunc(funcs, func(f ExtraUpdater[T]) bool {
		return f == nil
	})
	if len(funcs) == 0 {
		return nil
	} else if len(funcs) == 1 {
		return funcs[0]
	}
	return func(ctx context.Context, p T) bool {
		changed := false
		for _, f := range funcs {
			changed = f(ctx, p) || changed
		}
		return changed
	}
}

var Unmuted = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

type UserLocalPortalInfo struct {
	// To signal an indefinite mute, use [event.MutedForever] as the value here.
	// To unmute, set any time before now, e.g. [bridgev2.Unmuted].
	MutedUntil *time.Time
	Tag        *event.RoomTag
}

func (portal *Portal) updateName(ctx context.Context, name string, sender MatrixAPI, ts time.Time) bool {
	if portal.Name == name && (portal.NameSet || portal.MXID == "") {
		return false
	}
	portal.Name = name
	portal.NameSet = portal.sendRoomMeta(ctx, sender, ts, event.StateRoomName, "", &event.RoomNameEventContent{Name: name})
	return true
}

func (portal *Portal) updateTopic(ctx context.Context, topic string, sender MatrixAPI, ts time.Time) bool {
	if portal.Topic == topic && (portal.TopicSet || portal.MXID == "") {
		return false
	}
	portal.Topic = topic
	portal.TopicSet = portal.sendRoomMeta(ctx, sender, ts, event.StateTopic, "", &event.TopicEventContent{Topic: topic})
	return true
}

func (portal *Portal) updateAvatar(ctx context.Context, avatar *Avatar, sender MatrixAPI, ts time.Time) bool {
	if portal.AvatarID == avatar.ID && (portal.AvatarSet || portal.MXID == "") {
		return false
	}
	portal.AvatarID = avatar.ID
	if sender == nil {
		sender = portal.Bridge.Bot
	}
	if avatar.Remove {
		portal.AvatarMXC = ""
		portal.AvatarHash = [32]byte{}
	} else {
		newMXC, newHash, err := avatar.Reupload(ctx, sender, portal.AvatarHash, portal.AvatarMXC)
		if err != nil {
			portal.AvatarSet = false
			zerolog.Ctx(ctx).Err(err).Msg("Failed to reupload room avatar")
			return true
		} else if newHash == portal.AvatarHash && portal.AvatarSet {
			return true
		}
		portal.AvatarMXC = newMXC
		portal.AvatarHash = newHash
	}
	portal.AvatarSet = portal.sendRoomMeta(ctx, sender, ts, event.StateRoomAvatar, "", &event.RoomAvatarEventContent{URL: portal.AvatarMXC})
	return true
}

func (portal *Portal) GetTopLevelParent() *Portal {
	if portal.Parent == nil {
		if portal.RoomType != database.RoomTypeSpace {
			return nil
		}
		return portal
	}
	return portal.Parent.GetTopLevelParent()
}

func (portal *Portal) getBridgeInfo() (string, event.BridgeEventContent) {
	bridgeInfo := event.BridgeEventContent{
		BridgeBot: portal.Bridge.Bot.GetMXID(),
		Creator:   portal.Bridge.Bot.GetMXID(),
		Protocol:  portal.Bridge.Network.GetName().AsBridgeInfoSection(),
		Channel: event.BridgeInfoSection{
			ID:          string(portal.ID),
			DisplayName: portal.Name,
			AvatarURL:   portal.AvatarMXC,
			// TODO external URL?
		},
		BeeperRoomTypeV2: string(portal.RoomType),
	}
	if portal.RoomType == database.RoomTypeDM || portal.RoomType == database.RoomTypeGroupDM {
		bridgeInfo.BeeperRoomType = "dm"
	}
	parent := portal.GetTopLevelParent()
	if parent != nil {
		bridgeInfo.Network = &event.BridgeInfoSection{
			ID:          string(parent.ID),
			DisplayName: parent.Name,
			AvatarURL:   parent.AvatarMXC,
			// TODO external URL?
		}
	}
	// TODO use something globally unique instead of bridge ID?
	//      maybe ask the matrix connector to use serverName+appserviceID+bridgeID
	stateKey := string(portal.BridgeID)
	return stateKey, bridgeInfo
}

func (portal *Portal) UpdateBridgeInfo(ctx context.Context) {
	if portal.MXID == "" {
		return
	}
	stateKey, bridgeInfo := portal.getBridgeInfo()
	portal.sendRoomMeta(ctx, nil, time.Now(), event.StateBridge, stateKey, &bridgeInfo)
	portal.sendRoomMeta(ctx, nil, time.Now(), event.StateHalfShotBridge, stateKey, &bridgeInfo)
}

func (portal *Portal) sendStateWithIntentOrBot(ctx context.Context, sender MatrixAPI, eventType event.Type, stateKey string, content *event.Content, ts time.Time) (resp *mautrix.RespSendEvent, err error) {
	if sender == nil {
		sender = portal.Bridge.Bot
	}
	resp, err = sender.SendState(ctx, portal.MXID, eventType, stateKey, content, ts)
	if errors.Is(err, mautrix.MForbidden) && sender != portal.Bridge.Bot {
		if content.Raw == nil {
			content.Raw = make(map[string]any)
		}
		content.Raw["fi.mau.bridge.set_by"] = sender.GetMXID()
		resp, err = portal.Bridge.Bot.SendState(ctx, portal.MXID, eventType, stateKey, content, ts)
	}
	return
}

func (portal *Portal) sendRoomMeta(ctx context.Context, sender MatrixAPI, ts time.Time, eventType event.Type, stateKey string, content any) bool {
	if portal.MXID == "" {
		return false
	}
	var extra map[string]any
	if !portal.NameIsCustom && (eventType == event.StateRoomName || eventType == event.StateRoomAvatar) {
		extra = map[string]any{
			"fi.mau.implicit_name": true,
		}
	}
	_, err := portal.sendStateWithIntentOrBot(ctx, sender, eventType, stateKey, &event.Content{
		Parsed: content,
		Raw:    extra,
	}, ts)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).
			Str("event_type", eventType.Type).
			Msg("Failed to set room metadata")
		return false
	}
	return true
}

func (portal *Portal) getInitialMemberList(ctx context.Context, members *ChatMemberList, source *UserLogin, pl *event.PowerLevelsEventContent) (invite, functional []id.UserID, err error) {
	if members == nil {
		invite = []id.UserID{source.UserMXID}
		return
	}
	var loginsInPortal []*UserLogin
	if members.CheckAllLogins {
		loginsInPortal, err = portal.Bridge.GetUserLoginsInPortal(ctx, portal.PortalKey)
		if err != nil {
			err = fmt.Errorf("failed to get user logins in portal: %w", err)
			return
		}
	}
	members.PowerLevels.Apply("", pl)
	for _, member := range members.Members {
		if member.Membership != event.MembershipJoin && member.Membership != "" {
			continue
		}
		if member.Sender != "" && member.UserInfo != nil {
			ghost, err := portal.Bridge.GetGhostByID(ctx, member.Sender)
			if err != nil {
				zerolog.Ctx(ctx).Err(err).Str("ghost_id", string(member.Sender)).Msg("Failed to get ghost from member list to update info")
			} else {
				ghost.UpdateInfo(ctx, member.UserInfo)
			}
		}
		intent, extraUserID := portal.getIntentAndUserMXIDFor(ctx, member.EventSender, source, loginsInPortal, 0)
		if extraUserID != "" {
			invite = append(invite, extraUserID)
			if member.PowerLevel != nil {
				pl.EnsureUserLevel(extraUserID, *member.PowerLevel)
			}
			if intent != nil {
				// If intent is present along with a user ID, it's the ghost of a logged-in user,
				// so add it to the functional members list
				functional = append(functional, intent.GetMXID())
			}
		}
		if intent != nil {
			invite = append(invite, intent.GetMXID())
			if member.PowerLevel != nil {
				pl.EnsureUserLevel(intent.GetMXID(), *member.PowerLevel)
			}
		}
	}
	portal.updateOtherUser(ctx, members)
	return
}

func (portal *Portal) updateOtherUser(ctx context.Context, members *ChatMemberList) (changed bool) {
	var expectedUserID networkid.UserID
	if portal.RoomType != database.RoomTypeDM {
		// expected user ID is empty
	} else if members.OtherUserID != "" {
		expectedUserID = members.OtherUserID
	} else if len(members.Members) == 2 && members.IsFull {
		if members.Members[0].IsFromMe && !members.Members[1].IsFromMe {
			expectedUserID = members.Members[1].Sender
		} else if members.Members[1].IsFromMe && !members.Members[0].IsFromMe {
			expectedUserID = members.Members[0].Sender
		}
	}
	if portal.OtherUserID != expectedUserID {
		zerolog.Ctx(ctx).Debug().
			Str("old_other_user_id", string(portal.OtherUserID)).
			Str("new_other_user_id", string(expectedUserID)).
			Msg("Updating other user ID in DM portal")
		portal.OtherUserID = expectedUserID
		return true
	}
	return false
}

func (portal *Portal) syncParticipants(ctx context.Context, members *ChatMemberList, source *UserLogin, sender MatrixAPI, ts time.Time) error {
	var loginsInPortal []*UserLogin
	var err error
	if members.CheckAllLogins {
		loginsInPortal, err = portal.Bridge.GetUserLoginsInPortal(ctx, portal.PortalKey)
		if err != nil {
			return fmt.Errorf("failed to get user logins in portal: %w", err)
		}
	}
	if sender == nil {
		sender = portal.Bridge.Bot
	}
	log := zerolog.Ctx(ctx)
	currentPower, err := portal.Bridge.Matrix.GetPowerLevels(ctx, portal.MXID)
	if err != nil {
		return fmt.Errorf("failed to get current power levels: %w", err)
	}
	currentMembers, err := portal.Bridge.Matrix.GetMembers(ctx, portal.MXID)
	if err != nil {
		return fmt.Errorf("failed to get current members: %w", err)
	}
	delete(currentMembers, portal.Bridge.Bot.GetMXID())
	powerChanged := members.PowerLevels.Apply(portal.Bridge.Bot.GetMXID(), currentPower)
	syncUser := func(extraUserID id.UserID, member ChatMember, hasIntent bool) bool {
		if member.Membership == "" {
			member.Membership = event.MembershipJoin
		}
		if member.PowerLevel != nil {
			powerChanged = currentPower.EnsureUserLevelAs(portal.Bridge.Bot.GetMXID(), extraUserID, *member.PowerLevel) || powerChanged
		}
		currentMember, ok := currentMembers[extraUserID]
		delete(currentMembers, extraUserID)
		if ok && currentMember.Membership == member.Membership {
			return false
		}
		if currentMember == nil {
			currentMember = &event.MemberEventContent{Membership: event.MembershipLeave}
		}
		if member.PrevMembership != "" && member.PrevMembership != currentMember.Membership {
			log.Trace().
				Stringer("user_id", extraUserID).
				Str("expected_prev_membership", string(member.PrevMembership)).
				Str("actual_prev_membership", string(currentMember.Membership)).
				Str("target_membership", string(member.Membership)).
				Msg("Not updating membership: prev membership mismatch")
			return false
		}
		content := &event.MemberEventContent{
			Membership:  member.Membership,
			Displayname: currentMember.Displayname,
			AvatarURL:   currentMember.AvatarURL,
		}
		wrappedContent := &event.Content{Parsed: content, Raw: make(map[string]any)}
		thisEvtSender := sender
		if member.Membership == event.MembershipJoin {
			content.Membership = event.MembershipInvite
			if hasIntent {
				wrappedContent.Raw["fi.mau.will_auto_accept"] = true
			}
			if thisEvtSender.GetMXID() == extraUserID {
				thisEvtSender = portal.Bridge.Bot
			}
		}
		if currentMember != nil && currentMember.Membership == event.MembershipBan && member.Membership != event.MembershipLeave {
			unbanContent := *content
			unbanContent.Membership = event.MembershipLeave
			wrappedUnbanContent := &event.Content{Parsed: &unbanContent}
			_, err = portal.sendStateWithIntentOrBot(ctx, thisEvtSender, event.StateMember, extraUserID.String(), wrappedUnbanContent, ts)
			if err != nil {
				log.Err(err).
					Stringer("target_user_id", extraUserID).
					Stringer("sender_user_id", thisEvtSender.GetMXID()).
					Str("prev_membership", string(currentMember.Membership)).
					Str("membership", string(member.Membership)).
					Msg("Failed to unban user to update membership")
			} else {
				log.Trace().
					Stringer("target_user_id", extraUserID).
					Stringer("sender_user_id", thisEvtSender.GetMXID()).
					Str("prev_membership", string(currentMember.Membership)).
					Str("membership", string(member.Membership)).
					Msg("Unbanned user to update membership")
			}
		}
		_, err = portal.sendStateWithIntentOrBot(ctx, thisEvtSender, event.StateMember, extraUserID.String(), wrappedContent, ts)
		if err != nil {
			log.Err(err).
				Stringer("target_user_id", extraUserID).
				Stringer("sender_user_id", thisEvtSender.GetMXID()).
				Str("prev_membership", string(currentMember.Membership)).
				Str("membership", string(member.Membership)).
				Msg("Failed to update user membership")
		} else {
			log.Trace().
				Stringer("target_user_id", extraUserID).
				Stringer("sender_user_id", thisEvtSender.GetMXID()).
				Str("prev_membership", string(currentMember.Membership)).
				Str("membership", string(member.Membership)).
				Msg("Updating membership in room")
		}
		return true
	}
	syncIntent := func(intent MatrixAPI, member ChatMember) {
		if !syncUser(intent.GetMXID(), member, true) {
			return
		}
		if member.Membership == event.MembershipJoin || member.Membership == "" {
			err = intent.EnsureJoined(ctx, portal.MXID)
			if err != nil {
				log.Err(err).
					Stringer("user_id", intent.GetMXID()).
					Msg("Failed to ensure user is joined to room")
			}
		}
	}
	for _, member := range members.Members {
		if member.Sender != "" && member.UserInfo != nil {
			ghost, err := portal.Bridge.GetGhostByID(ctx, member.Sender)
			if err != nil {
				zerolog.Ctx(ctx).Err(err).Str("ghost_id", string(member.Sender)).Msg("Failed to get ghost from member list to update info")
			} else {
				ghost.UpdateInfo(ctx, member.UserInfo)
			}
		}
		intent, extraUserID := portal.getIntentAndUserMXIDFor(ctx, member.EventSender, source, loginsInPortal, 0)
		if intent != nil {
			syncIntent(intent, member)
		}
		if extraUserID != "" {
			syncUser(extraUserID, member, false)
		}
	}
	if powerChanged {
		_, err = portal.sendStateWithIntentOrBot(ctx, sender, event.StatePowerLevels, "", &event.Content{Parsed: currentPower}, ts)
		if err != nil {
			log.Err(err).Msg("Failed to update power levels")
		}
	}
	portal.updateOtherUser(ctx, members)
	if members.IsFull {
		for extraMember, memberEvt := range currentMembers {
			if memberEvt.Membership == event.MembershipLeave || memberEvt.Membership == event.MembershipBan {
				continue
			}
			if !portal.Bridge.IsGhostMXID(extraMember) && portal.Relay != nil {
				continue
			}
			_, err = portal.Bridge.Bot.SendState(ctx, portal.MXID, event.StateMember, extraMember.String(), &event.Content{
				Parsed: &event.MemberEventContent{
					Membership:  event.MembershipLeave,
					AvatarURL:   memberEvt.AvatarURL,
					Displayname: memberEvt.Displayname,
					Reason:      "User is not in remote chat",
				},
			}, time.Now())
			if err != nil {
				zerolog.Ctx(ctx).Err(err).
					Stringer("user_id", extraMember).
					Msg("Failed to remove user from room")
			}
		}
	}
	return nil
}

func (portal *Portal) updateUserLocalInfo(ctx context.Context, info *UserLocalPortalInfo, source *UserLogin) {
	if portal.MXID == "" {
		return
	}
	dp := source.User.DoublePuppet(ctx)
	if dp == nil {
		return
	}
	dmMarkingMatrixAPI, canMarkDM := dp.(MarkAsDMMatrixAPI)
	if canMarkDM && portal.OtherUserID != "" && portal.RoomType == database.RoomTypeDM {
		dmGhost, err := portal.Bridge.GetGhostByID(ctx, portal.OtherUserID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to get DM ghost to mark room as DM")
		} else if err = dmMarkingMatrixAPI.MarkAsDM(ctx, portal.MXID, dmGhost.Intent.GetMXID()); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to mark room as DM")
		}
	}
	if info == nil {
		return
	}
	if info.MutedUntil != nil {
		err := dp.MuteRoom(ctx, portal.MXID, *info.MutedUntil)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to mute room")
		}
	}
	if info.Tag != nil {
		err := dp.TagRoom(ctx, portal.MXID, *info.Tag, *info.Tag != "")
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to tag room")
		}
	}
}

func (portal *Portal) UpdateDisappearingSetting(ctx context.Context, setting database.DisappearingSetting, sender MatrixAPI, ts time.Time, implicit, save bool) bool {
	if setting.Timer == 0 {
		setting.Type = ""
	}
	if portal.Disappear.Timer == setting.Timer && portal.Disappear.Type == setting.Type {
		return false
	}
	portal.Disappear.Type = setting.Type
	portal.Disappear.Timer = setting.Timer
	if save {
		err := portal.Save(ctx)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to save portal to database after updating disappearing setting")
		}
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    fmt.Sprintf("Disappearing messages set to %s", exfmt.Duration(setting.Timer)),
	}
	if implicit {
		content.Body = fmt.Sprintf("Automatically enabled disappearing message timer (%s) because incoming message is disappearing", exfmt.Duration(setting.Timer))
	} else if setting.Timer == 0 {
		content.Body = "Disappearing messages disabled"
	}
	if sender == nil {
		sender = portal.Bridge.Bot
	}
	_, err := sender.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: content,
	}, &MatrixSendExtra{Timestamp: ts})
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to send disappearing messages notice")
	} else {
		zerolog.Ctx(ctx).Debug().
			Dur("new_timer", portal.Disappear.Timer).
			Bool("implicit", implicit).
			Msg("Sent disappearing messages notice")
	}
	return true
}

func (portal *Portal) updateParent(ctx context.Context, newParent networkid.PortalID, source *UserLogin) bool {
	if portal.ParentID == newParent {
		return false
	}
	var err error
	if portal.MXID != "" && portal.InSpace && portal.Parent != nil && portal.Parent.MXID != "" {
		err = portal.toggleSpace(ctx, portal.Parent.MXID, false, true)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Stringer("old_space_mxid", portal.Parent.MXID).Msg("Failed to remove portal from old space")
		}
	}
	portal.ParentID = newParent
	portal.InSpace = false
	if newParent != "" {
		portal.Parent, err = portal.Bridge.GetPortalByKey(ctx, networkid.PortalKey{ID: newParent})
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to get new parent portal")
		}
	}
	if portal.MXID != "" && portal.Parent != nil && (source != nil || portal.Parent.MXID != "") {
		if portal.Parent.MXID == "" {
			zerolog.Ctx(ctx).Info().Msg("Parent portal doesn't exist, creating")
			err = portal.Parent.CreateMatrixRoom(ctx, source, nil)
			if err != nil {
				zerolog.Ctx(ctx).Err(err).Msg("Failed to create parent portal")
			}
		}
		if portal.Parent.MXID != "" {
			portal.addToParentSpaceAndSave(ctx, false)
		}
	}
	return true
}

func (portal *Portal) lockedUpdateInfoFromGhost(ctx context.Context, ghost *Ghost) {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	portal.UpdateInfoFromGhost(ctx, ghost)
}

func (portal *Portal) UpdateInfoFromGhost(ctx context.Context, ghost *Ghost) (changed bool) {
	if portal.NameIsCustom || !portal.Bridge.Config.PrivateChatPortalMeta || (portal.OtherUserID == "" && ghost == nil) || portal.RoomType != database.RoomTypeDM {
		return
	}
	var err error
	if ghost == nil {
		ghost, err = portal.Bridge.GetGhostByID(ctx, portal.OtherUserID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to get ghost to update info from")
			return
		}
	}
	changed = portal.updateName(ctx, ghost.Name, nil, time.Time{}) || changed
	changed = portal.updateAvatar(ctx, &Avatar{
		ID:     ghost.AvatarID,
		MXC:    ghost.AvatarMXC,
		Hash:   ghost.AvatarHash,
		Remove: ghost.AvatarID == "",
	}, nil, time.Time{}) || changed
	return
}

func (portal *Portal) UpdateInfo(ctx context.Context, info *ChatInfo, source *UserLogin, sender MatrixAPI, ts time.Time) {
	changed := false
	if info.Name != nil {
		portal.NameIsCustom = true
		changed = portal.updateName(ctx, *info.Name, sender, ts) || changed
	}
	if info.Topic != nil {
		changed = portal.updateTopic(ctx, *info.Topic, sender, ts) || changed
	}
	if info.Avatar != nil {
		portal.NameIsCustom = true
		changed = portal.updateAvatar(ctx, info.Avatar, sender, ts) || changed
	}
	if info.Disappear != nil {
		changed = portal.UpdateDisappearingSetting(ctx, *info.Disappear, sender, ts, false, false) || changed
	}
	if info.ParentID != nil {
		changed = portal.updateParent(ctx, *info.ParentID, source) || changed
	}
	if info.JoinRule != nil {
		// TODO change detection instead of spamming this every time?
		portal.sendRoomMeta(ctx, sender, ts, event.StateJoinRules, "", info.JoinRule)
	}
	if info.Type != nil && portal.RoomType != *info.Type {
		if portal.MXID != "" && (*info.Type == database.RoomTypeSpace || portal.RoomType == database.RoomTypeSpace) {
			zerolog.Ctx(ctx).Warn().
				Str("current_type", string(portal.RoomType)).
				Str("target_type", string(*info.Type)).
				Msg("Tried to change existing room type from/to space")
		} else {
			changed = true
			portal.RoomType = *info.Type
		}
	}
	if info.Members != nil && portal.MXID != "" && source != nil {
		err := portal.syncParticipants(ctx, info.Members, source, nil, time.Time{})
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to sync room members")
		}
		// TODO detect changes to functional members list?
	} else if info.Members != nil {
		portal.updateOtherUser(ctx, info.Members)
	}
	changed = portal.UpdateInfoFromGhost(ctx, nil) || changed
	if source != nil {
		source.MarkInPortal(ctx, portal)
		portal.updateUserLocalInfo(ctx, info.UserLocal, source)
	}
	if info.CanBackfill && source != nil {
		err := portal.Bridge.DB.BackfillTask.EnsureExists(ctx, portal.PortalKey, source.ID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to ensure backfill queue task exists")
		}
		// TODO wake up backfill queue if task was just created
	}
	if info.ExtraUpdates != nil {
		changed = info.ExtraUpdates(ctx, portal) || changed
	}
	if changed {
		portal.UpdateBridgeInfo(ctx)
		err := portal.Save(ctx)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to save portal to database after updating info")
		}
	}
}

func (portal *Portal) CreateMatrixRoom(ctx context.Context, source *UserLogin, info *ChatInfo) (retErr error) {
	if portal.MXID != "" {
		if source != nil {
			source.MarkInPortal(ctx, portal)
		}
		return nil
	}
	waiter := make(chan struct{})
	closed := false
	portal.events <- &portalCreateEvent{
		ctx:    ctx,
		source: source,
		info:   info,
		cb: func(err error) {
			retErr = err
			if !closed {
				closed = true
				close(waiter)
			}
		},
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-waiter:
		return
	}
}

func (portal *Portal) createMatrixRoomInLoop(ctx context.Context, source *UserLogin, info *ChatInfo) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if portal.MXID != "" {
		if source != nil {
			source.MarkInPortal(ctx, portal)
		}
		return nil
	}
	log := zerolog.Ctx(ctx).With().
		Str("action", "create matrix room").
		Logger()
	ctx = log.WithContext(ctx)
	log.Info().Msg("Creating Matrix room")

	var err error
	if info == nil || info.Members == nil {
		if info != nil {
			log.Warn().Msg("CreateMatrixRoom got info without members. Refetching info")
		}
		info, err = source.Client.GetChatInfo(ctx, portal)
		if err != nil {
			log.Err(err).Msg("Failed to update portal info for creation")
			return err
		}
	}

	portal.UpdateInfo(ctx, info, source, nil, time.Time{})

	powerLevels := &event.PowerLevelsEventContent{
		Events: map[string]int{
			event.StateTombstone.Type:  100,
			event.StateServerACL.Type:  100,
			event.StateEncryption.Type: 100,
		},
		Users: map[id.UserID]int{
			portal.Bridge.Bot.GetMXID(): 9001,
		},
	}
	initialMembers, extraFunctionalMembers, err := portal.getInitialMemberList(ctx, info.Members, source, powerLevels)
	if err != nil {
		log.Err(err).Msg("Failed to process participant list for portal creation")
		return err
	}
	powerLevels.EnsureUserLevel(portal.Bridge.Bot.GetMXID(), 9001)

	req := mautrix.ReqCreateRoom{
		Visibility:         "private",
		Name:               portal.Name,
		Topic:              portal.Topic,
		CreationContent:    make(map[string]any),
		InitialState:       make([]*event.Event, 0, 6),
		Preset:             "private_chat",
		IsDirect:           portal.RoomType == database.RoomTypeDM,
		PowerLevelOverride: powerLevels,
		BeeperLocalRoomID:  id.RoomID(fmt.Sprintf("!%s:%s", portal.ID, portal.Bridge.Matrix.ServerName())),
	}
	autoJoinInvites := portal.Bridge.Matrix.GetCapabilities().AutoJoinInvites
	if autoJoinInvites {
		req.BeeperInitialMembers = initialMembers
		// TODO remove this after initial_members is supported in hungryserv
		req.BeeperAutoJoinInvites = true
		req.Invite = initialMembers
	}
	if portal.RoomType == database.RoomTypeSpace {
		req.CreationContent["type"] = event.RoomTypeSpace
	}
	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()

	req.InitialState = append(req.InitialState, &event.Event{
		Type: event.StateElementFunctionalMembers,
		Content: event.Content{Parsed: &event.ElementFunctionalMembersContent{
			ServiceMembers: append(extraFunctionalMembers, portal.Bridge.Bot.GetMXID()),
		}},
	}, &event.Event{
		StateKey: &bridgeInfoStateKey,
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: &bridgeInfo},
	}, &event.Event{
		StateKey: &bridgeInfoStateKey,
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: &bridgeInfo},
	})
	if req.Topic == "" {
		// Add explicit topic event if topic is empty to ensure the event is set.
		// This ensures that there won't be an extra event later if PUT /state/... is called.
		req.InitialState = append(req.InitialState, &event.Event{
			Type:    event.StateTopic,
			Content: event.Content{Parsed: &event.TopicEventContent{Topic: ""}},
		})
	}
	if portal.AvatarMXC != "" {
		req.InitialState = append(req.InitialState, &event.Event{
			Type:    event.StateRoomAvatar,
			Content: event.Content{Parsed: &event.RoomAvatarEventContent{URL: portal.AvatarMXC}},
		})
	}
	if portal.Parent != nil && portal.Parent.MXID != "" {
		req.InitialState = append(req.InitialState, &event.Event{
			StateKey: ptr.Ptr(portal.Parent.MXID.String()),
			Type:     event.StateSpaceParent,
			Content: event.Content{Parsed: &event.SpaceParentEventContent{
				Via:       []string{portal.Bridge.Matrix.ServerName()},
				Canonical: true,
			}},
		})
	}
	if info.JoinRule != nil {
		req.InitialState = append(req.InitialState, &event.Event{
			Type:    event.StateJoinRules,
			Content: event.Content{Parsed: info.JoinRule},
		})
	}
	roomID, err := portal.Bridge.Bot.CreateRoom(ctx, &req)
	if err != nil {
		log.Err(err).Msg("Failed to create Matrix room")
		return err
	}
	log.Info().Stringer("room_id", roomID).Msg("Matrix room created")
	portal.AvatarSet = true
	portal.TopicSet = true
	portal.NameSet = true
	portal.MXID = roomID
	portal.Bridge.cacheLock.Lock()
	portal.Bridge.portalsByMXID[roomID] = portal
	portal.Bridge.cacheLock.Unlock()
	portal.updateLogger()
	err = portal.Save(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to save portal to database after creating Matrix room")
		return err
	}
	if info.CanBackfill {
		err = portal.Bridge.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{
			PortalKey:         portal.PortalKey,
			UserLoginID:       source.ID,
			NextDispatchMinTS: time.Now().Add(BackfillMinBackoffAfterRoomCreate),
		})
		if err != nil {
			log.Err(err).Msg("Failed to create backfill queue task after creating room")
		}
		portal.Bridge.WakeupBackfillQueue()
	}
	if portal.Parent != nil {
		if portal.Parent.MXID != "" {
			portal.addToParentSpaceAndSave(ctx, true)
		} else {
			log.Info().Msg("Parent portal doesn't exist, creating in background")
			go portal.createParentAndAddToSpace(ctx, source)
		}
	}
	portal.updateUserLocalInfo(ctx, info.UserLocal, source)
	if !autoJoinInvites {
		if info.Members == nil {
			dp := source.User.DoublePuppet(ctx)
			if dp != nil {
				err = dp.EnsureJoined(ctx, portal.MXID)
				if err != nil {
					log.Err(err).Msg("Failed to ensure user is joined to room after creation")
				}
			}
		} else {
			err = portal.syncParticipants(ctx, info.Members, source, nil, time.Time{})
			if err != nil {
				log.Err(err).Msg("Failed to sync participants after room creation")
			}
		}
	}
	if portal.Parent == nil {
		userPortals, err := portal.Bridge.DB.UserPortal.GetAllInPortal(ctx, portal.PortalKey)
		if err != nil {
			log.Err(err).Msg("Failed to get user logins in portal to add portal to spaces")
		} else {
			for _, up := range userPortals {
				login := portal.Bridge.GetCachedUserLoginByID(up.LoginID)
				if login != nil {
					login.inPortalCache.Remove(portal.PortalKey)
					go login.tryAddPortalToSpace(ctx, portal, up.CopyWithoutValues())
				}
			}
		}
	}
	portal.doForwardBackfill(ctx, source, nil)
	return nil
}

func (portal *Portal) Delete(ctx context.Context) error {
	err := portal.Bridge.DB.Portal.Delete(ctx, portal.PortalKey)
	if err != nil {
		return err
	}
	portal.Bridge.cacheLock.Lock()
	defer portal.Bridge.cacheLock.Unlock()
	portal.unlockedDeleteCache()
	return nil
}

func (portal *Portal) RemoveMXID(ctx context.Context) error {
	if portal.MXID == "" {
		return nil
	}
	portal.MXID = ""
	err := portal.Save(ctx)
	if err != nil {
		return err
	}
	portal.Bridge.cacheLock.Lock()
	defer portal.Bridge.cacheLock.Unlock()
	delete(portal.Bridge.portalsByMXID, portal.MXID)
	return nil
}

func (portal *Portal) unlockedDelete(ctx context.Context) error {
	// TODO delete child portals?
	err := portal.Bridge.DB.Portal.Delete(ctx, portal.PortalKey)
	if err != nil {
		return err
	}
	portal.unlockedDeleteCache()
	return nil
}

func (portal *Portal) unlockedDeleteCache() {
	delete(portal.Bridge.portalsByKey, portal.PortalKey)
	if portal.MXID != "" {
		delete(portal.Bridge.portalsByMXID, portal.MXID)
	}
}

func (portal *Portal) Save(ctx context.Context) error {
	return portal.Bridge.DB.Portal.Update(ctx, portal.Portal)
}

func (portal *Portal) SetRelay(ctx context.Context, relay *UserLogin) error {
	portal.Relay = relay
	if relay == nil {
		portal.RelayLoginID = ""
	} else {
		portal.RelayLoginID = relay.ID
	}
	err := portal.Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (portal *Portal) handleMatrixMembership(
	ctx context.Context,
	sender *UserLogin,
	origSender *OrigSender,
	evt *event.Event,
) {
	api, ok := sender.Client.(MembershipHandlingNetworkAPI)
	if !ok {
		portal.sendErrorStatus(ctx, evt, ErrMembershipNotSupported)
		return
	}
	log := zerolog.Ctx(ctx)
	targetMXID := id.UserID(*evt.StateKey)
	isSelf := sender.User.MXID == targetMXID
	var err error
	var targetUserLogin *UserLogin
	targetGhost, err := portal.Bridge.GetGhostByMXID(ctx, targetMXID)
	if err != nil {
		log.Err(err).Stringer("mxid", targetMXID).Msg("Failed to get target ghost")
		return
	}
	if targetGhost == nil {
		targetUser, err := portal.Bridge.GetUserByMXID(ctx, targetMXID)
		if err != nil {
			log.Err(err).Stringer("mxid", targetMXID).Msg("Failed to get target user")
			return
		}
		targetUserLogin, _, err = portal.FindPreferredLogin(ctx, targetUser, false)
		if err != nil {
			log.Err(err).Stringer("mxid", targetMXID).Msg("Failed to get target user login")
			return
		}
	}
	prevContent := &event.MemberEventContent{Membership: event.MembershipLeave}
	if evt.Unsigned.PrevContent != nil {
		_ = evt.Unsigned.PrevContent.ParseRaw(evt.Type)
		prevContent, _ = evt.Unsigned.PrevContent.Parsed.(*event.MemberEventContent)
	}

	content := evt.Content.AsMember()
	membershipChangeType := MembershipChangeType{From: prevContent.Membership, To: content.Membership, IsSelf: isSelf}
	if !portal.Bridge.Config.BridgeMatrixLeave && membershipChangeType == Leave {
		log.Debug().Msg("Dropping leave event")
		return
	}
	membershipChange := &MatrixMembershipChange{
		MatrixEventBase: MatrixEventBase[*event.MemberEventContent]{
			Event:      evt,
			Content:    content,
			Portal:     portal,
			OrigSender: origSender,
		},
		TargetGhost:     targetGhost,
		TargetUserLogin: targetUserLogin,
		Type:            membershipChangeType,
	}
	_, err = api.HandleMatrixMembership(ctx, membershipChange)
	if err != nil {
		log.Err(err).Msg("Failed to handle Matrix Membership Change")
		portal.sendErrorStatus(ctx, evt, err)
		return
	}
}
