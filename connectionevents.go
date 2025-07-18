// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (cli *Client) handleStreamError(node *waBinary.Node) {
	ctx := cli.BackgroundEventCtx
	cli.isLoggedIn.Store(false)
	cli.clearResponseWaiters(node)
	code, _ := node.Attrs["code"].(string)
	conflict, _ := node.GetOptionalChildByTag("conflict")
	conflictType := conflict.AttrGetter().OptionalString("type")
	switch {
	case code == "515":
		if cli.DisableLoginAutoReconnect {
			cli.Log.Infof("Got 515 code, but login autoreconnect is disabled, not reconnecting")
			cli.dispatchEvent(&events.ManualLoginReconnect{})
			return
		}
		cli.Log.Infof("Got 515 code, reconnecting...")
		go func() {
			cli.Disconnect()
			err := cli.connect()
			if err != nil {
				cli.Log.Errorf("Failed to reconnect after 515 code: %v", err)
			}
		}()
	case code == "401" && conflictType == "device_removed":
		cli.expectDisconnect()
		cli.Log.Infof("Got device removed stream error, sending LoggedOut event and deleting session")
		go cli.dispatchEvent(&events.LoggedOut{OnConnect: false, Reason: events.ConnectFailureLoggedOut})
		err := cli.Store.Delete(ctx)
		if err != nil {
			cli.Log.Warnf("Failed to delete store after device_removed error: %v", err)
		}
	case conflictType == "replaced":
		cli.expectDisconnect()
		cli.Log.Infof("Got replaced stream error, sending StreamReplaced event")
		go cli.dispatchEvent(&events.StreamReplaced{})
	case code == "503":
		// This seems to happen when the server wants to restart or something.
		// The disconnection will be emitted as an events.Disconnected and then the auto-reconnect will do its thing.
		cli.Log.Warnf("Got 503 stream error, assuming automatic reconnect will handle it")
	case cli.RefreshCAT != nil && (code == events.ConnectFailureCATInvalid.NumberString() || code == events.ConnectFailureCATExpired.NumberString()):
		cli.Log.Infof("Got %s stream error, refreshing CAT before reconnecting...", code)
		cli.socketLock.RLock()
		defer cli.socketLock.RUnlock()
		err := cli.RefreshCAT(ctx)
		if err != nil {
			cli.Log.Errorf("Failed to refresh CAT: %v", err)
			cli.expectDisconnect()
			go cli.dispatchEvent(&events.CATRefreshError{Error: err})
		}
	default:
		cli.Log.Errorf("Unknown stream error: %s", node.XMLString())
		go cli.dispatchEvent(&events.StreamError{Code: code, Raw: node})
	}
}

func (cli *Client) handleIB(node *waBinary.Node) {
	children := node.GetChildren()
	for _, child := range children {
		ag := child.AttrGetter()
		switch child.Tag {
		case "downgrade_webclient":
			go cli.dispatchEvent(&events.QRScannedWithoutMultidevice{})
		case "offline_preview":
			cli.dispatchEvent(&events.OfflineSyncPreview{
				Total:          ag.Int("count"),
				AppDataChanges: ag.Int("appdata"),
				Messages:       ag.Int("message"),
				Notifications:  ag.Int("notification"),
				Receipts:       ag.Int("receipt"),
			})
		case "offline":
			cli.dispatchEvent(&events.OfflineSyncCompleted{
				Count: ag.Int("count"),
			})
		}
	}
}

func (cli *Client) handleConnectFailure(node *waBinary.Node) {
	ctx := cli.BackgroundEventCtx
	ag := node.AttrGetter()
	reason := events.ConnectFailureReason(ag.Int("reason"))
	message := ag.OptionalString("message")
	willAutoReconnect := true
	switch {
	default:
		// By default, expect a disconnect (i.e. prevent auto-reconnect)
		cli.expectDisconnect()
		willAutoReconnect = false
	case reason == events.ConnectFailureServiceUnavailable || reason == events.ConnectFailureInternalServerError:
		// Auto-reconnect for 503s
	case reason == events.ConnectFailureCATInvalid || reason == events.ConnectFailureCATExpired:
		// Auto-reconnect when rotating CAT, lock socket to ensure refresh goes through before reconnect
		cli.socketLock.RLock()
		defer cli.socketLock.RUnlock()
	}
	if reason == 403 {
		cli.Log.Debugf(
			"Message for 403 connect failure: %s / %s",
			ag.OptionalString("logout_message_header"),
			ag.OptionalString("logout_message_subtext"),
		)
	}
	if reason.IsLoggedOut() {
		cli.Log.Infof("Got %s connect failure, sending LoggedOut event and deleting session", reason)
		go cli.dispatchEvent(&events.LoggedOut{OnConnect: true, Reason: reason})
		err := cli.Store.Delete(ctx)
		if err != nil {
			cli.Log.Warnf("Failed to delete store after %d failure: %v", int(reason), err)
		}
	} else if reason == events.ConnectFailureTempBanned {
		cli.Log.Warnf("Temporary ban connect failure: %s", node.XMLString())
		go cli.dispatchEvent(&events.TemporaryBan{
			Code:   events.TempBanReason(ag.Int("code")),
			Expire: time.Duration(ag.Int("expire")) * time.Second,
		})
	} else if reason == events.ConnectFailureClientOutdated {
		cli.Log.Errorf("Client outdated (405) connect failure (client version: %s)", store.GetWAVersion().String())
		go cli.dispatchEvent(&events.ClientOutdated{})
	} else if reason == events.ConnectFailureCATInvalid || reason == events.ConnectFailureCATExpired {
		cli.Log.Infof("Got %d/%s connect failure, refreshing CAT before reconnecting...", int(reason), message)
		err := cli.RefreshCAT(ctx)
		if err != nil {
			cli.Log.Errorf("Failed to refresh CAT: %v", err)
			cli.expectDisconnect()
			go cli.dispatchEvent(&events.CATRefreshError{Error: err})
		}
	} else if willAutoReconnect {
		cli.Log.Warnf("Got %d/%s connect failure, assuming automatic reconnect will handle it", int(reason), message)
	} else {
		cli.Log.Warnf("Unknown connect failure: %s", node.XMLString())
		go cli.dispatchEvent(&events.ConnectFailure{Reason: reason, Message: message, Raw: node})
	}
}

func (cli *Client) handleConnectSuccess(node *waBinary.Node) {
	ctx := cli.BackgroundEventCtx
	cli.Log.Infof("Successfully authenticated")
	cli.LastSuccessfulConnect = time.Now()
	cli.AutoReconnectErrors = 0
	cli.isLoggedIn.Store(true)
	nodeLID := node.AttrGetter().JID("lid")
	if !cli.Store.LID.IsEmpty() && !nodeLID.IsEmpty() && cli.Store.LID != nodeLID {
		// This should probably never happen, but check just in case.
		cli.Log.Warnf("Stored LID doesn't match one in connect success: %s != %s", cli.Store.LID, nodeLID)
		cli.Store.LID = types.EmptyJID
	}
	if cli.Store.LID.IsEmpty() && !nodeLID.IsEmpty() {
		cli.Store.LID = nodeLID
		err := cli.Store.Save(ctx)
		if err != nil {
			cli.Log.Warnf("Failed to save device after updating LID: %v", err)
		} else {
			cli.Log.Infof("Updated LID to %s", cli.Store.LID)
		}
	}
	// Some users are missing their own LID-PN mapping even though it's already in the device table,
	// so do this unconditionally for a few months to ensure everyone gets the row.
	cli.StoreLIDPNMapping(ctx, cli.Store.GetLID(), cli.Store.GetJID())
	go func() {
		if dbCount, err := cli.Store.PreKeys.UploadedPreKeyCount(ctx); err != nil {
			cli.Log.Errorf("Failed to get number of prekeys in database: %v", err)
		} else if serverCount, err := cli.getServerPreKeyCount(ctx); err != nil {
			cli.Log.Warnf("Failed to get number of prekeys on server: %v", err)
		} else {
			cli.Log.Debugf("Database has %d prekeys, server says we have %d", dbCount, serverCount)
			if serverCount < MinPreKeyCount || dbCount < MinPreKeyCount {
				cli.uploadPreKeys(ctx)
				sc, _ := cli.getServerPreKeyCount(ctx)
				cli.Log.Debugf("Prekey count after upload: %d", sc)
			}
		}
		err := cli.SetPassive(ctx, false)
		if err != nil {
			cli.Log.Warnf("Failed to send post-connect passive IQ: %v", err)
		}
		cli.dispatchEvent(&events.Connected{})
		cli.closeSocketWaitChan()
	}()
}

// SetPassive tells the WhatsApp server whether this device is passive or not.
//
// This seems to mostly affect whether the device receives certain events.
// By default, whatsmeow will automatically do SetPassive(false) after connecting.
func (cli *Client) SetPassive(ctx context.Context, passive bool) error {
	tag := "active"
	if passive {
		tag = "passive"
	}
	_, err := cli.sendIQ(infoQuery{
		Namespace: "passive",
		Type:      "set",
		To:        types.ServerJID,
		Context:   ctx,
		Content:   []waBinary.Node{{Tag: tag}},
	})
	if err != nil {
		return err
	}
	return nil
}
