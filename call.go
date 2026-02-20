// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"fmt"
	"strconv"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (cli *Client) handleCallEvent(ctx context.Context, node *waBinary.Node) {
	defer cli.maybeDeferredAck(ctx, node)()

	if len(node.GetChildren()) != 1 {
		cli.dispatchEvent(&events.UnknownCallEvent{Node: node})
		return
	}
	ag := node.AttrGetter()
	child := node.GetChildren()[0]
	cag := child.AttrGetter()
	basicMeta := types.BasicCallMeta{
		From:        ag.JID("from"),
		Timestamp:   ag.UnixTime("t"),
		CallCreator: cag.JID("call-creator"),
		CallID:      cag.String("call-id"),
		GroupJID:    cag.OptionalJIDOrEmpty("group-jid"),
	}
	if basicMeta.CallCreator.Server == types.HiddenUserServer {
		basicMeta.CallCreatorAlt = cag.OptionalJIDOrEmpty("caller_pn")
	} else {
		// This may not actually exist
		basicMeta.CallCreatorAlt = cag.OptionalJIDOrEmpty("caller_lid")
	}
	switch child.Tag {
	case "offer":
		cli.dispatchEvent(&events.CallOffer{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "offer_notice":
		cli.dispatchEvent(&events.CallOfferNotice{
			BasicCallMeta: basicMeta,
			Media:         cag.String("media"),
			Type:          cag.String("type"),
			Data:          &child,
		})
	case "relaylatency":
		cli.dispatchEvent(&events.CallRelayLatency{
			BasicCallMeta: basicMeta,
			Data:          &child,
		})
	case "accept":
		cli.dispatchEvent(&events.CallAccept{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "preaccept":
		cli.dispatchEvent(&events.CallPreAccept{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "transport":
		cli.dispatchEvent(&events.CallTransport{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "terminate":
		cli.dispatchEvent(&events.CallTerminate{
			BasicCallMeta: basicMeta,
			Reason:        cag.String("reason"),
			Data:          &child,
		})
	case "reject":
		cli.dispatchEvent(&events.CallReject{
			BasicCallMeta: basicMeta,
			Data:          &child,
		})
	default:
		cli.dispatchEvent(&events.UnknownCallEvent{Node: node})
	}
}

// RejectCall reject an incoming call.
func (cli *Client) RejectCall(ctx context.Context, callFrom types.JID, callID string) error {
	ownID := cli.getOwnID()
	if ownID.IsEmpty() {
		return ErrNotLoggedIn
	}
	ownID, callFrom = ownID.ToNonAD(), callFrom.ToNonAD()
	return cli.sendNode(ctx, waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"id": cli.GenerateMessageID(), "from": ownID, "to": callFrom},
		Content: []waBinary.Node{{
			Tag:     "reject",
			Attrs:   waBinary.Attrs{"call-id": callID, "call-creator": callFrom, "count": "0"},
			Content: nil,
		}},
	})
}

type CallLinkType string

const (
	CallLinkTypeAudio CallLinkType = "audio"
	CallLinkTypeVideo CallLinkType = "video"

	CallLinkAudioPrefix = "https://call.whatsapp.com/voice/"
	CallLinkVideoPrefix = "https://call.whatsapp.com/video/"
)

// CreateCallLink asks WhatsApp for a call link token.
//
// For scheduled events, pass the event start time as the third parameter.
//
//	token, err := cli.CreateCallLink(ctx, whatsmeow.CallLinkTypeVideo, eventStart)
//	link := whatsmeow.CallLinkVideoPrefix + token
func (cli *Client) CreateCallLink(ctx context.Context, mediaType CallLinkType, eventStartTime ...time.Time) (string, error) {
	if cli == nil {
		return "", ErrClientIsNil
	}
	switch mediaType {
	case CallLinkTypeAudio, CallLinkTypeVideo:
	default:
		return "", fmt.Errorf("unsupported call link media type %q", mediaType)
	}

	reqID := cli.generateRequestID()
	respChan := cli.waitResponse(reqID)

	linkCreate := waBinary.Node{
		Tag: "link_create",
		Attrs: waBinary.Attrs{
			"media": string(mediaType),
		},
	}
	if len(eventStartTime) > 0 && !eventStartTime[0].IsZero() {
		linkCreate.Content = []waBinary.Node{{
			Tag: "event",
			Attrs: waBinary.Attrs{
				"start_time": strconv.FormatInt(eventStartTime[0].Unix(), 10),
			},
		}}
	}

	data, err := cli.sendNodeAndGetData(ctx, waBinary.Node{
		Tag: "call",
		Attrs: waBinary.Attrs{
			"id": reqID,
			"to": "@call",
		},
		Content: []waBinary.Node{linkCreate},
	})
	if err != nil {
		cli.cancelResponse(reqID, respChan)
		return "", err
	}

	timeout := defaultRequestTimeout
	var resp *waBinary.Node
	select {
	case resp = <-respChan:
	case <-ctx.Done():
		cli.cancelResponse(reqID, respChan)
		return "", ctx.Err()
	case <-time.After(timeout):
		cli.cancelResponse(reqID, respChan)
		return "", ErrIQTimedOut
	}
	if isDisconnectNode(resp) {
		resp, err = cli.retryFrame(ctx, "call link creation", reqID, data, resp, timeout)
		if err != nil {
			return "", err
		}
	}

	linkCreateResp, ok := resp.GetOptionalChildByTag("link_create")
	if !ok {
		return "", fmt.Errorf("unexpected call link response: %s", resp.XMLString())
	}
	token := linkCreateResp.AttrGetter().OptionalString("token")
	if token == "" {
		return "", fmt.Errorf("call link response didn't contain token: %s", resp.XMLString())
	}
	return token, nil
}
