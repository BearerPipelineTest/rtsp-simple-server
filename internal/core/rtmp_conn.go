package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/h264"
	"github.com/aler9/gortsplib/pkg/ringbuffer"
	"github.com/aler9/gortsplib/pkg/rtpaac"
	"github.com/aler9/gortsplib/pkg/rtph264"
	"github.com/notedit/rtmp/av"
	nh264 "github.com/notedit/rtmp/codec/h264"
	"github.com/pion/rtcp"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/externalcmd"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/rtcpsenderset"
	"github.com/aler9/rtsp-simple-server/internal/rtmp"
)

const (
	rtmpConnPauseAfterAuthError = 2 * time.Second
)

func pathNameAndQuery(inURL *url.URL) (string, url.Values, string) {
	// remove leading and trailing slashes inserted by OBS and some other clients
	tmp := strings.TrimRight(inURL.String(), "/")
	ur, _ := url.Parse(tmp)
	pathName := strings.TrimLeft(ur.Path, "/")
	return pathName, ur.Query(), ur.RawQuery
}

type rtmpConnState int

const (
	rtmpConnStateIdle rtmpConnState = iota //nolint:deadcode,varcheck
	rtmpConnStateRead
	rtmpConnStatePublish
)

type rtmpConnTrackIDDataPair struct {
	trackID int
	data    *data
}

type rtmpConnPathManager interface {
	onReaderSetupPlay(req pathReaderSetupPlayReq) pathReaderSetupPlayRes
	onPublisherAnnounce(req pathPublisherAnnounceReq) pathPublisherAnnounceRes
}

type rtmpConnParent interface {
	log(logger.Level, string, ...interface{})
	onConnClose(*rtmpConn)
}

type rtmpConn struct {
	id                        string
	externalAuthenticationURL string
	rtspAddress               string
	readTimeout               conf.StringDuration
	writeTimeout              conf.StringDuration
	readBufferCount           int
	runOnConnect              string
	runOnConnectRestart       bool
	wg                        *sync.WaitGroup
	conn                      *rtmp.Conn
	externalCmdPool           *externalcmd.Pool
	pathManager               rtmpConnPathManager
	parent                    rtmpConnParent

	ctx        context.Context
	ctxCancel  func()
	path       *path
	ringBuffer *ringbuffer.RingBuffer // read
	state      rtmpConnState
	stateMutex sync.Mutex
}

func newRTMPConn(
	parentCtx context.Context,
	id string,
	externalAuthenticationURL string,
	rtspAddress string,
	readTimeout conf.StringDuration,
	writeTimeout conf.StringDuration,
	readBufferCount int,
	runOnConnect string,
	runOnConnectRestart bool,
	wg *sync.WaitGroup,
	nconn net.Conn,
	externalCmdPool *externalcmd.Pool,
	pathManager rtmpConnPathManager,
	parent rtmpConnParent) *rtmpConn {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	c := &rtmpConn{
		id:                        id,
		externalAuthenticationURL: externalAuthenticationURL,
		rtspAddress:               rtspAddress,
		readTimeout:               readTimeout,
		writeTimeout:              writeTimeout,
		readBufferCount:           readBufferCount,
		runOnConnect:              runOnConnect,
		runOnConnectRestart:       runOnConnectRestart,
		wg:                        wg,
		conn:                      rtmp.NewServerConn(nconn),
		externalCmdPool:           externalCmdPool,
		pathManager:               pathManager,
		parent:                    parent,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
	}

	c.log(logger.Info, "opened")

	c.wg.Add(1)
	go c.run()

	return c
}

// Close closes a Conn.
func (c *rtmpConn) close() {
	c.ctxCancel()
}

// ID returns the ID of the Conn.
func (c *rtmpConn) ID() string {
	return c.id
}

// RemoteAddr returns the remote address of the Conn.
func (c *rtmpConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *rtmpConn) log(level logger.Level, format string, args ...interface{}) {
	c.parent.log(level, "[conn %v] "+format, append([]interface{}{c.conn.RemoteAddr()}, args...)...)
}

func (c *rtmpConn) ip() net.IP {
	return c.conn.RemoteAddr().(*net.TCPAddr).IP
}

func (c *rtmpConn) safeState() rtmpConnState {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	return c.state
}

func (c *rtmpConn) run() {
	defer c.wg.Done()

	err := func() error {
		if c.runOnConnect != "" {
			c.log(logger.Info, "runOnConnect command started")
			_, port, _ := net.SplitHostPort(c.rtspAddress)
			onConnectCmd := externalcmd.NewCmd(
				c.externalCmdPool,
				c.runOnConnect,
				c.runOnConnectRestart,
				externalcmd.Environment{
					"RTSP_PATH": "",
					"RTSP_PORT": port,
				},
				func(co int) {
					c.log(logger.Info, "runOnConnect command exited with code %d", co)
				})

			defer func() {
				onConnectCmd.Close()
				c.log(logger.Info, "runOnConnect command stopped")
			}()
		}

		ctx, cancel := context.WithCancel(c.ctx)
		runErr := make(chan error)
		go func() {
			runErr <- c.runInner(ctx)
		}()

		select {
		case err := <-runErr:
			cancel()
			return err

		case <-c.ctx.Done():
			cancel()
			<-runErr
			return errors.New("terminated")
		}
	}()

	c.ctxCancel()

	c.parent.onConnClose(c)

	c.log(logger.Info, "closed (%v)", err)
}

func (c *rtmpConn) runInner(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		c.conn.Close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
	c.conn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
	err := c.conn.ServerHandshake()
	if err != nil {
		return err
	}

	if c.conn.IsPublishing() {
		return c.runPublish(ctx)
	}
	return c.runRead(ctx)
}

func (c *rtmpConn) runRead(ctx context.Context) error {
	pathName, query, rawQuery := pathNameAndQuery(c.conn.URL())

	res := c.pathManager.onReaderSetupPlay(pathReaderSetupPlayReq{
		author:   c,
		pathName: pathName,
		authenticate: func(
			pathIPs []interface{},
			pathUser conf.Credential,
			pathPass conf.Credential) error {
			return c.authenticate(pathName, pathIPs, pathUser, pathPass, "read", query, rawQuery)
		},
	})

	if res.err != nil {
		if terr, ok := res.err.(pathErrAuthCritical); ok {
			// wait some seconds to stop brute force attacks
			<-time.After(rtmpConnPauseAfterAuthError)
			return errors.New(terr.message)
		}
		return res.err
	}

	c.path = res.path

	defer func() {
		c.path.onReaderRemove(pathReaderRemoveReq{author: c})
	}()

	c.stateMutex.Lock()
	c.state = rtmpConnStateRead
	c.stateMutex.Unlock()

	var videoTrack *gortsplib.TrackH264
	videoTrackID := -1
	var audioTrack *gortsplib.TrackAAC
	audioTrackID := -1
	var aacDecoder *rtpaac.Decoder

	for i, track := range res.stream.tracks() {
		switch tt := track.(type) {
		case *gortsplib.TrackH264:
			if videoTrack != nil {
				return fmt.Errorf("can't read track %d with RTMP: too many tracks", i+1)
			}

			videoTrack = tt
			videoTrackID = i

		case *gortsplib.TrackAAC:
			if audioTrack != nil {
				return fmt.Errorf("can't read track %d with RTMP: too many tracks", i+1)
			}

			audioTrack = tt
			audioTrackID = i
			aacDecoder = rtpaac.NewDecoder(track.ClockRate())
		}
	}

	if videoTrack == nil && audioTrack == nil {
		return fmt.Errorf("the stream doesn't contain an H264 track or an AAC track")
	}

	c.conn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
	err := c.conn.WriteTracks(videoTrack, audioTrack)
	if err != nil {
		return err
	}

	c.ringBuffer = ringbuffer.New(uint64(c.readBufferCount))

	go func() {
		<-ctx.Done()
		c.ringBuffer.Close()
	}()

	c.path.onReaderPlay(pathReaderPlayReq{
		author: c,
	})

	if c.path.Conf().RunOnRead != "" {
		c.log(logger.Info, "runOnRead command started")
		onReadCmd := externalcmd.NewCmd(
			c.externalCmdPool,
			c.path.Conf().RunOnRead,
			c.path.Conf().RunOnReadRestart,
			c.path.externalCmdEnv(),
			func(co int) {
				c.log(logger.Info, "runOnRead command exited with code %d", co)
			})
		defer func() {
			onReadCmd.Close()
			c.log(logger.Info, "runOnRead command stopped")
		}()
	}

	// disable read deadline
	c.conn.SetReadDeadline(time.Time{})

	var videoStartPTS time.Duration
	var videoDTSEst *h264.DTSEstimator
	videoFirstIDRFound := false

	for {
		data, ok := c.ringBuffer.Pull()
		if !ok {
			return fmt.Errorf("terminated")
		}
		pair := data.(rtmpConnTrackIDDataPair)

		if videoTrack != nil && pair.trackID == videoTrackID {
			if pair.data.nalus == nil {
				continue
			}

			var filteredNALUs [][]byte
			idrPresent := false

			for _, nalu := range pair.data.nalus {
				typ := h264.NALUType(nalu[0] & 0x1F)

				switch typ {
				case h264.NALUTypeSPS, h264.NALUTypePPS:
					// added automatically before every IDR
					continue

				case h264.NALUTypeAccessUnitDelimiter:
					// not needed
					continue

				case h264.NALUTypeIDR:
					idrPresent = true
					// add SPS and PPS before every IDR
					// TODO: send H264DecoderConfig instead of NALUs?
					filteredNALUs = append(filteredNALUs, videoTrack.SPS(), videoTrack.PPS())
				}

				filteredNALUs = append(filteredNALUs, nalu)
			}

			if filteredNALUs == nil {
				continue
			}

			// wait until we receive an IDR
			if !videoFirstIDRFound {
				if !idrPresent {
					continue
				}

				videoFirstIDRFound = true
				videoStartPTS = pair.data.pts
				videoDTSEst = h264.NewDTSEstimator()
			}

			data, err := h264.EncodeAVCC(filteredNALUs)
			if err != nil {
				return err
			}

			pts := pair.data.pts - videoStartPTS
			dts := videoDTSEst.Feed(pts)

			c.conn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
			err = c.conn.WritePacket(av.Packet{
				Type:  av.H264,
				Data:  data,
				Time:  dts,
				CTime: pts - dts,
			})
			if err != nil {
				return err
			}
		} else if audioTrack != nil && pair.trackID == audioTrackID {
			aus, pts, err := aacDecoder.Decode(pair.data.rtp)
			if err != nil {
				if err != rtpaac.ErrMorePacketsNeeded {
					c.log(logger.Warn, "unable to decode audio track: %v", err)
				}
				continue
			}

			if videoTrack != nil && !videoFirstIDRFound {
				continue
			}

			pts -= videoStartPTS
			if pts < 0 {
				continue
			}

			for _, au := range aus {
				c.conn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
				err := c.conn.WritePacket(av.Packet{
					Type: av.AAC,
					Data: au,
					Time: pts,
				})
				if err != nil {
					return err
				}

				pts += 1000 * time.Second / time.Duration(audioTrack.ClockRate())
			}
		}
	}
}

func (c *rtmpConn) runPublish(ctx context.Context) error {
	c.conn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
	videoTrack, audioTrack, err := c.conn.ReadTracks()
	if err != nil {
		return err
	}

	var tracks gortsplib.Tracks
	videoTrackID := -1
	audioTrackID := -1

	var h264Encoder *rtph264.Encoder
	if videoTrack != nil {
		h264Encoder = rtph264.NewEncoder(96, nil, nil, nil)
		videoTrackID = len(tracks)
		tracks = append(tracks, videoTrack)
	}

	var aacEncoder *rtpaac.Encoder
	if audioTrack != nil {
		aacEncoder = rtpaac.NewEncoder(96, audioTrack.ClockRate(), nil, nil, nil)
		audioTrackID = len(tracks)
		tracks = append(tracks, audioTrack)
	}

	pathName, query, rawQuery := pathNameAndQuery(c.conn.URL())

	res := c.pathManager.onPublisherAnnounce(pathPublisherAnnounceReq{
		author:   c,
		pathName: pathName,
		authenticate: func(
			pathIPs []interface{},
			pathUser conf.Credential,
			pathPass conf.Credential) error {
			return c.authenticate(pathName, pathIPs, pathUser, pathPass, "publish", query, rawQuery)
		},
	})

	if res.err != nil {
		if terr, ok := res.err.(pathErrAuthCritical); ok {
			// wait some seconds to stop brute force attacks
			<-time.After(rtmpConnPauseAfterAuthError)
			return errors.New(terr.message)
		}
		return res.err
	}

	c.path = res.path

	defer func() {
		c.path.onPublisherRemove(pathPublisherRemoveReq{author: c})
	}()

	c.stateMutex.Lock()
	c.state = rtmpConnStatePublish
	c.stateMutex.Unlock()

	// disable write deadline
	c.conn.SetWriteDeadline(time.Time{})

	rres := c.path.onPublisherRecord(pathPublisherRecordReq{
		author: c,
		tracks: tracks,
	})
	if rres.err != nil {
		return rres.err
	}

	rtcpSenders := rtcpsenderset.New(tracks, rres.stream.onPacketRTCP)
	defer rtcpSenders.Close()

	for {
		c.conn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
		pkt, err := c.conn.ReadPacket()
		if err != nil {
			return err
		}

		switch pkt.Type {
		case av.H264DecoderConfig:
			codec, err := nh264.FromDecoderConfig(pkt.Data)
			if err != nil {
				return err
			}

			pts := pkt.Time + pkt.CTime
			nalus := [][]byte{
				codec.SPS[0],
				codec.PPS[0],
			}

			pkts, err := h264Encoder.Encode(nalus, pts)
			if err != nil {
				return fmt.Errorf("error while encoding H264: %v", err)
			}

			lastPkt := len(pkts) - 1
			for i, pkt := range pkts {
				rtcpSenders.OnPacketRTP(videoTrackID, pkt)

				if i != lastPkt {
					rres.stream.onPacketRTP(videoTrackID, &data{
						rtp: pkt,
					})
				} else {
					rres.stream.onPacketRTP(videoTrackID, &data{
						rtp:   pkt,
						nalus: nalus,
						pts:   pts,
					})
				}
			}

		case av.H264:
			if videoTrack == nil {
				return fmt.Errorf("received an H264 packet, but track is not set up")
			}

			nalus, err := h264.DecodeAVCC(pkt.Data)
			if err != nil {
				return err
			}

			var outNALUs [][]byte

			for _, nalu := range nalus {
				typ := h264.NALUType(nalu[0] & 0x1F)
				if typ == h264.NALUTypeAccessUnitDelimiter {
					// not needed
					continue
				}

				outNALUs = append(outNALUs, nalu)
			}

			if len(outNALUs) == 0 {
				continue
			}

			pts := pkt.Time + pkt.CTime

			pkts, err := h264Encoder.Encode(outNALUs, pts)
			if err != nil {
				return fmt.Errorf("error while encoding H264: %v", err)
			}

			lastPkt := len(pkts) - 1
			for i, pkt := range pkts {
				rtcpSenders.OnPacketRTP(videoTrackID, pkt)

				if i != lastPkt {
					rres.stream.onPacketRTP(videoTrackID, &data{
						rtp: pkt,
					})
				} else {
					rres.stream.onPacketRTP(videoTrackID, &data{
						rtp:   pkt,
						nalus: outNALUs,
						pts:   pts,
					})
				}
			}

		case av.AAC:
			if audioTrack == nil {
				return fmt.Errorf("received an AAC packet, but track is not set up")
			}

			pkts, err := aacEncoder.Encode([][]byte{pkt.Data}, pkt.Time+pkt.CTime)
			if err != nil {
				return fmt.Errorf("error while encoding AAC: %v", err)
			}

			for _, pkt := range pkts {
				rtcpSenders.OnPacketRTP(audioTrackID, pkt)
				rres.stream.onPacketRTP(audioTrackID, &data{rtp: pkt})
			}
		}
	}
}

func (c *rtmpConn) authenticate(
	pathName string,
	pathIPs []interface{},
	pathUser conf.Credential,
	pathPass conf.Credential,
	action string,
	query url.Values,
	rawQuery string,
) error {
	if c.externalAuthenticationURL != "" {
		err := externalAuth(
			c.externalAuthenticationURL,
			c.ip().String(),
			query.Get("user"),
			query.Get("pass"),
			pathName,
			action,
			rawQuery)
		if err != nil {
			return pathErrAuthCritical{
				message: fmt.Sprintf("external authentication failed: %s", err),
			}
		}
	}

	if pathIPs != nil {
		ip := c.ip()
		if !ipEqualOrInRange(ip, pathIPs) {
			return pathErrAuthCritical{
				message: fmt.Sprintf("IP '%s' not allowed", ip),
			}
		}
	}

	if pathUser != "" {
		if query.Get("user") != string(pathUser) ||
			query.Get("pass") != string(pathPass) {
			return pathErrAuthCritical{
				message: "invalid credentials",
			}
		}
	}

	return nil
}

// onReaderAccepted implements reader.
func (c *rtmpConn) onReaderAccepted() {
	c.log(logger.Info, "is reading from path '%s'", c.path.Name())
}

// onReaderPacketRTP implements reader.
func (c *rtmpConn) onReaderPacketRTP(trackID int, data *data) {
	c.ringBuffer.Push(rtmpConnTrackIDDataPair{trackID, data})
}

// onReaderPacketRTCP implements reader.
func (c *rtmpConn) onReaderPacketRTCP(trackID int, pkt rtcp.Packet) {
}

// onReaderAPIDescribe implements reader.
func (c *rtmpConn) onReaderAPIDescribe() interface{} {
	return struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{"rtmpConn", c.id}
}

// onSourceAPIDescribe implements source.
func (c *rtmpConn) onSourceAPIDescribe() interface{} {
	return struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{"rtmpConn", c.id}
}

// onPublisherAccepted implements publisher.
func (c *rtmpConn) onPublisherAccepted(tracksLen int) {
	c.log(logger.Info, "is publishing to path '%s', %d %s",
		c.path.Name(),
		tracksLen,
		func() string {
			if tracksLen == 1 {
				return "track"
			}
			return "tracks"
		}())
}
