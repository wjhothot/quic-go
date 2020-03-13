package qlog

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/francoispqt/gojay"

	"github.com/lucas-clemente/quic-go/internal/congestion"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/wire"
)

const eventChanSize = 50

// A Tracer records events to be exported to a qlog.
type Tracer interface {
	Export() error
	StartedConnection(t time.Time, local, remote net.Addr, version protocol.VersionNumber, srcConnID, destConnID protocol.ConnectionID)
	SentPacket(t time.Time, hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, ack *wire.AckFrame, frames []wire.Frame)
	ReceivedRetry(time.Time, *wire.Header)
	ReceivedPacket(t time.Time, hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, frames []wire.Frame)
	BufferedPacket(time.Time, PacketType)
	UpdatedMetrics(t time.Time, rttStats *congestion.RTTStats, cwnd protocol.ByteCount, bytesInFLight protocol.ByteCount, packetsInFlight int)
	LostPacket(time.Time, protocol.EncryptionLevel, protocol.PacketNumber, PacketLossReason)
	UpdatedPTOCount(time.Time, uint32)
	UpdatedKeyFromTLS(time.Time, protocol.EncryptionLevel, protocol.Perspective)
	UpdatedKey(t time.Time, generation protocol.KeyPhase, remote bool)
}

type tracer struct {
	w           io.WriteCloser
	odcid       protocol.ConnectionID
	perspective protocol.Perspective

	suffix     []byte
	events     chan event
	encodeErr  error
	runStopped chan struct{}
}

var _ Tracer = &tracer{}

// NewTracer creates a new tracer to record a qlog.
func NewTracer(w io.WriteCloser, p protocol.Perspective, odcid protocol.ConnectionID) Tracer {
	t := &tracer{
		w:           w,
		perspective: p,
		odcid:       odcid,
		runStopped:  make(chan struct{}),
		events:      make(chan event, eventChanSize),
	}
	go t.run()
	return t
}

func (t *tracer) run() {
	defer close(t.runStopped)
	buf := &bytes.Buffer{}
	enc := gojay.NewEncoder(buf)
	tl := &topLevel{
		traces: traces{
			{
				VantagePoint: vantagePoint{Type: t.perspective},
				CommonFields: commonFields{ODCID: connectionID(t.odcid), GroupID: connectionID(t.odcid)},
				EventFields:  eventFields[:],
			},
		}}
	if err := enc.Encode(tl); err != nil {
		panic(fmt.Sprintf("qlog encoding into a bytes.Buffer failed: %s", err))
	}
	data := buf.Bytes()
	t.suffix = data[buf.Len()-4:]
	if _, err := t.w.Write(data[:buf.Len()-4]); err != nil {
		t.encodeErr = err
	}
	enc = gojay.NewEncoder(t.w)
	isFirst := true
	for ev := range t.events {
		if t.encodeErr != nil { // if encoding failed, just continue draining the event channel
			continue
		}
		if !isFirst {
			t.w.Write([]byte(","))
		}
		if err := enc.Encode(ev); err != nil {
			t.encodeErr = err
		}
		isFirst = false
	}
}

// Export writes a qlog.
func (t *tracer) Export() error {
	close(t.events)
	<-t.runStopped
	if t.encodeErr != nil {
		return t.encodeErr
	}
	if _, err := t.w.Write(t.suffix); err != nil {
		return err
	}
	return t.w.Close()
}

func (t *tracer) StartedConnection(time time.Time, local, remote net.Addr, version protocol.VersionNumber, srcConnID, destConnID protocol.ConnectionID) {
	// ignore this event if we're not dealing with UDP addresses here
	localAddr, ok := local.(*net.UDPAddr)
	if !ok {
		return
	}
	remoteAddr, ok := remote.(*net.UDPAddr)
	if !ok {
		return
	}
	t.events <- event{
		Time: time,
		eventDetails: eventConnectionStarted{
			SrcAddr:          localAddr,
			DestAddr:         remoteAddr,
			Version:          version,
			SrcConnectionID:  srcConnID,
			DestConnectionID: destConnID,
		},
	}
}

func (t *tracer) SentPacket(time time.Time, hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, ack *wire.AckFrame, frames []wire.Frame) {
	numFrames := len(frames)
	if ack != nil {
		numFrames++
	}
	fs := make([]frame, 0, numFrames)
	if ack != nil {
		fs = append(fs, *transformFrame(ack))
	}
	for _, f := range frames {
		fs = append(fs, *transformFrame(f))
	}
	header := *transformExtendedHeader(hdr)
	header.PacketSize = packetSize
	t.events <- event{
		Time: time,
		eventDetails: eventPacketSent{
			PacketType: PacketTypeFromHeader(&hdr.Header),
			Header:     header,
			Frames:     fs,
		},
	}
}

func (t *tracer) ReceivedPacket(time time.Time, hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, frames []wire.Frame) {
	fs := make([]frame, len(frames))
	for i, f := range frames {
		fs[i] = *transformFrame(f)
	}
	header := *transformExtendedHeader(hdr)
	header.PacketSize = packetSize
	t.events <- event{
		Time: time,
		eventDetails: eventPacketReceived{
			PacketType: PacketTypeFromHeader(&hdr.Header),
			Header:     header,
			Frames:     fs,
		},
	}
}

func (t *tracer) ReceivedRetry(time time.Time, hdr *wire.Header) {
	t.events <- event{
		Time: time,
		eventDetails: eventRetryReceived{
			Header: *transformHeader(hdr),
		},
	}
}

func (t *tracer) BufferedPacket(time time.Time, packetType PacketType) {
	t.events <- event{
		Time:         time,
		eventDetails: eventPacketBuffered{PacketType: packetType},
	}
}

func (t *tracer) UpdatedMetrics(time time.Time, rttStats *congestion.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	t.events <- event{
		Time: time,
		eventDetails: eventMetricsUpdated{
			MinRTT:           rttStats.MinRTT(),
			SmoothedRTT:      rttStats.SmoothedRTT(),
			LatestRTT:        rttStats.LatestRTT(),
			RTTVariance:      rttStats.MeanDeviation(),
			CongestionWindow: cwnd,
			BytesInFlight:    bytesInFlight,
			PacketsInFlight:  packetsInFlight,
		},
	}
}

func (t *tracer) LostPacket(time time.Time, encLevel protocol.EncryptionLevel, pn protocol.PacketNumber, lossReason PacketLossReason) {
	t.events <- event{
		Time: time,
		eventDetails: eventPacketLost{
			PacketType:   getPacketTypeFromEncryptionLevel(encLevel),
			PacketNumber: pn,
			Trigger:      lossReason,
		},
	}
}

func (t *tracer) UpdatedPTOCount(time time.Time, value uint32) {
	t.events <- event{
		Time:         time,
		eventDetails: eventUpdatedPTO{Value: value},
	}
}

func (t *tracer) UpdatedKeyFromTLS(time time.Time, encLevel protocol.EncryptionLevel, pers protocol.Perspective) {
	t.events <- event{
		Time: time,
		eventDetails: eventKeyUpdated{
			Trigger: keyUpdateTLS,
			KeyType: encLevelToKeyType(encLevel, pers),
		},
	}
}

func (t *tracer) UpdatedKey(time time.Time, generation protocol.KeyPhase, remote bool) {
	trigger := keyUpdateLocal
	if remote {
		trigger = keyUpdateRemote
	}
	t.events <- event{
		Time: time,
		eventDetails: eventKeyUpdated{
			Trigger:    trigger,
			KeyType:    keyTypeClient1RTT,
			Generation: generation,
		},
	}
	t.events <- event{
		Time: time,
		eventDetails: eventKeyUpdated{
			Trigger:    trigger,
			KeyType:    keyTypeServer1RTT,
			Generation: generation,
		},
	}
}
