package video

import (
	"errors"
	"fmt"
	"nvr/pkg/log"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/gortsplib/pkg/base"
	"sync"
)

type rtspSessionPathManager interface {
	publisherAdd(name string, session *rtspSession) (*path, error)
	readerAdd(name string, session *rtspSession) (*path, *stream, error)
}

type rtspSession struct {
	id          string
	ss          *gortsplib.ServerSession
	author      *gortsplib.ServerConn
	pathManager rtspSessionPathManager
	logger      *log.Logger

	path            *path
	state           gortsplib.ServerSessionState
	stateMutex      sync.Mutex
	announcedTracks gortsplib.Tracks // publish
	stream          *stream          // publish
}

func newRTSPSession(
	id string,
	ss *gortsplib.ServerSession,
	sc *gortsplib.ServerConn,
	pathManager rtspSessionPathManager,
	logger *log.Logger,
) *rtspSession {
	s := &rtspSession{
		id:          id,
		ss:          ss,
		author:      sc,
		pathManager: pathManager,
		logger:      logger,
		path:        &path{conf: &PathConf{}},
	}

	return s
}

// close closes a Session.
func (s *rtspSession) close() {
	s.ss.Close()
}

// ID returns the public ID of the session.
func (s *rtspSession) ID() string {
	return s.id
}

func (s *rtspSession) logf(level log.Level, conf PathConf, format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	sendLogf(s.logger, conf, level, "RTSP:", "S:%s %s", s.id, msg)
}

// close is called by rtspServer.
func (s *rtspSession) onClose(conf PathConf, err error) {
	switch s.ss.State() {
	case gortsplib.ServerSessionStatePrePlay, gortsplib.ServerSessionStatePlay:
		s.path.readerRemove(s)
		s.path = nil

	case gortsplib.ServerSessionStatePreRecord, gortsplib.ServerSessionStateRecord:
		s.path.close()
		s.path = nil
	}

	s.logf(log.LevelDebug, conf, "destroyed (%v)", err)
}

// Errors .
var (
	ErrTrackInvalidH264 = errors.New("h264 SPS or PPS not provided into the SDP")
	ErrTrackInvalidAAC  = errors.New("aac track is not valid")
	ErrTrackInvalidOpus = errors.New("opus track is not valid")
)

// onAnnounce is called by rtspServer.
func (s *rtspSession) onAnnounce(
	pathName string,
	tracks gortsplib.Tracks,
) (*base.Response, error) {
	path, err := s.pathManager.publisherAdd(pathName, s)
	if err != nil {
		return &base.Response{StatusCode: base.StatusBadRequest}, err
	}

	s.path = path
	s.announcedTracks = tracks

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStatePreRecord
	s.stateMutex.Unlock()

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// ErrTrackNotExist Track does not exist.
var ErrTrackNotExist = errors.New("track does not exist")

// onSetup is called by rtspServer.
func (s *rtspSession) onSetup(
	pathName string,
	trackID int,
) (*base.Response, *gortsplib.ServerStream, error) {
	state := s.ss.State()

	// record
	if state != gortsplib.ServerSessionStateInitial &&
		state != gortsplib.ServerSessionStatePrePlay {
		return &base.Response{StatusCode: base.StatusOK}, nil, nil
	}

	// play
	path, stream, err := s.pathManager.readerAdd(pathName, s)
	if err != nil {
		if errors.Is(err, ErrPathNoOnePublishing) {
			return &base.Response{StatusCode: base.StatusNotFound}, nil, err
		}
		return &base.Response{StatusCode: base.StatusBadRequest}, nil, err
	}

	s.path = path

	if trackID >= len(stream.tracks()) {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, nil, fmt.Errorf("%w (%d)", ErrTrackNotExist, trackID)
	}

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStatePrePlay
	s.stateMutex.Unlock()

	return &base.Response{StatusCode: base.StatusOK}, stream.rtspStream, nil
}

// onPlay is called by rtspServer.
func (s *rtspSession) onPlay() (*base.Response, error) {
	h := make(base.Header)

	if s.ss.State() == gortsplib.ServerSessionStatePrePlay {
		s.path.readerStart(s)

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePlay
		s.stateMutex.Unlock()
	}

	return &base.Response{
		StatusCode: base.StatusOK,
		Header:     h,
	}, nil
}

// onRecord is called by rtspServer.
func (s *rtspSession) onRecord() (*base.Response, error) {
	stream, err := s.path.publisherStart(s.announcedTracks)
	if err != nil {
		return &base.Response{StatusCode: base.StatusBadRequest}, err
	}

	s.stream = stream

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStateRecord
	s.stateMutex.Unlock()

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// onPacketRTP is called by rtspServer.
func (s *rtspSession) onPacketRTP(ctx *gortsplib.PacketRTPCtx) {
	if ctx.H264NALUs != nil {
		s.stream.writeData(&data{
			trackID:      ctx.TrackID,
			rtpPacket:    ctx.Packet,
			ptsEqualsDTS: ctx.PTSEqualsDTS,
			h264NALUs:    ctx.H264NALUs,
			pts:          ctx.H264PTS,
		})
	} else {
		s.stream.writeData(&data{
			trackID:      ctx.TrackID,
			rtpPacket:    ctx.Packet,
			ptsEqualsDTS: ctx.PTSEqualsDTS,
		})
	}
}
