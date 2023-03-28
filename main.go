// This program forwards WebRTC streams from Unreal Engine pixel streaming over RTP to some arbitrary receiever.
// This program uses websockets to connect to Unreal Engine pixel streaming through the intermediate signalling server ("cirrus").
// This program then uses Pion WebRTC to receive video/audio from Unreal Engine and the forwards those RTP streams
// to a specified address and ports. This is a proof of concept that is designed so FFPlay can receive these RTP streams.
// This program is a heavily modified version of: https://github.com/pion/webrtc/tree/master/examples/rtp-forwarder

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"log"
	"net"
	"sync"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msgs/sensor_msgs"
	"github.com/aler9/gortsplib/v2/pkg/format"
	"github.com/aler9/gortsplib/v2/pkg/url"
	"github.com/gorilla/websocket"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/x264"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// CirrusPort - The port of the Cirrus signalling server that the Pixel Streaming instance is connected to.
var CirrusPort = flag.Int("CirrusPort", 80, "The port of the Cirrus signalling server that the Pixel Streaming instance is connected to.")

// CirrusAddress - The address of the Cirrus signalling server that the Pixel Streaming instance is connected to.10.187.89.92
var CirrusAddress = flag.String("CirrusAddress", "192.168.66.67", "The address of the Cirrus signalling server that the Pixel Streaming instance is connected to.")

// RTPVideoPayloadType - The payload type of the RTP packet, 125 is H264 constrained baseline 2.0 in Chrome, with packetization mode of 1.
var RTPVideoPayloadType = flag.Uint("RTPVideoPayloadType", 125, "The payload type of the RTP packet, 125 is H264 constrained baseline in Chrome.")

// RTCPIntervalMs - How often (ms) to send RTCP messages (such as REMB, PLI)
var RTCPIntervalMs = flag.Int("RTCPIntervalMs", 2000, "How often (ms) to send RTCP message such as REMB, PLI.")

// Whether or not to send PLI messages on an interval.
var RTCPSendPLI = flag.Bool("RTCPSendPLI", true, "Whether or not to send PLI messages on an interval.")

// Whether or not to send REMB messages on an interval.
var RTCPSendREMB = flag.Bool("RTCPSendREMB", true, "Whether or not to send REMB messages on an interval.")

// Receiver-side estimated maximum bitrate.
var REMB float32 = 1000000

var pub *goroslib.Publisher

var packet_buffer []rtp.Packet
var lock sync.Mutex

type udpConn struct {
	conn        *net.UDPConn
	port        int
	payloadType uint8
}

type ueICECandidateResp struct {
	Type      string                  `json:"type"`
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

// Allows compressing offer/answer to bypass terminal input limits.
const compress = false

func writeWSMessage(wsConn *websocket.Conn, msg string) {
	err := wsConn.WriteMessage(websocket.TextMessage, []byte(msg))
	if err != nil {
		log.Println("Error writing websocket message: ", err)
	}
}

func createOffer(peerConnection *webrtc.PeerConnection) (string, error) {
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		log.Println("Error creating peer connection offer: ", err)
		return "", err
	}

	if err = peerConnection.SetLocalDescription(offer); err != nil {
		log.Println("Error setting local description of peer connection: ", err)
		return "", err
	}

	offerStringBytes, err := json.Marshal(offer)
	if err != nil {
		log.Println("Error unmarshalling json from offer object: ", err)
		return "", err
	}
	offerString := string(offerStringBytes)
	return offerString, err
}

func createPeerConnection() (*webrtc.PeerConnection, error) {
	// Create a MediaEngine object to configure the supported codec
	m := webrtc.MediaEngine{}

	// This sets up H.264, OPUS, etc.
	// m.RegisterDefaultCodecs()
	x264Params, err := x264.NewParams()
	x264Params.Preset = x264.PresetFaster
	x264Params.BitRate = 500_000 // 1mbps

	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&x264Params),
	)
	
	codecSelector.Populate(&m)

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))

	// Prepare the configuration
	// UE is using unified plan on the backend so we should too
	config := webrtc.Configuration{SDPSemantics: webrtc.SDPSemanticsUnifiedPlan}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)

	if err != nil {
		log.Println("Error making new peer connection: ", err)
		return nil, err
	}

	// Allow us to receive 1 audio track, and 1 video track in the "recvonly" mode
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RtpTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		log.Println("Error adding RTP video transceiver: ", err)
		return nil, err
	}

	return peerConnection, err
}

// Pion has recieved an "answer" from the remote Unreal Engine Pixel Streaming (through Cirrus)
// Pion will now set its remote session description that it got from the answer.
// Once Pion has its own local session description and the remote session description set
// then it should begin signalling the ice candidates it got from the Unreal Engine side.
// This flow is based on:
// https://github.com/pion/webrtc/blob/687d915e05a69441beae1bba0802e28756eecbbc/examples/pion-to-pion/offer/main.go#L90
func handleRemoteAnswer(message []byte, peerConnection *webrtc.PeerConnection, wsConn *websocket.Conn, pendingCandidates *[]*webrtc.ICECandidate) {
	sdp := webrtc.SessionDescription{}
	unmarshalError := json.Unmarshal([]byte(message), &sdp)

	if unmarshalError != nil {
		log.Printf("Error occured during unmarshaling sdp. Error: %s", unmarshalError.Error())
		return
	}

	// Set remote session description we got from UE pixel streaming
	if sdpErr := peerConnection.SetRemoteDescription(sdp); sdpErr != nil {
		log.Printf("Error occured setting remote session description. Error: %s", sdpErr.Error())
		return
	}
	fmt.Println("Added session description from UE to Pion.")

	// User websocket to send our local ICE candidates to UE
	for _, localIceCandidate := range *pendingCandidates {
		sendLocalIceCandidate(wsConn, localIceCandidate)
	}
}

// Pion has received an ice candidate from the remote Unreal Engine Pixel Streaming (through Cirrus).
// We parse this message and add that ice candidate to our peer connection.
// Flow based on: https://github.com/pion/webrtc/blob/687d915e05a69441beae1bba0802e28756eecbbc/examples/pion-to-pion/offer/main.go#L82
func handleRemoteIceCandidate(message []byte, peerConnection *webrtc.PeerConnection) {
	var iceCandidateInit webrtc.ICECandidateInit
	jsonErr := json.Unmarshal(message, &iceCandidateInit)
	if jsonErr != nil {
		log.Printf("Error unmarshaling ice candidate. Error: %s", jsonErr.Error())
		return
	}

	// The actual adding of the remote ice candidate happens here.
	if candidateErr := peerConnection.AddICECandidate(iceCandidateInit); candidateErr != nil {
		log.Printf("Error adding remote ice candidate. Error: %s", candidateErr.Error())
		return
	}

	fmt.Println(fmt.Sprintf("Added remote ice candidate from UE - %s", iceCandidateInit.Candidate))
}

// Starts an infinite loop where we poll for new websocket messages and react to them.
func startControlLoop(wsConn *websocket.Conn, peerConnection *webrtc.PeerConnection, pendingCandidates *[]*webrtc.ICECandidate) {
	// Start loop here to read web socket messages
	for {

		messageType, message, err := wsConn.ReadMessage()
		if err != nil {
			log.Printf("Websocket read message error: %v", err)
			log.Printf("Closing Pion websocket control loop.")
			wsConn.Close()
			break
		}
		stringMessage := string(message)

		// We print the recieved messages in a different colour so they are easier to distinguish.
		colorGreen := "\033[32m"
		colorReset := "\033[0m"
		fmt.Println(string(colorGreen), fmt.Sprintf("Received message, (type=%d): %s", messageType, stringMessage), string(colorReset))

		// Transform the raw bytes into a map of string: []byte pairs, we can unmarshall each key/value as needed.
		var objmap map[string]json.RawMessage
		err = json.Unmarshal(message, &objmap)

		if err != nil {
			log.Printf("Error unmarshalling bytes from websocket message. Error: %s", err.Error())
			continue
		}

		// Get the type of message we received from the Unreal Engine side
		var pixelStreamingMessageType string
		err = json.Unmarshal(objmap["type"], &pixelStreamingMessageType)

		if err != nil {
			log.Printf("Error unmarshaling type from pixel streaming message. Error: %s", err.Error())
			continue
		}

		// Based on the "type" of message we received, we react accordingly.
		switch pixelStreamingMessageType {
		case "playerCount":
			var playerCount int
			err = json.Unmarshal(objmap["count"], &playerCount)
			if err != nil {
				log.Printf("Error unmarshaling player count. Error: %s", err.Error())
			}
			fmt.Println(fmt.Sprintf("Player count is: %d", playerCount))
		case "config":
			fmt.Println("Got config message, ToDO: react based on config that was passed.")
		case "answer":
			handleRemoteAnswer(message, peerConnection, wsConn, pendingCandidates)
		case "iceCandidate":
			candidateMsg := objmap["candidate"]
			handleRemoteIceCandidate(candidateMsg, peerConnection)
		default:
			log.Println("Got message we do not specifically handle, type was: " + pixelStreamingMessageType)
		}

	}
}

// Send an "offer" string over websocket to Unreal Engine to start the WebRTC handshake.
func sendOffer(wsConn *websocket.Conn, peerConnection *webrtc.PeerConnection) {

	offerString, err := createOffer(peerConnection)

	if err != nil {
		log.Printf("Error creating offer. Error: %s", err.Error())
	} else {
		// Write our offer over websocket: "{"type":"offer","sdp":"v=0\r\no=- 2927396662845926191 2 IN IP4 127.0.0.1....."
		writeWSMessage(wsConn, offerString)
		fmt.Println("Sending offer...")
		fmt.Println(offerString)
	}
}

// Send our local ICE candidate to Unreal Engine using websockets.
func sendLocalIceCandidate(wsConn *websocket.Conn, localIceCandidate *webrtc.ICECandidate) {
	var iceCandidateInit webrtc.ICECandidateInit = localIceCandidate.ToJSON()
	var respPayload ueICECandidateResp = ueICECandidateResp{Type: "iceCandidate", Candidate: iceCandidateInit}

	jsonPayload, err := json.Marshal(respPayload)

	if err != nil {
		log.Printf("Error turning local ice candidate into JSON. Error: %s", err.Error())
	}

	jsonStr := string(jsonPayload)
	writeWSMessage(wsConn, jsonStr)
	fmt.Println(fmt.Sprintf("Sending our local ice candidate to UE...%s", jsonStr))
}

func setupMediaForwarding(peerConnection *webrtc.PeerConnection) {

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

		var trackType string = track.Kind().String()
		fmt.Println(fmt.Sprintf("Got %s track from Unreal Engine Pixel Streaming WebRTC.", trackType))

		// Send RTCP message on an interval to the UE side. a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		go func() {
			ticker := time.NewTicker(time.Millisecond * 2000)
			for range ticker.C {

				// Send PLI (picture loss indicator)
				if *RTCPSendPLI {
					if rtcpErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); rtcpErr != nil {
						fmt.Println(rtcpErr)
					}
				}

				// Send REMB (receiver-side estimated maximum bandwidth)
				if *RTCPSendREMB {
					if rtcpErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: REMB, SSRCs: []uint32{uint32(track.SSRC())}}}); rtcpErr != nil {
						fmt.Println(rtcpErr)
					}
				}
			}
		}()

		var err error
		rtpPacket := &rtp.Packet{}
		for {
			// Read
			rtpPacket, _, err = track.ReadRTP()
			if err != nil {
				panic(err)
			}

			if track.Kind().String() == "video" {
				if lock.TryLock() {
					packet_buffer = append(packet_buffer, *rtpPacket.Clone())
					lock.Unlock()
				}
			}
		}

	})
}

func deplete_buffer() {
	// go deplete_ros_buffer()
	var packet rtp.Packet

	// find the H264 media and format
	var forma = format.H264{
		PayloadTyp: 125,
	}

	// setup RTP/H264->H264 decoder
	rtpDec := forma.CreateDecoder()

	// setup H264->raw frames decoder
	h264RawDec, err := newH264Decoder()
	if err != nil {
		panic(err)
	}
	defer h264RawDec.close()

	ros_img := &sensor_msgs.Image{}
	ros_img.Encoding = "rgba8"
	start_stamp := time.Now()
	var counter int
	var overflow bool
	for {
		if len(packet_buffer) == 0 {
			continue
		}

		lock.Lock()
		// pop front
		packet, packet_buffer = packet_buffer[0], packet_buffer[1:]
		lock.Unlock()

		// Decode rtp packet
		nalus, stamp, err := rtpDec.Decode(&packet)
		if err != nil {
			// if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
			// 	log.Printf("ERR: %v", err)
			// }
			continue
		}
		
		
		for _, nalu := range nalus {
			// convert NALUs into RGBA frames
			img, err := h264RawDec.decode(nalu)
			if err != nil {
				panic(err)
			}
			
			// Skip video frames i10.187.89.92f getting behind
			if stamp != 0{
				diff := (time.Now().Sub(start_stamp).Milliseconds() - stamp.Milliseconds())
				if counter <= 0 {
					overflow = false
					counter = 2
				}
				if diff > 500 || overflow {
					overflow = true
					counter--
					continue
				}
			}

			// wait for a frame
			if img == nil {
				continue
			}
			
			// Cast to image.RGBA and send over ros
			if img_ok, ok := img.(*image.RGBA); ok {
				ros_img.Data = img_ok.Pix
				ros_img.Height = uint32(img_ok.Rect.Dy())
				ros_img.Width = uint32(img_ok.Rect.Dx())
				ros_img.Header.Stamp = time.Now()
				pub.Write(ros_img)
			}
		}

	}
}

func main() {
	flag.Parse()

	// Setup ros node
	n, err := goroslib.NewNode(goroslib.NodeConf{
		Name:          "goroslib_pub",
		MasterAddress: "localhost:11311",
	})

	if err != nil {
		panic(err)
	}
	defer n.Close()

	// create a publisher
	pub, err = goroslib.NewPublisher(goroslib.PublisherConf{
		Node:  n,
		Topic: "test_topic",
		Msg:   &sensor_msgs.Image{},
	})
	if err != nil {
		panic(err)
	}
	defer pub.Close()

	// Setup a websocket connection between this application and the Cirrus webserver.
	serverURL := url.URL{Scheme: "ws", Host: fmt.Sprintf("%s:%d", *CirrusAddress, *CirrusPort), Path: "/"}
	wsConn, _, err := websocket.DefaultDialer.Dial(serverURL.String(), nil)
	if err != nil {
		log.Fatal("Websocket dialing error: ", err)
		return
	}

	defer wsConn.Close()

	peerConnection, err := createPeerConnection()
	if err != nil {
		panic(err)
	}

	// Store our local ice candidates that we will transmit to UE
	pendingCandidates := make([]*webrtc.ICECandidate, 0)

	// Setup a callback to capture our local ice candidates when they are ready
	// Note: can happen at random times so might be before or after we have sent offer.
	peerConnection.OnICECandidate(func(localIceCandidate *webrtc.ICECandidate) {
		if localIceCandidate == nil {
			return
		}

		desc := peerConnection.RemoteDescription()
		if desc == nil {
			pendingCandidates = append(pendingCandidates, localIceCandidate)
			fmt.Println("Added local ICE candidate that we will send off later...")
		} else {
			sendLocalIceCandidate(wsConn, localIceCandidate)
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {

		colorPurple := "\033[35m"
		colorReset := "\033[0m"

		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println(string(colorPurple), "Connected to UE Pixel Streaming!", string(colorReset))
		} else if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateDisconnected {
			fmt.Println(string(colorPurple), "Disconnected from UE Pixel Streaming.", string(colorReset))
		}
	})

	setupMediaForwarding(peerConnection)

	// Run a goroutine to deplete rtp buffer
	go deplete_buffer()

	sendOffer(wsConn, peerConnection)
	startControlLoop(wsConn, peerConnection, &pendingCandidates)

}
