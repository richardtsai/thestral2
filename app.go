package main

import (
	"context"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/richardtsai/thestral2/db"
	. "github.com/richardtsai/thestral2/lib"
	"go.uber.org/zap"
)

const (
	defaultConnectTimeout = time.Minute * 1
	relayBufferSize       = 32 * 1024
)

// Thestral is the main thestral app.
type Thestral struct {
	log            *zap.SugaredLogger
	downstreams    map[string]ProxyServer
	upstreams      map[string]ProxyClient
	upstreamNames  []string
	ruleMatcher    *RuleMatcher
	connectTimeout time.Duration
	monitor        AppMonitor
}

// NewThestralApp creates a Thestral app object from the given configuration.
func NewThestralApp(config Config) (app *Thestral, err error) {
	if len(config.Downstreams) == 0 {
		err = errors.New("no downstream server defined")
	}
	if err == nil && len(config.Upstreams) == 0 {
		err = errors.New("no upstream server defined")
	}

	app = &Thestral{
		downstreams: make(map[string]ProxyServer),
		upstreams:   make(map[string]ProxyClient),
	}

	// create logger
	if err == nil {
		app.log, err = CreateLogger(config.Logging)
		if err != nil {
			err = errors.WithMessage(err, "failed to create logger")
		}
	}

	// init db
	if err == nil && config.DB != nil {
		err = db.InitDB(*config.DB)
	}

	// create downstream servers
	if err == nil {
		dsLogger := app.log.Named("downstreams")
		for k, v := range config.Downstreams {
			app.downstreams[k], err = CreateProxyServer(dsLogger.Named(k), v)
			if err != nil {
				err = errors.WithMessage(
					err, "failed to create downstream server: "+k)
				break
			}
		}
	}

	// create upstream clients
	if err == nil {
		for k, v := range config.Upstreams {
			app.upstreams[k], err = CreateProxyClient(v)
			if err != nil {
				err = errors.WithMessage(
					err, "failed to create upstream client: "+k)
				break
			}
			app.upstreamNames = append(app.upstreamNames, k)
		}
	}

	// create rule matcher
	if err == nil {
		app.ruleMatcher, err = NewRuleMatcher(config.Rules)
		if err != nil {
			err = errors.WithMessage(err, "failed to create rule matcher")
		}
	}
	if err == nil {
		for _, ruleUpstream := range app.ruleMatcher.AllUpstreams {
			if _, ok := app.upstreams[ruleUpstream]; !ok {
				err = errors.Errorf(
					"undefined upstream '%s' used in the rule set",
					ruleUpstream)
			}
		}
	}

	// parse other settings
	if err == nil {
		if config.Misc.ConnectTimeout != "" {
			app.connectTimeout, err = time.ParseDuration(
				config.Misc.ConnectTimeout)
			if err != nil {
				err = errors.WithStack(err)
			}
			if err == nil && app.connectTimeout <= 0 {
				err = errors.New("'connect_timeout' should be greater than 0")
			}
		} else {
			app.connectTimeout = defaultConnectTimeout
		}
	}
	if err == nil && config.Misc.EnableMonitor {
		app.monitor.Start(config.Misc.MonitorPath)
	}

	return
}

// Run starts the thestral app and blocks until the context is canceled.
func (t *Thestral) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for dsName, server := range t.downstreams {
		reqCh, err := server.Start()
		if err != nil {
			t.log.Errorw(
				"failed to start downstream server: "+dsName, "error", err)
			return err
		}

		wg.Add(1)
		go func(reqCh <-chan ProxyRequest, dsName string, server ProxyServer) {
			log := t.log.Named("downstreams").Named(dsName)
			log.Infof("downstream server started: %s", dsName)

			t.processRequests(ctx, dsName, reqCh) // blocks

			server.Stop()
			log.Infof("downstream server stopped: %s", dsName)
			wg.Done()
		}(reqCh, dsName, server)
	}

	t.log.Info("thestral app started")
	wg.Wait()
	return nil
}

func (t *Thestral) processRequests(
	ctx context.Context, dsName string, reqCh <-chan ProxyRequest) {
	for {
		select {
		case req := <-reqCh:
			peerIDs, err := req.GetPeerIdentifiers()
			if err != nil {
				req.Logger().Warnw(
					"failed to get peer identifiers", "error", err)
			}
			req.Logger().Infow("request accepted",
				"downstream", dsName,
				"clientAddr", req.PeerAddr(),
				"target", req.TargetAddr(),
				"userIDs", peerIDs)
			go t.processOneRequest(ctx, req, dsName)
		case <-ctx.Done():
			return
		}
	}
}

func (t *Thestral) processOneRequest(
	ctx context.Context, req ProxyRequest, dsName string) {
	// match against rule set
	ruleName := ""
	var upstreams []string
	switch addr := req.TargetAddr().(type) {
	case *TCP4Addr:
		ruleName, upstreams = t.ruleMatcher.MatchIP(addr.IP)
	case *TCP6Addr:
		ruleName, upstreams = t.ruleMatcher.MatchIP(addr.IP)
	case *DomainNameAddr:
		ruleName, upstreams = t.ruleMatcher.MatchDomain(addr.DomainName)
	default:
		req.Logger().Errorw("unknown target address", "addr", addr)
		req.Fail(&ProxyError{Error: nil, ErrType: ProxyAddrUnsupported})
		return
	}

	// select an upstream
	if ruleName == "" { // unmatch and no default rule, allow all
		upstreams = t.upstreamNames
	} else if len(upstreams) == 0 { // no upstream, reject
		req.Logger().Errorw(
			"request rejected by rule",
			"rule", ruleName, "addr", req.TargetAddr())
		req.Fail(&ProxyError{Error: nil, ErrType: ProxyNotAllowed})
		return
	}
	//TODO: the selection is not actually uniform, fix it
	selected := upstreams[rand.Intn(len(upstreams))]
	req.Logger().Debugw(
		"upstream selected",
		"rule", ruleName, "upstream", selected, "addr", req.TargetAddr())
	upstream := t.upstreams[selected]

	// make request
	reqCtx, cancelFunc := context.WithTimeout(ctx, t.connectTimeout)
	defer cancelFunc()
	startTime := time.Now()
	upConn, boundAddr, pErr := upstream.Request(reqCtx, req.TargetAddr())
	if pErr != nil {
		req.Logger().Errorw(
			"connection failed", "addr", req.TargetAddr(),
			"error", pErr.Error, "errType", pErr.ErrType, "upstream", selected)
		req.Fail(pErr)
		t.monitor.AddError(selected)
		return
	}
	connLatency := time.Since(startTime)

	var peerIDs []*PeerIdentifier
	if wpi, ok := upConn.(WithPeerIdentifiers); ok {
		peerIDs, _ = wpi.GetPeerIdentifiers()
	}
	req.Logger().Infow(
		"connection established",
		"addr", req.TargetAddr(), "boundAddr", boundAddr, "upstream", selected,
		"serverIDs", peerIDs)
	downRWC := req.Success(boundAddr)
	relayCtx, cancelFunc := context.WithCancel(ctx)
	tunnelMonitor := t.monitor.OpenTunnelMonitor(
		req, ruleName, dsName, selected, peerIDs, boundAddr.String(),
		connLatency, cancelFunc)
	t.doRelay(relayCtx, cancelFunc, tunnelMonitor, req, downRWC, upConn) // block
}

func (t *Thestral) doRelay(
	relayCtx context.Context, cancelFunc context.CancelFunc,
	tunnelMonitor *TunnelMonitor, req ProxyRequest,
	downRWC io.ReadWriteCloser, upRWC io.ReadWriteCloser) {
	defer tunnelMonitor.Close()
	relay := func(dst, src io.ReadWriteCloser, srcName string,
		reportBytesTransfered func(uint32)) {
		defer cancelFunc()
		var n int64
		var err error
		n, err = t.relayHalf(dst, src, reportBytesTransfered)
		if err == nil { // src closed
			req.Logger().Infow(
				"connection closed", "src", srcName, "bytesTransferred", n)
		} else if relayCtx.Err() == context.Canceled { // other direction closed
			req.Logger().Infow(
				"relay ended", "src", srcName, "bytesTransferred", n)
		} else { // error
			req.Logger().Warnw(
				"error occurred",
				"error", err, "src", srcName, "bytesTransferred", n)
		}
	}

	go relay(upRWC, downRWC, "downstream", tunnelMonitor.IncBytesUploaded)
	go relay(downRWC, upRWC, "upstream", tunnelMonitor.IncBytesDownloaded)

	<-relayCtx.Done() // block until done/canceled
	if err := upRWC.Close(); err != nil {
		req.Logger().Warnw(
			"error occurred when closing upstream", "error", err)
	}
	if err := downRWC.Close(); err != nil {
		req.Logger().Warnw(
			"error occurred when closing downstream", "error", err)
	}
}

func (t *Thestral) relayHalf(
	dst io.Writer, src io.Reader,
	reportBytesTransfered func(uint32)) (n int64, err error) {
	buf := GlobalBufPool.Get(relayBufferSize)
	defer GlobalBufPool.Free(buf)
	for {
		var nr, nw int
		if nr, err = src.Read(buf); err == nil { // data read from src
			nw, err = dst.Write(buf[:nr])
			n += int64(nw)
			reportBytesTransfered(uint32(nw))
			if err != nil { // write failed
				break
			}
			if nw < nr {
				err = io.ErrShortWrite
				break
			}
		} else { // EOF or error occurred
			if err == io.EOF { // ended
				err = nil
			}
			break
		}
	}

	err = errors.WithStack(err)
	return
}
