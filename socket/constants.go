// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package socket implements a subset of the Noise protocol framework on top of websockets as used by WhatsApp.
package socket

import "errors"

const (
	// Origin is the Origin header for all WhatsApp websocket connections
	Origin = "https://web.whatsapp.com"
	// URL is the websocket URL for the new multidevice protocol
	URL = "wss://web.whatsapp.com/ws/chat"
)

const (
	NoiseStartPattern = "Noise_XX_25519_AESGCM_SHA256\x00\x00\x00\x00"

	WADictVersion = 2
	WAMagicValue  = 5
)

var WAConnHeader = []byte{'W', 'A', WAMagicValue, WADictVersion}

const (
	FrameMaxSize    = 2 << 23
	FrameLengthSize = 3
)

var (
	ErrFrameTooLarge     = errors.New("frame too large")
	ErrSocketClosed      = errors.New("frame socket is closed")
	ErrSocketAlreadyOpen = errors.New("frame socket is already open")
)