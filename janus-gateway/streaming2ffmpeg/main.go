package main


import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"time"

	janus "github.com/notedit/janus-go"
	"github.com/pion/webrtc/v2"
	"github.com/pion/webrtc/v2/pkg/media"
	"github.com/at-wat/ebml-go/webm"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v2/pkg/media/samplebuilder"
	"github.com/pion/rtcp"
)

var (
	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp uint32
)

func saveToDisk(i media.Writer, track *webrtc.Track) {
	defer func() {
		if err := i.Close(); err != nil {
			panic(err)
		}
	}()

	for {
		packet, err := track.ReadRTP()
		if err != nil {
			panic(err)
		}

		if err := i.WriteRTP(packet); err != nil {
			panic(err)
		}
	}
}

func watchHandle(handle *janus.Handle) {
	// wait for event
	for {
		msg := <-handle.Events
		switch msg := msg.(type) {
		case *janus.SlowLinkMsg:
			fmt.Print("SlowLinkMsg type ", handle.ID)
		case *janus.MediaMsg:
			fmt.Print("MediaEvent type", msg.Type, " receiving ", msg.Receiving)
		case *janus.WebRTCUpMsg:
			fmt.Print("WebRTCUp type ", handle.ID)
		case *janus.HangupMsg:
			fmt.Print("HangupEvent type ", handle.ID)
		case *janus.EventMsg:
			fmt.Printf("EventMsg %+v", msg.Plugindata.Data)
		}
	}
}

func startFFmpeg(width, height int) {
	// Create a ffmpeg process that consumes MKV via stdin, and broadcasts out to Twitch
	ffmpeg := exec.Command("ffmpeg", "-y", "-re", "-i", "pipe:0", "-c:v", "libx264", "-preset", "veryfast", "-maxrate", "3000k", "-bufsize", "6000k", "-pix_fmt", "yuv420p", "-g", "50", "-c:a", "aac", "-b:a", "160k", "-ac", "2", "-ar", "44100", "-f", "matroska", "foobar.mkv") //nolint
	ffmpegIn, _ := ffmpeg.StdinPipe()
	ffmpegOut, _ := ffmpeg.StderrPipe()
	if err := ffmpeg.Start(); err != nil {
		panic(err)
	}

	go func() {
		scanner := bufio.NewScanner(ffmpegOut)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	}()

	ws, err := webm.NewSimpleBlockWriter(ffmpegIn,
		[]webm.TrackEntry{
			{
				Name:            "Audio",
				TrackNumber:     1,
				TrackUID:        12345,
				CodecID:         "A_OPUS",
				TrackType:       2,
				DefaultDuration: 20000000,
				Audio: &webm.Audio{
					SamplingFrequency: 48000.0,
					Channels:          2,
				},
			}, {
				Name:            "Video",
				TrackNumber:     2,
				TrackUID:        67890,
				CodecID:         "V_VP8",
				TrackType:       1,
				DefaultDuration: 33333333,
				Video: &webm.Video{
					PixelWidth:  uint64(width),
					PixelHeight: uint64(height),
				},
			},
		})
	if err != nil {
		panic(err)
	}

	fmt.Printf("WebM saver has started with video width=%d, height=%d\n", width, height)
	audioWriter = ws[0]
	videoWriter = ws[1]
}

// Parse Opus audio and Write to WebM
func pushOpus(rtpPacket *rtp.Packet) {
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

// Parse VP8 video and Write to WebM
func pushVP8(rtpPacket *rtp.Packet) {
	videoBuilder.Push(rtpPacket)

	for {
		sample := videoBuilder.Pop()
		if sample == nil {
			return
		}
		// Read VP8 header.
		videoKeyframe := (sample.Data[0]&0x1 == 0)
		if videoKeyframe {
			// Keyframe has frame information.
			raw := uint(sample.Data[6]) | uint(sample.Data[7])<<8 | uint(sample.Data[8])<<16 | uint(sample.Data[9])<<24
			width := int(raw & 0x3FFF)
			height := int((raw >> 16) & 0x3FFF)

			if videoWriter == nil || audioWriter == nil {
				// Initialize WebM saver using received frame size.
				startFFmpeg(width, height)
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
	// Everything below is the pion-WebRTC API! Thanks for using it ❤️.

	// Janus
	gateway, err := janus.Connect("ws://localhost:8188/")
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
	_, err = handle.Request(map[string]interface{}{
		"request": "list",
	})
	if err != nil {
		panic(err)
	}

	// Watch the second stream
	msg, err := handle.Message(map[string]interface{}{
		"request": "watch",
		"id":      1,
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

		audioBuilder = samplebuilder.New(10, &codecs.OpusPacket{})
		videoBuilder = samplebuilder.New(10, &codecs.VP8Packet{})


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
			fmt.Printf("Connection State has changed %s \n", connectionState.String())
		})

		peerConnection.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
			// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
			// This is a temporary fix until we implement incoming RTCP events, then we would push a PLI only when a viewer requests it
			go func() {
				ticker := time.NewTicker(time.Second * 3)
				for range ticker.C {
					rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}})
					if rtcpSendErr != nil {
						fmt.Println(rtcpSendErr)
					}
				}
			}()
	
			fmt.Printf("Track has started, of type %d: %s \n", track.PayloadType(), track.Codec().Name)
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
					pushVP8(rtp)
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
