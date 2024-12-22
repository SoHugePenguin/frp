// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/SoHugePenguin/frp/server/models"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/SoHugePenguin/golib/crypto"
	"github.com/SoHugePenguin/golib/net/mux"
	fmux "github.com/hashicorp/yamux"
	"github.com/quic-go/quic-go"
	"github.com/samber/lo"

	"github.com/SoHugePenguin/frp/pkg/auth"
	v1 "github.com/SoHugePenguin/frp/pkg/config/v1"
	modelmetrics "github.com/SoHugePenguin/frp/pkg/metrics"
	"github.com/SoHugePenguin/frp/pkg/msg"
	ctl_ "github.com/SoHugePenguin/frp/pkg/msg"
	"github.com/SoHugePenguin/frp/pkg/nathole"
	plugin "github.com/SoHugePenguin/frp/pkg/plugin/server"
	"github.com/SoHugePenguin/frp/pkg/ssh"
	"github.com/SoHugePenguin/frp/pkg/transport"
	httppkg "github.com/SoHugePenguin/frp/pkg/util/http"
	"github.com/SoHugePenguin/frp/pkg/util/log"
	netpkg "github.com/SoHugePenguin/frp/pkg/util/net"
	"github.com/SoHugePenguin/frp/pkg/util/tcpmux"
	"github.com/SoHugePenguin/frp/pkg/util/util"
	"github.com/SoHugePenguin/frp/pkg/util/version"
	"github.com/SoHugePenguin/frp/pkg/util/vhost"
	"github.com/SoHugePenguin/frp/pkg/util/xlog"
	"github.com/SoHugePenguin/frp/server/controller"
	"github.com/SoHugePenguin/frp/server/group"
	"github.com/SoHugePenguin/frp/server/metrics"
	"github.com/SoHugePenguin/frp/server/ports"
	"github.com/SoHugePenguin/frp/server/proxy"
	"github.com/SoHugePenguin/frp/server/visitor"
)

const (
	connReadTimeout       time.Duration = 10 * time.Second
	vhostReadWriteTimeout time.Duration = 30 * time.Second
)

func init() {
	crypto.DefaultSalt = "frp"
	// Disable quic-go's receive buffer warning.
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	// Disable quic-go's ECN support by default. It may cause issues on certain operating systems.
	if os.Getenv("QUIC_GO_DISABLE_ECN") == "" {
		_ = os.Setenv("QUIC_GO_DISABLE_ECN", "true")
	}
}

// Service Server service
type Service struct {
	// Dispatch connections to different handlers listen on same port
	muxer *mux.Mux

	// Accept connections from client
	listener net.Listener

	// penguin add
	realTimeListener net.Listener

	// Accept connections using kcp
	kcpListener net.Listener

	// Accept connections using quic
	quicListener *quic.Listener

	// Accept connections using websocket
	websocketListener net.Listener

	// Accept frp tls connections
	tlsListener net.Listener

	// Accept pipe connections from ssh tunnel gateway
	sshTunnelListener *netpkg.InternalListener

	// Manage all controllers
	ctlManager *ControlManager

	// Manage all proxies
	pxyManager *proxy.Manager

	// Manage all plugins
	pluginManager *plugin.Manager

	// HTTP vhost router
	httpVhostRouter *vhost.Routers

	// All resource managers and controllers
	rc *controller.ResourceController

	// web server for dashboard UI and apis
	webServer *httppkg.Server

	sshTunnelGateway *ssh.Gateway

	// Verifies authentication based on selected method
	authVerifier auth.Verifier

	tlsConfig *tls.Config

	cfg *v1.ServerConfig

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel context.CancelFunc
}

func NewService(cfg *v1.ServerConfig) (*Service, error) {
	tlsConfig, err := transport.NewServerTLSConfig(
		cfg.Transport.TLS.CertFile,
		cfg.Transport.TLS.KeyFile,
		cfg.Transport.TLS.TrustedCaFile)
	if err != nil {
		return nil, err
	}

	var webServer *httppkg.Server
	if cfg.WebServer.Port > 0 {
		ws, err := httppkg.NewServer(cfg.WebServer)
		if err != nil {
			return nil, err
		}
		webServer = ws

		modelmetrics.EnableMem()
		if cfg.EnablePrometheus {
			modelmetrics.EnablePrometheus()
		}
	}

	svr := &Service{
		ctlManager:    NewControlManager(),
		pxyManager:    proxy.NewManager(),
		pluginManager: plugin.NewManager(),
		rc: &controller.ResourceController{
			VisitorManager: visitor.NewManager(),
			TCPPortManager: ports.NewManager("tcp", cfg.ProxyBindAddr, cfg.AllowPorts),
			UDPPortManager: ports.NewManager("udp", cfg.ProxyBindAddr, cfg.AllowPorts),
		},
		sshTunnelListener: netpkg.NewInternalListener(),
		httpVhostRouter:   vhost.NewRouters(),
		authVerifier:      auth.NewAuthVerifier(cfg.Auth),
		webServer:         webServer,
		tlsConfig:         tlsConfig,
		cfg:               cfg,
		ctx:               context.Background(),
	}
	if webServer != nil {
		webServer.RouteRegister(svr.registerRouteHandlers)
	}

	// Create tcpmux httpconnect multiplexer.
	if cfg.TCPMuxHTTPConnectPort > 0 {
		var l net.Listener
		address := net.JoinHostPort(cfg.ProxyBindAddr, strconv.Itoa(cfg.TCPMuxHTTPConnectPort))
		l, err = net.Listen("tcp", address)
		if err != nil {
			return nil, fmt.Errorf("create server listener error, %v", err)
		}

		svr.rc.TCPMuxHTTPConnectMuxer, err = tcpmux.NewHTTPConnectTCPMuxer(l, cfg.TCPMuxPassthrough, vhostReadWriteTimeout)
		if err != nil {
			return nil, fmt.Errorf("create vhost tcpMuxer error, %v", err)
		}
		log.Infof("tcpmux httpconnect multiplexer listen on %s, passthough: %v", address, cfg.TCPMuxPassthrough)
	}

	// Init all plugins
	for _, p := range cfg.HTTPPlugins {
		svr.pluginManager.Register(plugin.NewHTTPPluginOptions(p))
		log.Infof("plugin [%s] has been registered", p.Name)
	}
	svr.rc.PluginManager = svr.pluginManager

	// Init group controller
	svr.rc.TCPGroupCtl = group.NewTCPGroupCtl(svr.rc.TCPPortManager)

	// Init HTTP group controller
	svr.rc.HTTPGroupCtl = group.NewHTTPGroupController(svr.httpVhostRouter)

	// Init TCP mux group controller
	svr.rc.TCPMuxGroupCtl = group.NewTCPMuxGroupCtl(svr.rc.TCPMuxHTTPConnectMuxer)

	// Init 404 not found page
	vhost.NotFoundPagePath = cfg.Custom404Page

	var (
		httpMuxOn  bool
		httpsMuxOn bool
	)
	if cfg.BindAddr == cfg.ProxyBindAddr {
		if cfg.BindPort == cfg.VhostHTTPPort {
			httpMuxOn = true
		}
		if cfg.BindPort == cfg.VhostHTTPSPort {
			httpsMuxOn = true
		}
	}

	// Listen for accepting connections from client.
	address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.BindPort))
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("create server listener error, %v", err)
	}

	svr.muxer = mux.NewMux(ln)
	svr.muxer.SetKeepAlive(time.Duration(cfg.Transport.TCPKeepAlive) * time.Second)
	go func() {
		_ = svr.muxer.Serve()
	}()
	ln = svr.muxer.DefaultListener()

	svr.listener = ln
	log.Infof("frps tcp listen on %s", address)

	// Listen for accepting connections from client using kcp protocol.
	if cfg.KCPBindPort > 0 {
		address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.KCPBindPort))
		svr.kcpListener, err = netpkg.ListenKcp(address)
		if err != nil {
			return nil, fmt.Errorf("listen on kcp udp address %s error: %v", address, err)
		}
		log.Infof("frps kcp listen on udp %s", address)
	}

	if cfg.QUICBindPort > 0 {
		address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.QUICBindPort))
		quicTLSCfg := tlsConfig.Clone()
		quicTLSCfg.NextProtos = []string{"frp"}
		svr.quicListener, err = quic.ListenAddr(address, quicTLSCfg, &quic.Config{
			MaxIdleTimeout:     time.Duration(cfg.Transport.QUIC.MaxIdleTimeout) * time.Second,
			MaxIncomingStreams: int64(cfg.Transport.QUIC.MaxIncomingStreams),
			KeepAlivePeriod:    time.Duration(cfg.Transport.QUIC.KeepalivePeriod) * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("listen on quic udp address %s error: %v", address, err)
		}
		log.Infof("frps quic listen on %s", address)
	}

	if cfg.SSHTunnelGateway.BindPort > 0 {
		sshGateway, err := ssh.NewGateway(cfg.SSHTunnelGateway, cfg.ProxyBindAddr, svr.sshTunnelListener)
		if err != nil {
			return nil, fmt.Errorf("create ssh gateway error: %v", err)
		}
		svr.sshTunnelGateway = sshGateway
		log.Infof("frps sshTunnelGateway listen on port %d", cfg.SSHTunnelGateway.BindPort)
	}

	// Listen for accepting connections from client using websocket protocol.
	websocketPrefix := []byte("GET " + netpkg.FrpWebsocketPath)
	websocketLn := svr.muxer.Listen(0, uint32(len(websocketPrefix)), func(data []byte) bool {
		return bytes.Equal(data, websocketPrefix)
	})
	svr.websocketListener = netpkg.NewWebsocketListener(websocketLn)

	// Create http vhost muxer.
	if cfg.VhostHTTPPort > 0 {
		rp := vhost.NewHTTPReverseProxy(vhost.HTTPReverseProxyOptions{
			ResponseHeaderTimeoutS: cfg.VhostHTTPTimeout,
		}, svr.httpVhostRouter)
		svr.rc.HTTPReverseProxy = rp

		address := net.JoinHostPort(cfg.ProxyBindAddr, strconv.Itoa(cfg.VhostHTTPPort))
		server := &http.Server{
			Addr:              address,
			Handler:           rp,
			ReadHeaderTimeout: 60 * time.Second,
		}
		var l net.Listener
		if httpMuxOn {
			l = svr.muxer.ListenHTTP(1)
		} else {
			l, err = net.Listen("tcp", address)
			if err != nil {
				return nil, fmt.Errorf("create vhost http listener error, %v", err)
			}
		}
		go func() {
			_ = server.Serve(l)
		}()
		log.Infof("http service listen on %s", address)
	}

	// Create https vhost muxer.
	if cfg.VhostHTTPSPort > 0 {
		var l net.Listener
		if httpsMuxOn {
			l = svr.muxer.ListenHTTPS(1)
		} else {
			address := net.JoinHostPort(cfg.ProxyBindAddr, strconv.Itoa(cfg.VhostHTTPSPort))
			l, err = net.Listen("tcp", address)
			if err != nil {
				return nil, fmt.Errorf("create server listener error, %v", err)
			}
			log.Infof("https service listen on %s", address)
		}

		svr.rc.VhostHTTPSMuxer, err = vhost.NewHTTPSMuxer(l, vhostReadWriteTimeout)
		if err != nil {
			return nil, fmt.Errorf("create vhost httpsMuxer error, %v", err)
		}
	}

	// frp tls listener
	svr.tlsListener = svr.muxer.Listen(2, 1, func(data []byte) bool {
		// tls first byte can be 0x16 only when vhost https port is not same with bind port
		return int(data[0]) == netpkg.FRPTLSHeadByte || int(data[0]) == 0x16
	})

	// penguin realtime msg listener
	address = net.JoinHostPort("0.0.0.0", strconv.Itoa(svr.cfg.RealtimeMsgPort))
	rLn, rErr := net.Listen("tcp", address)
	if rErr != nil {
		return nil, fmt.Errorf("create server listener error, %v", rErr)
	}
	svr.muxer = mux.NewMux(rLn)
	svr.muxer.SetKeepAlive(time.Duration(cfg.Transport.TCPKeepAlive) * time.Second)
	go func() {
		_ = svr.muxer.Serve()
	}()
	svr.realTimeListener = svr.muxer.DefaultListener()
	log.Infof("frps penguin msg listen on %s", address)

	// Create nat hole controller.
	nc, err := nathole.NewController(time.Duration(cfg.NatHoleAnalysisDataReserveHours) * time.Hour)
	if err != nil {
		return nil, fmt.Errorf("create nat hole controller error, %v", err)
	}
	svr.rc.NatHoleController = nc
	return svr, nil
}

func (svr *Service) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	svr.ctx = ctx
	svr.cancel = cancel

	// run dashboard web server.
	if svr.webServer != nil {
		go func() {
			log.Infof("dashboard listen on %s", svr.webServer.Address())
			if err := svr.webServer.Run(); err != nil {
				log.Warnf("dashboard server exit with error: %v", err)
			}
		}()
	}

	go svr.HandleListener(svr.sshTunnelListener, true)

	if svr.kcpListener != nil {
		go svr.HandleListener(svr.kcpListener, false)
	}
	if svr.quicListener != nil {
		go svr.HandleQUICListener(svr.quicListener)
	}
	go svr.HandleListener(svr.websocketListener, false)
	go svr.HandleListener(svr.tlsListener, false)

	if svr.rc.NatHoleController != nil {
		go svr.rc.NatHoleController.CleanWorker(svr.ctx)
	}

	if svr.sshTunnelGateway != nil {
		go svr.sshTunnelGateway.Run()
	}

	// penguin init msg listener
	go svr.HandlePenguinRealtimeMsgListener(svr.realTimeListener)

	// 先登录获取run_id后登录msg server
	svr.HandleListener(svr.listener, false)

	<-svr.ctx.Done()
	// service context may not be canceled by svr.Close(), we should call it here to release resources
	if svr.listener != nil {
		_ = svr.Close()
	}
}

func (svr *Service) Close() error {
	if svr.kcpListener != nil {
		_ = svr.kcpListener.Close()
		svr.kcpListener = nil
	}
	if svr.quicListener != nil {
		_ = svr.quicListener.Close()
		svr.quicListener = nil
	}
	if svr.websocketListener != nil {
		_ = svr.websocketListener.Close()
		svr.websocketListener = nil
	}
	if svr.tlsListener != nil {
		_ = svr.tlsListener.Close()
		svr.tlsConfig = nil
	}
	if svr.listener != nil {
		_ = svr.listener.Close()
		svr.listener = nil
	}
	_ = svr.ctlManager.Close()
	if svr.cancel != nil {
		svr.cancel()
	}
	return nil
}

func (svr *Service) handleConnection(ctx context.Context, conn net.Conn, internal bool) {
	xl := xlog.FromContextSafe(ctx)
	var (
		rawMsg msg.Message
		err    error
	)
	_ = conn.SetReadDeadline(time.Now().Add(connReadTimeout))
	if rawMsg, err = msg.ReadMsg(conn); err != nil {
		log.Tracef("Failed to read message: %v", err)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	switch m := rawMsg.(type) {
	case *msg.Login:
		// 登录失败拒绝建立连接
		userMail, exist := m.Metas["email"]
		if !exist {
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error: util.GenerateResponseErrorString("401~",
					models.InitError("email 未指定！重新检查你的配置。", 401),
					lo.FromPtr(svr.cfg.DetailedErrorsToClient)),
			})
			_ = conn.Close()
			return
		}

		_, exist = m.Metas["password"]
		if !exist {
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error: util.GenerateResponseErrorString("401~",
					models.InitError("密码 未指定！重新检查你的配置。", 401),
					lo.FromPtr(svr.cfg.DetailedErrorsToClient)),
			})
			_ = conn.Close()
			return
		}

		_, exist = svr.cfg.Blacklist[userMail]
		if exist {
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error: util.GenerateResponseErrorString("403 gg",
					models.InitError("没流量了你用个屁！给你客户端掐咯。", 403),
					lo.FromPtr(svr.cfg.DetailedErrorsToClient)),
			})
			log.Infof("用户 [%s] 在黑名单中试图建立连接，已被拒绝。", m.Metas["email"])
			_ = conn.Close()
			return
		}

		// server plugin hook
		content := &plugin.LoginContent{
			Login:         *m,
			ClientAddress: conn.RemoteAddr().String(),
		}
		retContent, err := svr.pluginManager.Login(content)
		if err == nil {
			m = &retContent.Login
			// runID 经过这里时回执给客户端，所以要在这里处理runID -> userToken map
			err = svr.RegisterControl(conn, m, internal, userMail)
			log.Infof("%s %s", content.Metas["email"], "登录成功！")
		} else {
			// If login failed, send error message there.
			// Otherwise, send success message in control's work goroutine.
			xl.Warnf("%s 失败: %v", content.ClientAddress, err)
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error: util.GenerateResponseErrorString("401~",
					models.InitError("登陆失败！请检查你的配置。", 401),
					lo.FromPtr(svr.cfg.DetailedErrorsToClient)),
			})
			_ = conn.Close()
		}
	case *msg.NewWorkConn:
		if err := svr.RegisterWorkConn(conn, m); err != nil {
			_ = conn.Close()
		}
	case *msg.NewVisitorConn:
		if err = svr.RegisterVisitorConn(conn, m); err != nil {
			xl.Warnf("register visitor conn error: %v", err)
			_ = msg.WriteMsg(conn, &msg.NewVisitorConnResp{
				ProxyName: m.ProxyName,
				Error:     util.GenerateResponseErrorString("register visitor conn error", err, lo.FromPtr(svr.cfg.DetailedErrorsToClient)),
			})
			_ = conn.Close()
		} else {
			_ = msg.WriteMsg(conn, &msg.NewVisitorConnResp{
				ProxyName: m.ProxyName,
				Error:     "",
			})
		}
	default:
		log.Warnf("Error message type for the new connection [%s]", conn.RemoteAddr().String())
		_ = conn.Close()
	}
}

// HandleListener accepts connections from client and call handleConnection to handle them.
// If internal is true, it means that this listener is used for internal communication like ssh tunnel gateway.
// TODO(fatedier): Pass some parameters of listener/connection through context to avoid passing too many parameters.
func (svr *Service) HandleListener(l net.Listener, internal bool) {
	// Listen for incoming connections from client.
	for {
		c, err := l.Accept()
		if err != nil {
			log.Warnf("Listener for incoming connections from client closed")
			return
		}
		// inject xlog object into net.Conn context
		xl := xlog.New()
		ctx := context.Background()

		c = netpkg.NewContextConn(xlog.NewContext(ctx, xl), c)

		if !internal {
			log.Tracef("start check TLS connection...")
			originConn := c
			forceTLS := svr.cfg.Transport.TLS.Force
			var isTLS, custom bool
			c, isTLS, custom, err = netpkg.CheckAndEnableTLSServerConnWithTimeout(c, svr.tlsConfig, forceTLS, connReadTimeout)
			if err != nil {
				log.Warnf("CheckAndEnableTLSServerConnWithTimeout error: %v", err)
				_ = originConn.Close()
				continue
			}
			log.Tracef("check TLS connection success, isTLS: %v custom: %v internal: %v", isTLS, custom, internal)
		}

		// Start a new goroutine to handle connection.
		go func(ctx context.Context, frpConn net.Conn) {
			if lo.FromPtr(svr.cfg.Transport.TCPMux) && !internal {
				fmuxCfg := fmux.DefaultConfig()
				fmuxCfg.KeepAliveInterval = time.Duration(svr.cfg.Transport.TCPMuxKeepaliveInterval) * time.Second
				fmuxCfg.LogOutput = io.Discard
				fmuxCfg.MaxStreamWindowSize = 6 * 1024 * 1024
				session, err := fmux.Server(frpConn, fmuxCfg)
				if err != nil {
					log.Warnf("Failed to create mux connection: %v", err)
					_ = frpConn.Close()
					return
				}

				for {
					stream, err := session.AcceptStream()
					if err != nil {
						log.Debugf("Accept new mux stream error: %v", err)
						_ = session.Close()
						return
					}
					go svr.handleConnection(ctx, stream, internal)
				}
			} else {
				svr.handleConnection(ctx, frpConn, internal)
			}
		}(ctx, c)
	}
}

func (svr *Service) HandlePenguinRealtimeMsgListener(l net.Listener) {
	for {
		// 监听新的客户端连接
		conn, err := l.Accept()
		if err != nil {
			log.Warnf("[penguin msg] Listener for incoming connections from client closed")
			continue
		}

		var runIDMsg msg.MessageLogin
		err = msg.ReadMsgInto(conn, &runIDMsg)
		if err != nil {
			log.Errorf("[penguin msg] 读取客户端配置文件失败: %v", err)
			continue
		}

		if runIDMsg.RunID == "" {
			continue
		}

		msgConn, exist := svr.cfg.RunIdList[runIDMsg.RunID]
		if !exist {
			continue
		}

		// 绑定conn 方便在任何地方都可以对客户端通讯
		if msgConn.Conn != nil {
			_ = svr.cfg.RunIdList[runIDMsg.RunID].Conn.Close()
		}
		svr.cfg.RunIdList[runIDMsg.RunID].Conn = conn

		msg.WriteRealtimeMsg(conn, "成功连接上msg Server, 通讯服务正常 runID: "+runIDMsg.RunID, 665)
		log.Infof("用户 [%s] 成功连接 msg Server, runID: [%s]", msgConn.Token, runIDMsg.RunID)
	}
}

func (svr *Service) HandleQUICListener(l *quic.Listener) {
	// Listen for incoming connections from client.
	for {
		c, err := l.Accept(context.Background())
		if err != nil {
			log.Warnf("QUICListener for incoming connections from client closed")
			return
		}
		// Start a new goroutine to handle connection.
		go func(ctx context.Context, frpConn quic.Connection) {
			for {
				stream, err := frpConn.AcceptStream(context.Background())
				if err != nil {
					log.Debugf("Accept new quic mux stream error: %v", err)
					_ = frpConn.CloseWithError(0, "")
					return
				}
				go svr.handleConnection(ctx, netpkg.QuicStreamToNetConn(stream, frpConn), false)
			}
		}(context.Background(), c)
	}
}

func (svr *Service) RegisterControl(ctlConn net.Conn, loginMsg *msg.Login, internal bool, userToken string) error {
	// If client's RunID is empty, it's a new client, we just create a new controller.
	// Otherwise, we check if there is one controller has the same run id. If so, we release previous controller and start new one.
	var err error
	if loginMsg.RunID == "" {
		loginMsg.RunID, err = util.RandID()
		if err != nil {
			return err
		}
	}

	ctx := netpkg.NewContextFromConn(ctlConn)
	xl := xlog.FromContextSafe(ctx)
	xl.AppendPrefix(loginMsg.RunID)
	ctx = xlog.NewContext(ctx, xl)
	xl.Infof("client login info: ip [%s] version [%s] hostname [%s] os [%s] arch [%s]",
		ctlConn.RemoteAddr().String(), loginMsg.Version, loginMsg.Hostname, loginMsg.Os, loginMsg.Arch)

	// Check auth.
	authVerifier := svr.authVerifier
	if internal && loginMsg.ClientSpec.AlwaysAuthPass {
		authVerifier = auth.AlwaysPassVerifier
	}
	if err := authVerifier.VerifyLogin(loginMsg); err != nil {
		return err
	}

	// TODO(fatedier): use SessionContext
	ctl, err := NewControl(ctx, svr.rc, svr.pxyManager, svr.pluginManager, authVerifier, ctlConn, !internal, loginMsg, svr.cfg)

	// penguin init timer test alive
	// 确保每个控制器只注册一个监视
	_, exist := svr.cfg.RunIdList[ctl.runID]
	if !exist {
		go func() {
			defer func() {
				delete(svr.cfg.RunIdList, ctl.runID)
				_ = ctl.Close()
			}()

			// 注册客户端的run_id到map中
			svr.cfg.RunIdList[ctl.runID] = &ctl_.ConnMsg{
				Token: userToken,
			}

			for {
				select {
				case <-time.After(5 * time.Second):
					_, exist := svr.cfg.Blacklist[userToken]
					if exist {
						_ = msg.WriteRealtimeMsgByConfig(svr.cfg.RunIdList, loginMsg.RunID, "流量已耗尽，您已无法使用内网穿透，请查看仪表盘或咨询管理员。", 403)
						log.Warnf("用户 [%s] 建立控制器，但流量耗尽，已被强制关闭", loginMsg.Metas["email"])
						_ = ctl.Close()
						return
					}
					// 心跳检测
					var heartErr error
					heartErr = msg.WriteRealtimeMsgByConfig(svr.cfg.RunIdList, loginMsg.RunID, "", 666)
					if heartErr != nil {
						log.Errorf("用户 [%s] 与msg server的tcp连接中断！", loginMsg.Metas["email"])
						return
					}
				}
			}
		}()
	}

	if err != nil {
		xl.Warnf("create new controller error: %v", err)
		// don't return detailed errors to client
		return fmt.Errorf("unexpected error when creating new controller")
	}
	if oldCtl := svr.ctlManager.Add(loginMsg.RunID, ctl); oldCtl != nil {
		oldCtl.WaitClosed()
	}

	ctl.Start()

	// for statistics
	metrics.Server.NewClient()

	go func() {
		// block until control closed
		ctl.WaitClosed()
		svr.ctlManager.Del(loginMsg.RunID, ctl)
	}()
	return nil
}

// RegisterWorkConn register a new work connection to control and proxies need it.
func (svr *Service) RegisterWorkConn(workConn net.Conn, newMsg *msg.NewWorkConn) error {
	xl := netpkg.NewLogFromConn(workConn)
	ctl, exist := svr.ctlManager.GetByID(newMsg.RunID)
	if !exist {
		xl.Warnf("No client control found for run id [%s]", newMsg.RunID)
		return fmt.Errorf("no client control found for run id [%s]", newMsg.RunID)
	}
	// server plugin hook
	content := &plugin.NewWorkConnContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		NewWorkConn: *newMsg,
	}
	retContent, err := svr.pluginManager.NewWorkConn(content)
	if err == nil {
		newMsg = &retContent.NewWorkConn
		// Check auth.
		err = ctl.authVerifier.VerifyNewWorkConn(newMsg)

		// flow test
		_, exist = svr.cfg.Blacklist[content.User.Metas["email"]]
		if exist {
			_ = workConn.Close()
		} else {
			go func(c net.Conn) {
				defer func(c net.Conn) {
					_ = c.Close()
				}(c)
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						// 写入空字节触发心跳机制
						_, err := c.Write([]byte(""))
						if err != nil {
							return
						}
						// 查询黑名单
						_, exist = svr.cfg.Blacklist[content.User.Metas["email"]]
						if exist {
							// 流量没了的情况下 直接杀死！
							return
						}
					}
				}
			}(workConn)
		}
	}
	if err != nil {
		xl.Warnf("invalid NewWorkConn with run id [%s]", newMsg.RunID)
		_ = msg.WriteMsg(workConn, &msg.StartWorkConn{
			Error: util.GenerateResponseErrorString("invalid NewWorkConn", err, lo.FromPtr(svr.cfg.DetailedErrorsToClient)),
		})
		return fmt.Errorf("invalid NewWorkConn with run id [%s]", newMsg.RunID)
	}
	return ctl.RegisterWorkConn(workConn)
}

func (svr *Service) RegisterVisitorConn(visitorConn net.Conn, newMsg *msg.NewVisitorConn) error {
	visitorUser := ""
	// TODO(deprecation): Compatible with old versions, can be without runID, user is empty. In later versions, it will be mandatory to include runID.
	// If runID is required, it is not compatible with versions prior to v0.50.0.
	if newMsg.RunID != "" {
		ctl, exist := svr.ctlManager.GetByID(newMsg.RunID)
		if !exist {
			return fmt.Errorf("no client control found for run id [%s]", newMsg.RunID)
		}
		visitorUser = ctl.loginMsg.User
	}
	return svr.rc.VisitorManager.NewConn(newMsg.ProxyName, visitorConn, newMsg.Timestamp, newMsg.SignKey,
		newMsg.UseEncryption, newMsg.UseCompression, visitorUser)
}
