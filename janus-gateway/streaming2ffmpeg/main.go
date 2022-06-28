package main


import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	janus "github.com/notedit/janus-go"
	"github.com/pion/webrtc/v2"
	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v2/pkg/media/samplebuilder"
	"github.com/pion/rtcp"
)

const (
	audioMaxLate = 32
	videoMaxLate = 256
)

var (
	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp uint32
)

func watchHandle(handle *janus.Handle) {
	// wait for event
	for {
		msg := <-handle.Events
		switch msg := msg.(type) {
		case *janus.SlowLinkMsg:
			fmt.Fprintln(os.Stderr, "SlowLinkMsg type ", handle.ID)
		case *janus.MediaMsg:
			fmt.Fprintln(os.Stderr, "MediaEvent type", msg.Type, " receiving ", msg.Receiving)
		case *janus.WebRTCUpMsg:
			fmt.Fprintln(os.Stderr, "WebRTCUp type ", handle.ID)
		case *janus.HangupMsg:
			fmt.Fprintln(os.Stderr, "HangupEvent type ", handle.ID)
		case *janus.EventMsg:
			fmt.Fprintf(os.Stderr, "EventMsg %+v\n", msg.Plugindata.Data)
		}
	}
}

func startOutput(videoMimeType string, width uint32, height uint32) {
	header := webm.DefaultEBMLHeader
	isWebm := videoMimeType == "video/vp9" || videoMimeType == "video/vp8"
	if !isWebm {
		h := *header
		h.DocType = "matroska"
		header = &h
	}

	interceptor, err := mkvcore.NewMultiTrackBlockSorter(
		// must be larger than the samplebuilder's MaxLate.
		mkvcore.WithMaxDelayedPackets(videoMaxLate+16),
		mkvcore.WithSortRule(mkvcore.BlockSorterWriteOutdated),
	)
	if err != nil {
		panic(err)
	}

	audioEntry := webm.TrackEntry{
		Name:        "Audio",
		TrackNumber: 1,
		CodecID:     "A_OPUS",
		TrackType:   2,
		Audio: &webm.Audio{
			SamplingFrequency: float64(48000),
			Channels:          uint64(2),
		},
	}

	var videoCodecID string;
	if videoMimeType == "video/h264" {
		videoCodecID = "V_MPEG4/ISO/AVC"
	} else if videoMimeType == "video/vp9" {
		videoCodecID = "V_VP9"
	} else if videoMimeType == "video/vp8" {
		videoCodecID = "V_VP8"
	} else {
		panic("Unexpected video codec")
	}


	videoEntry := webm.TrackEntry{
		Name:        "Video",
		TrackNumber: 2,
		CodecID:     videoCodecID,
		TrackType:   1,
		Video: &webm.Video{
			PixelWidth:  uint64(width),
			PixelHeight: uint64(height),
		},
	}
	var desc []mkvcore.TrackDescription

	desc = append(desc,
		mkvcore.TrackDescription{
			TrackNumber: 1,
			TrackEntry:  audioEntry,
		},
	)
	desc = append(desc,
		mkvcore.TrackDescription{
			TrackNumber: 2,
			TrackEntry:  videoEntry,
		},
	)

	if isWebm {
		tracks := []webm.TrackEntry{audioEntry, videoEntry}
		ws, err := webm.NewSimpleBlockWriter(os.Stdout, tracks,
			mkvcore.WithEBMLHeader(header),
			mkvcore.WithSegmentInfo(webm.DefaultSegmentInfo),
			mkvcore.WithBlockInterceptor(interceptor))
		if err != nil {
			panic(err)
		}
		audioWriter = ws[0]
		videoWriter = ws[1]
	} else {
		ws, err := mkvcore.NewSimpleBlockWriter(os.Stdout, desc,
			mkvcore.WithEBMLHeader(header),
			mkvcore.WithSegmentInfo(webm.DefaultSegmentInfo),
			mkvcore.WithBlockInterceptor(interceptor))
		if err != nil {
			panic(err)
		}
		audioWriter = ws[0]
		videoWriter = ws[1]
	}

	fmt.Fprintf(os.Stderr, "WebM saver has started with video width=%d, height=%d\n", width, height)
}

// Parse Opus audio and Write to WebM
func pushOpus(rtpPacket *rtp.Packet) {
	if audioBuilder == nil {
		audioBuilder = samplebuilder.New(audioMaxLate, &codecs.OpusPacket{})
	}
	audioBuilder.Push(rtpPacket)

	for {
		sample := audioBuilder.Pop()
		if sample == nil {
			return
		}
		if audioWriter != nil {
			audioTimestamp += sample.Samples
			t := audioTimestamp / 48
			if _, err := audioWriter.Write(true, int64(t), rtpPacket.Payload); err != nil {
				panic(err)
			}
		}
	}
}

// These come verbatim from galene, Copyright (c) 2020 by Juliusz Chroboczek (MIT License)
// source: https://github.com/jech/galene/blob/40bf93cdd04b71d66b94e68ccbff8265724485b9/codecs/codecs.go#L13

// Keyframe determines if packet is the start of a keyframe.
// It returns (true, true) if that is the case, (false, true) if that is
// definitely not the case, and (false, false) if the information cannot
// be determined.
func Keyframe(codec string, packet *rtp.Packet) (bool, bool) {
	if strings.EqualFold(codec, "video/vp8") {
		var vp8 codecs.VP8Packet
		_, err := vp8.Unmarshal(packet.Payload)
		if err != nil || len(vp8.Payload) < 1 {
			return false, false
		}

		if vp8.S != 0 && vp8.PID == 0 && (vp8.Payload[0]&0x1) == 0 {
			return true, true
		}
		return false, true
	} else if strings.EqualFold(codec, "video/vp9") {
		var vp9 codecs.VP9Packet
		_, err := vp9.Unmarshal(packet.Payload)
		if err != nil || len(vp9.Payload) < 1 {
			return false, false
		}
		if !vp9.B {
			return false, true
		}

		if (vp9.Payload[0] & 0xc0) != 0x80 {
			return false, false
		}

		profile := (vp9.Payload[0] >> 4) & 0x3
		if profile != 3 {
			return (vp9.Payload[0] & 0xC) == 0, true
		}
		return (vp9.Payload[0] & 0x6) == 0, true
	} else if strings.EqualFold(codec, "video/av1") {
		if len(packet.Payload) < 2 {
			return false, true
		}
		// Z=0, N=1
		if (packet.Payload[0] & 0x88) != 0x08 {
			return false, true
		}
		w := (packet.Payload[0] & 0x30) >> 4

		getObu := func(data []byte, last bool) ([]byte, int, bool) {
			if last {
				return data, len(data), false
			}
			offset := 0
			length := 0
			for {
				if len(data) <= offset {
					return nil, offset, offset > 0
				}
				l := data[offset]
				length |= int(l&0x7f) << (offset * 7)
				offset++
				if (l & 0x80) == 0 {
					break
				}
			}
			if len(data) < offset+length {
				return data[offset:], len(data), true
			}
			return data[offset : offset+length],
				offset + length, false
		}
		offset := 1
		i := 0
		for {
			obu, length, truncated :=
				getObu(packet.Payload[offset:], int(w) == i+1)
			if len(obu) < 1 {
				return false, false
			}
			tpe := (obu[0] & 0x38) >> 3
			switch i {
			case 0:
				// OBU_SEQUENCE_HEADER
				if tpe != 1 {
					return false, true
				}
			default:
				// OBU_FRAME_HEADER or OBU_FRAME
				if tpe == 3 || tpe == 6 {
					if len(obu) < 2 {
						return false, false
					}
					// show_existing_frame == 0
					if (obu[1] & 0x80) != 0 {
						return false, true
					}
					// frame_type == KEY_FRAME
					return (obu[1] & 0x60) == 0, true
				}
			}
			if truncated || i >= int(w) {
				// the first frame header is in a second
				// packet, give up.
				return false, false
			}
			offset += length
			i++
		}
	} else if strings.EqualFold(codec, "video/h264") {
		if len(packet.Payload) < 1 {
			return false, false
		}
		nalu := packet.Payload[0] & 0x1F
		if nalu == 0 {
			// reserved
			return false, false
		} else if nalu <= 23 {
			// simple NALU
			return nalu == 7, true
		} else if nalu == 24 || nalu == 25 || nalu == 26 || nalu == 27 {
			// STAP-A, STAP-B, MTAP16 or MTAP24
			i := 1
			if nalu == 25 || nalu == 26 || nalu == 27 {
				// skip DON
				i += 2
			}
			for i < len(packet.Payload) {
				if i+2 > len(packet.Payload) {
					return false, false
				}
				length := uint16(packet.Payload[i])<<8 |
					uint16(packet.Payload[i+1])
				i += 2
				if i+int(length) > len(packet.Payload) {
					return false, false
				}
				offset := 0
				if nalu == 26 {
					offset = 3
				} else if nalu == 27 {
					offset = 4
				}
				if offset >= int(length) {
					return false, false
				}
				n := packet.Payload[i+offset] & 0x1F
				if n == 7 {
					// All of our keyframes are found in this codepath (specfically nalu==24, STAP-A)
					// which seems to stem from using `rtph264pay` with `aggregate-mode=zero-latency` (will browsers do similar?)
					return true, true
				} else if n >= 24 {
					// is this legal?
					return false, false
				}
				i += int(length)
			}
			if i == len(packet.Payload) {
				return false, true
			}
			return false, false
		} else if nalu == 28 || nalu == 29 {
			// FU-A or FU-B
			if len(packet.Payload) < 2 {
				return false, false
			}
			if (packet.Payload[1] & 0x80) == 0 {
				// not a starting fragment
				return false, true
			}
			return (packet.Payload[1]&0x1F == 7), true
		}
		return false, false
	}
	return false, false
}

func KeyframeDimensions(codec string, packet *rtp.Packet) (uint32, uint32) {
	if strings.EqualFold(codec, "video/vp8") {
		var vp8 codecs.VP8Packet
		_, err := vp8.Unmarshal(packet.Payload)
		if err != nil {
			return 0, 0
		}
		if len(vp8.Payload) < 10 {
			return 0, 0
		}
		raw := uint32(vp8.Payload[6]) | uint32(vp8.Payload[7])<<8 |
			uint32(vp8.Payload[8])<<16 | uint32(vp8.Payload[9])<<24
		width := raw & 0x3FFF
		height := (raw >> 16) & 0x3FFF
		return width, height
	} else if strings.EqualFold(codec, "video/vp9") {
		if packet == nil {
			return 0, 0
		}
		var vp9 codecs.VP9Packet
		_, err := vp9.Unmarshal(packet.Payload)
		if err != nil {
			return 0, 0
		}
		if !vp9.V {
			return 0, 0
		}
		w := uint32(0)
		h := uint32(0)
		for i := range vp9.Width {
			if i >= len(vp9.Height) {
				break
			}
			if w < uint32(vp9.Width[i]) {
				w = uint32(vp9.Width[i])
			}
			if h < uint32(vp9.Height[i]) {
				h = uint32(vp9.Height[i])
			}
		}
		return w, h
	} else if strings.EqualFold(codec, "video/h264") {
		fmt.Fprintln(os.Stderr, "FIXME: parse SPS or find some lib that does?")
		return 0, 0
	} else {
		return 0, 0
	}
}

// Parse VP8 video and Write to WebM
func pushVP8(rtpPacket *rtp.Packet) {
	const videoMimeType = "video/vp8"
	if videoBuilder == nil {
		videoBuilder = samplebuilder.New(videoMaxLate, &codecs.VP8Packet{})
	}

	videoBuilder.Push(rtpPacket)

	for {
		sample := videoBuilder.Pop()
		if sample == nil {
			return
		}
		// Read VP8 header.
		videoKeyframe, _ := Keyframe(videoMimeType, rtpPacket)
		if videoKeyframe {
			// Keyframe has frame information.
			width, height := KeyframeDimensions(videoMimeType, rtpPacket)

			if videoWriter == nil || audioWriter == nil {
				// Initialize WebM saver using received frame size.
				startOutput(videoMimeType, width, height)
			}
		}
		if videoWriter != nil {
			videoTimestamp += sample.Samples
			t := videoTimestamp / 90
			if _, err := videoWriter.Write(videoKeyframe, int64(t), sample.Data); err != nil {
				panic(err)
			}
		}
	}
}

// Parse H264 video and Write to MKV
func pushH264(rtpPacket *rtp.Packet) {
	const videoMimeType = "video/h264"
	if videoBuilder == nil {
		videoBuilder = samplebuilder.New(videoMaxLate, &codecs.H264Packet{})
	}
	videoBuilder.Push(rtpPacket)

	for {
		sample := videoBuilder.Pop()
		if sample == nil {
			return
		}
		// Read H264 header.
		videoKeyframe, videoKeyframeKnown := Keyframe(videoMimeType, rtpPacket)
		if videoKeyframe && videoKeyframeKnown{
			// Keyframe has frame information.
			/* FIXME: actually get these from bitstream */
			width, height := KeyframeDimensions(videoMimeType, rtpPacket)

			fmt.Fprintln(os.Stderr, "Got H.264 key frame", width, "x", height)
			if videoWriter == nil || audioWriter == nil {
				// Initialize WebM saver using received frame size.
				startOutput(videoMimeType, width, height)
			}
		}
		if videoWriter != nil {
			videoTimestamp += sample.Samples
			t := videoTimestamp / 90
			if _, err := videoWriter.Write(videoKeyframe, int64(t), sample.Data); err != nil {
				panic(err)
			}
		}
	}
}

// Parse VP9 video and Write to WebM
func pushVP9(rtpPacket *rtp.Packet) {
	const videoMimeType = "video/vp9"
	if videoBuilder == nil {
		videoBuilder = samplebuilder.New(videoMaxLate, &codecs.VP9Packet{})
	}
	videoBuilder.Push(rtpPacket)

	for {
		sample := videoBuilder.Pop()
		if sample == nil {
			return
		}
		// Read VP9 header.
		videoKeyframe, _ := Keyframe(videoMimeType, rtpPacket)
		if videoKeyframe {
			// Keyframe has frame information.
			width, height := KeyframeDimensions(videoMimeType, rtpPacket)

			if videoWriter == nil || audioWriter == nil {
				// Initialize WebM saver using received frame size.
				startOutput(videoMimeType, width, height)
			}
		}
		if videoWriter != nil {
			videoTimestamp += sample.Samples
			t := videoTimestamp / 90
			if _, err := videoWriter.Write(videoKeyframe, int64(t), sample.Data); err != nil {
				panic(err)
			}
		}
	}
}

func main() {

	var streamUrl string
	flag.StringVar(&streamUrl, "url", "", "wss stream URL")
	flag.Parse()

	u, err := url.Parse(streamUrl)
	if err != nil {
		panic(err)
	}
	// extract last slug from path (which is free of query params or trailing parameters)
	streamId := path.Base(u.Path)

	// Everything below is the pion-WebRTC API! Thanks for using it ❤️.

	// Janus
	gateway, err := janus.Connect(streamUrl)
	if err != nil {
		panic(err)
	}

	// Create session
	session, err := gateway.Create()
	if err != nil {
		panic(err)
	}

	// Create handle
	handle, err := session.Attach("janus.plugin.streaming")
	if err != nil {
		panic(err)
	}

	go watchHandle(handle)

	// Get streaming list
	listMsg, err := handle.Request(map[string]interface{}{
		"request": "list",
	})
	if err != nil {
		panic(err)
	}

	fmt.Fprintln(os.Stderr, "Got data", listMsg.PluginData.Data["list"])

	// Watch the second stream
	msg, err := handle.Message(map[string]interface{}{
		"request": "watch",
		"id":      streamId,
	}, nil)
	if err != nil {
		panic(err)
	}

	if msg.Jsep != nil {
		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  msg.Jsep["sdp"].(string),
		}

		mediaEngine := webrtc.MediaEngine{}
		if err = mediaEngine.PopulateFromSDP(offer); err != nil {
			panic(err)
		}

		// Create a new RTCPeerConnection
		var peerConnection *webrtc.PeerConnection
		peerConnection, err = webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs: []string{"stun:stun.l.google.com:19302"},
				},
			},
			SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
		})
		if err != nil {
			panic(err)
		}

		// Allow us to receive 1 audio track, and 1 video track
		if _, err = peerConnection.AddTransceiver(webrtc.RTPCodecTypeAudio); err != nil {
			panic(err)
		} else if _, err = peerConnection.AddTransceiver(webrtc.RTPCodecTypeVideo); err != nil {
			panic(err)
		}

		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			fmt.Fprintf(os.Stderr, "Connection State has changed %s\n", connectionState.String())
		})

		peerConnection.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
			// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
			// This is a temporary fix until we implement incoming RTCP events, then we would push a PLI only when a viewer requests it
			go func() {
				ticker := time.NewTicker(time.Second * 3)
				for range ticker.C {
					rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}})
					if rtcpSendErr != nil {
						fmt.Fprintln(os.Stderr, rtcpSendErr)
					}
				}
			}()
	
			fmt.Fprintf(os.Stderr, "Track has started, of type %d: %s \n", track.PayloadType(), track.Codec().Name)
			for {
				// Read RTP packets being sent to Pion
				rtp, readErr := track.ReadRTP()
				if readErr != nil {
					if readErr == io.EOF {
						return
					}
					panic(readErr)
				}
				switch track.Kind() {
				case webrtc.RTPCodecTypeAudio:
					pushOpus(rtp)
				case webrtc.RTPCodecTypeVideo:
					if track.Codec().Name == "H264" {
						pushH264(rtp)
					} else if track.Codec().Name == "VP9" {
						pushVP9(rtp)
					} else if track.Codec().Name == "VP8" {
						pushVP8(rtp)
					} else {
						fmt.Fprintln(os.Stderr, "Unexpected video codec", track.Codec().Name)
					}
				}
			}
		})

		if err = peerConnection.SetRemoteDescription(offer); err != nil {
			panic(err)
		}

		answer, answerErr := peerConnection.CreateAnswer(nil)
		if answerErr != nil {
			panic(answerErr)
		}

		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		// now we start
		_, err = handle.Message(map[string]interface{}{
			"request": "start",
		}, map[string]interface{}{
			"type":    "answer",
			"sdp":     answer.SDP,
			"trickle": false,
		})
		if err != nil {
			panic(err)
		}
	}
	for {
		_, err = session.KeepAlive()
		if err != nil {
			panic(err)
		}

		time.Sleep(5 * time.Second)
	}
}
