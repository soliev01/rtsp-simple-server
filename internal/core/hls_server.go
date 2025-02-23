package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	gopath "path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/aler9/mediamtx/internal/conf"
	"github.com/aler9/mediamtx/internal/logger"
)

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

type hlsServerAPIMuxersListItem struct {
	Created     time.Time `json:"created"`
	LastRequest time.Time `json:"lastRequest"`
	BytesSent   uint64    `json:"bytesSent"`
}

type hlsServerAPIMuxersListData struct {
	Items map[string]hlsServerAPIMuxersListItem `json:"items"`
}

type hlsServerAPIMuxersListRes struct {
	data   *hlsServerAPIMuxersListData
	muxers map[string]*hlsMuxer
	err    error
}

type hlsServerAPIMuxersListReq struct {
	res chan hlsServerAPIMuxersListRes
}

type hlsServerAPIMuxersListSubReq struct {
	data *hlsServerAPIMuxersListData
	res  chan struct{}
}

type hlsServerParent interface {
	Log(logger.Level, string, ...interface{})
}

type hlsServer struct {
	externalAuthenticationURL string
	alwaysRemux               bool
	variant                   conf.HLSVariant
	segmentCount              int
	segmentDuration           conf.StringDuration
	partDuration              conf.StringDuration
	segmentMaxSize            conf.StringSize
	allowOrigin               string
	directory                 string
	readBufferCount           int
	pathManager               *pathManager
	metrics                   *metrics
	parent                    hlsServerParent

	ctx        context.Context
	ctxCancel  func()
	wg         sync.WaitGroup
	ln         net.Listener
	httpServer *http.Server
	muxers     map[string]*hlsMuxer

	// in
	chPathSourceReady    chan *path
	chPathSourceNotReady chan *path
	request              chan *hlsMuxerRequest
	chMuxerClose         chan *hlsMuxer
	chAPIMuxerList       chan hlsServerAPIMuxersListReq
}

func newHLSServer(
	parentCtx context.Context,
	address string,
	encryption bool,
	serverKey string,
	serverCert string,
	externalAuthenticationURL string,
	alwaysRemux bool,
	variant conf.HLSVariant,
	segmentCount int,
	segmentDuration conf.StringDuration,
	partDuration conf.StringDuration,
	segmentMaxSize conf.StringSize,
	allowOrigin string,
	trustedProxies conf.IPsOrCIDRs,
	directory string,
	readTimeout conf.StringDuration,
	readBufferCount int,
	pathManager *pathManager,
	metrics *metrics,
	parent hlsServerParent,
) (*hlsServer, error) {
	ln, err := net.Listen(restrictNetwork("tcp", address))
	if err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	if encryption {
		crt, err := tls.LoadX509KeyPair(serverCert, serverKey)
		if err != nil {
			ln.Close()
			return nil, err
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{crt},
		}
	}

	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &hlsServer{
		externalAuthenticationURL: externalAuthenticationURL,
		alwaysRemux:               alwaysRemux,
		variant:                   variant,
		segmentCount:              segmentCount,
		segmentDuration:           segmentDuration,
		partDuration:              partDuration,
		segmentMaxSize:            segmentMaxSize,
		allowOrigin:               allowOrigin,
		directory:                 directory,
		readBufferCount:           readBufferCount,
		pathManager:               pathManager,
		parent:                    parent,
		metrics:                   metrics,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
		ln:                        ln,
		muxers:                    make(map[string]*hlsMuxer),
		chPathSourceReady:         make(chan *path),
		chPathSourceNotReady:      make(chan *path),
		request:                   make(chan *hlsMuxerRequest),
		chMuxerClose:              make(chan *hlsMuxer),
		chAPIMuxerList:            make(chan hlsServerAPIMuxersListReq),
	}

	router := gin.New()
	httpSetTrustedProxies(router, trustedProxies)

	router.NoRoute(httpLoggerMiddleware(s), httpServerHeaderMiddleware, s.onRequest)

	s.httpServer = &http.Server{
		Handler:           router,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: time.Duration(readTimeout),
		ErrorLog:          log.New(&nilWriter{}, "", 0),
	}

	s.log(logger.Info, "listener opened on "+address)

	s.pathManager.hlsServerSet(s)

	if s.metrics != nil {
		s.metrics.hlsServerSet(s)
	}

	s.wg.Add(1)
	go s.run()

	return s, nil
}

// Log is the main logging function.
func (s *hlsServer) log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[HLS] "+format, append([]interface{}{}, args...)...)
}

func (s *hlsServer) close() {
	s.log(logger.Info, "listener is closing")
	s.ctxCancel()
	s.wg.Wait()
}

func (s *hlsServer) run() {
	defer s.wg.Done()

	if s.httpServer.TLSConfig != nil {
		go s.httpServer.ServeTLS(s.ln, "", "")
	} else {
		go s.httpServer.Serve(s.ln)
	}

outer:
	for {
		select {
		case pa := <-s.chPathSourceReady:
			if s.alwaysRemux {
				s.createMuxer(pa.name, "")
			}

		case pa := <-s.chPathSourceNotReady:
			if s.alwaysRemux {
				c, ok := s.muxers[pa.name]
				if ok {
					c.close()
					delete(s.muxers, pa.name)
				}
			}

		case req := <-s.request:
			r, ok := s.muxers[req.path]
			switch {
			case ok:
				r.processRequest(req)

			case s.alwaysRemux:
				req.res <- nil

			default:
				r := s.createMuxer(req.path, req.clientIP)
				r.processRequest(req)
			}

		case c := <-s.chMuxerClose:
			if c2, ok := s.muxers[c.PathName()]; !ok || c2 != c {
				continue
			}
			delete(s.muxers, c.PathName())

		case req := <-s.chAPIMuxerList:
			muxers := make(map[string]*hlsMuxer)

			for name, m := range s.muxers {
				muxers[name] = m
			}

			req.res <- hlsServerAPIMuxersListRes{
				muxers: muxers,
			}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	s.httpServer.Shutdown(context.Background())
	s.ln.Close() // in case Shutdown() is called before Serve()

	s.pathManager.hlsServerSet(nil)

	if s.metrics != nil {
		s.metrics.hlsServerSet(nil)
	}
}

func (s *hlsServer) onRequest(ctx *gin.Context) {
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", s.allowOrigin)
	ctx.Writer.Header().Set("Access-Control-Allow-Credentials", "true")

	switch ctx.Request.Method {
	case http.MethodGet:

	case http.MethodOptions:
		ctx.Writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		ctx.Writer.Header().Set("Access-Control-Allow-Headers", ctx.Request.Header.Get("Access-Control-Request-Headers"))
		ctx.Writer.WriteHeader(http.StatusOK)
		return

	default:
		return
	}

	// remove leading prefix
	pa := ctx.Request.URL.Path[1:]

	switch pa {
	case "", "favicon.ico":
		return
	}

	dir, fname := func() (string, string) {
		if strings.HasSuffix(pa, ".m3u8") ||
			strings.HasSuffix(pa, ".ts") ||
			strings.HasSuffix(pa, ".mp4") ||
			strings.HasSuffix(pa, ".mp") {
			return gopath.Dir(pa), gopath.Base(pa)
		}
		return pa, ""
	}()

	if fname == "" && !strings.HasSuffix(dir, "/") {
		ctx.Writer.Header().Set("Location", "/"+dir+"/")
		ctx.Writer.WriteHeader(http.StatusMovedPermanently)
		return
	}

	if strings.HasSuffix(fname, ".mp") {
		fname += "4"
	}

	dir = strings.TrimSuffix(dir, "/")

	hreq := &hlsMuxerRequest{
		path:     dir,
		file:     fname,
		clientIP: ctx.ClientIP(),
		res:      make(chan *hlsMuxer),
	}

	select {
	case s.request <- hreq:
		muxer := <-hreq.res
		if muxer != nil {
			ctx.Request.URL.Path = fname
			muxer.handleRequest(ctx)
		}

	case <-s.ctx.Done():
	}
}

func (s *hlsServer) createMuxer(pathName string, remoteAddr string) *hlsMuxer {
	r := newHLSMuxer(
		s.ctx,
		remoteAddr,
		s.externalAuthenticationURL,
		s.alwaysRemux,
		s.variant,
		s.segmentCount,
		s.segmentDuration,
		s.partDuration,
		s.segmentMaxSize,
		s.directory,
		s.readBufferCount,
		&s.wg,
		pathName,
		s.pathManager,
		s)
	s.muxers[pathName] = r
	return r
}

// muxerClose is called by hlsMuxer.
func (s *hlsServer) muxerClose(c *hlsMuxer) {
	select {
	case s.chMuxerClose <- c:
	case <-s.ctx.Done():
	}
}

// pathSourceReady is called by pathManager.
func (s *hlsServer) pathSourceReady(pa *path) {
	select {
	case s.chPathSourceReady <- pa:
	case <-s.ctx.Done():
	}
}

// pathSourceNotReady is called by pathManager.
func (s *hlsServer) pathSourceNotReady(pa *path) {
	select {
	case s.chPathSourceNotReady <- pa:
	case <-s.ctx.Done():
	}
}

// apiMuxersList is called by api.
func (s *hlsServer) apiMuxersList() hlsServerAPIMuxersListRes {
	req := hlsServerAPIMuxersListReq{
		res: make(chan hlsServerAPIMuxersListRes),
	}

	select {
	case s.chAPIMuxerList <- req:
		res := <-req.res

		res.data = &hlsServerAPIMuxersListData{
			Items: make(map[string]hlsServerAPIMuxersListItem),
		}

		for _, pa := range res.muxers {
			pa.apiMuxersList(hlsServerAPIMuxersListSubReq{data: res.data})
		}

		return res

	case <-s.ctx.Done():
		return hlsServerAPIMuxersListRes{err: fmt.Errorf("terminated")}
	}
}
