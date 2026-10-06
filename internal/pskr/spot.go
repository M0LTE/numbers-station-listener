// Package pskr turns the PSKReporter MQTT spot feed into a path-open
// heuristic for ranking receivers.
//
// Numbers stations never appear in PSKReporter, so the feed says nothing
// about them directly. What it can say is whether HF is open between two
// regions right now: if amateurs near a station's transmitter site are
// being spotted by receivers near a candidate SDR, on amateur bands close
// to the station's frequency, that path is probably open for the station
// too.
//
// The feed is pskr2mqtt at mqtt.pskreporter.info (verified 2026-10-06):
//
//	pskr/filter/v2/{band}/{mode}/{senderCall}/{receiverCall}/{senderLoc4}/{receiverLoc4}/{senderAdif}/{receiverAdif}
//
// with a JSON payload such as
//
//	{"sq":73468726263,"f":10137366,"md":"FT8","rp":-11,"t":1791326355,"t_tx":1791326340,
//	 "sc":"WB1DX","sl":"FN32XE","rc":"PA4HJH","rl":"JO22UU","sa":291,"ra":263,"b":"30m"}
//
// The v2 tier only carries spots with a valid locator at both ends. Topic
// locators are cut to 4 characters; payload locators keep whatever the
// station sent (4 to 10 characters, either case). "ra" can be null, and its
// topic level is then empty.
package pskr

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Spot is one PSKReporter reception report.
type Spot struct {
	Seq    int64
	FreqHz int64
	Mode   string
	// SNR is the receiver's reported SNR in dB, when HasSNR.
	SNR    int
	HasSNR bool
	// Time is when the receiver says it heard the sender ("t"). It comes
	// from the station's clock, which can be badly wrong, so the window
	// in Store runs on arrival time instead.
	Time            time.Time
	SenderCall      string
	SenderLocator   string
	ReceiverCall    string
	ReceiverLocator string
	// SenderADIF and ReceiverADIF are ADIF DXCC entity codes, 0 when
	// unknown.
	SenderADIF   int
	ReceiverADIF int
	// Band is the feed's band name, such as "20m".
	Band string
}

type wireSpot struct {
	Seq  *int64  `json:"sq"`
	F    *int64  `json:"f"`
	Md   string  `json:"md"`
	Rp   *int    `json:"rp"`
	T    *int64  `json:"t"`
	Sc   string  `json:"sc"`
	Sl   string  `json:"sl"`
	Rc   string  `json:"rc"`
	Rl   string  `json:"rl"`
	Sa   *int    `json:"sa"`
	Ra   *int    `json:"ra"`
	Band string  `json:"b"`
}

// ParseSpot decodes one message payload. Unknown fields are ignored so the
// feed can grow. It fails on malformed JSON and on a spot with no sender or
// receiver locator, which is useless here.
func ParseSpot(payload []byte) (Spot, error) {
	var w wireSpot
	if err := json.Unmarshal(payload, &w); err != nil {
		return Spot{}, fmt.Errorf("pskr: payload: %w", err)
	}
	if w.Sl == "" || w.Rl == "" {
		return Spot{}, errors.New("pskr: payload: missing sender or receiver locator")
	}
	s := Spot{
		Mode:            w.Md,
		SenderCall:      w.Sc,
		SenderLocator:   w.Sl,
		ReceiverCall:    w.Rc,
		ReceiverLocator: w.Rl,
		Band:            w.Band,
	}
	if w.Seq != nil {
		s.Seq = *w.Seq
	}
	if w.F != nil {
		s.FreqHz = *w.F
	}
	if w.Rp != nil {
		s.SNR, s.HasSNR = *w.Rp, true
	}
	if w.T != nil {
		s.Time = time.Unix(*w.T, 0).UTC()
	}
	if w.Sa != nil {
		s.SenderADIF = *w.Sa
	}
	if w.Ra != nil {
		s.ReceiverADIF = *w.Ra
	}
	return s, nil
}
