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

package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SoHugePenguin/golib/crypto"
	"github.com/samber/lo"

	"github.com/SoHugePenguin/frp/client/proxy"
	"github.com/SoHugePenguin/frp/pkg/auth"
	v1 "github.com/SoHugePenguin/frp/pkg/config/v1"
	"github.com/SoHugePenguin/frp/pkg/msg"
	httppkg "github.com/SoHugePenguin/frp/pkg/util/http"
	"github.com/SoHugePenguin/frp/pkg/util/log"
	netpkg "github.com/SoHugePenguin/frp/pkg/util/net"
	"github.com/SoHugePenguin/frp/pkg/util/version"
	"github.com/SoHugePenguin/frp/pkg/util/wait"
	"github.com/SoHugePenguin/frp/pkg/util/xlog"
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

type cancelErr struct {
	Err error
}

func (e cancelErr) Error() string {
	return e.Err.Error()
}

// ServiceOptions contains options for creating a new client service.
type ServiceOptions struct {
	Common      *v1.ClientCommonConfig
	ProxyCfgs   []v1.ProxyConfigure
	VisitorCfgs []v1.VisitorConfigurer

	// ConfigFilePath is the path to the configuration file used to initialize.
	// If it is empty, it means that the configuration file is not used for initialization.
	// It may be initialized using command line parameters or called directly.
	ConfigFilePath string

	// ClientSpec is the client specification that control the client behavior.
	ClientSpec *msg.ClientSpec

	// ConnectorCreator is a function that creates a new connector to make connections to the server.
	// The Connector shields the underlying connection details, whether it is through TCP or QUIC connection,
	// and regardless of whether multiplexing is used.
	//
	// If it is not set, the default frpc connector will be used.
	// By using a custom Connector, it can be used to implement a VirtualClient, which connects to frps
	// through a pipe instead of a real physical connection.
	ConnectorCreator func(context.Context, *v1.ClientCommonConfig) Connector

	// HandleWorkConnCb is a callback function that is called when a new work connection is created.
	//
	// If it is not set, the default frpc implementation will be used.
	HandleWorkConnCb func(*v1.ProxyBaseConfig, net.Conn, *msg.StartWorkConn) bool
}

// setServiceOptionsDefault sets the default values for ServiceOptions.
func setServiceOptionsDefault(options *ServiceOptions) {
	if options.Common != nil {
		options.Common.Complete()
	}
	if options.ConnectorCreator == nil {
		options.ConnectorCreator = NewConnector
	}
}

// Service is the client service that connects to frps and provides proxy services.
type Service struct {
	ctlMu sync.RWMutex
	// manager control connection with server
	ctl *Control
	// Uniq id got from frps, it will be attached to loginMsg.
	runID string

	// Sets authentication based on selected method
	authSetter auth.Setter

	// web server for admin UI and apis
	webServer *httppkg.Server

	cfgMu       sync.RWMutex
	common      *v1.ClientCommonConfig
	proxyCfgs   []v1.ProxyConfigure
	visitorCfgs []v1.VisitorConfigurer
	clientSpec  *msg.ClientSpec

	// The configuration file used to initialize this client, or an empty
	// string if no configuration file was used.
	configFilePath string

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel                   context.CancelCauseFunc
	gracefulShutdownDuration time.Duration

	connectorCreator func(context.Context, *v1.ClientCommonConfig) Connector
	handleWorkConnCb func(*v1.ProxyBaseConfig, net.Conn, *msg.StartWorkConn) bool
}

func NewService(options ServiceOptions) (*Service, error) {
	setServiceOptionsDefault(&options)

	var webServer *httppkg.Server
	if options.Common.WebServer.Port > 0 {
		ws, err := httppkg.NewServer(options.Common.WebServer)
		if err != nil {
			return nil, err
		}
		webServer = ws
	}
	s := &Service{
		ctx:              context.Background(),
		authSetter:       auth.NewAuthSetter(options.Common.Auth),
		webServer:        webServer,
		common:           options.Common,
		configFilePath:   options.ConfigFilePath,
		proxyCfgs:        options.ProxyCfgs,
		visitorCfgs:      options.VisitorCfgs,
		clientSpec:       options.ClientSpec,
		connectorCreator: options.ConnectorCreator,
		handleWorkConnCb: options.HandleWorkConnCb,
	}
	if webServer != nil {
		webServer.RouteRegister(s.registerRouteHandlers)
	}
	return s, nil
}

func (svr *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	svr.ctx = xlog.NewContext(ctx, xlog.FromContextSafe(ctx))
	svr.cancel = cancel

	// set custom DNSServer
	if svr.common.DNSServer != "" {
		netpkg.SetDefaultDNSAddress(svr.common.DNSServer)
	}

	if svr.webServer != nil {
		go func() {
			log.Infof("admin server listen on %s", svr.webServer.Address())
			if err := svr.webServer.Run(); err != nil {
				log.Warnf("admin server exit with error: %v", err)
			}
		}()
	}

	// first login to frps
	svr.loopLoginUntilSuccess(10*time.Second, lo.FromPtr(svr.common.LoginFailExit))
	if svr.ctl == nil {
		cancelCause := cancelErr{}
		_ = errors.As(context.Cause(svr.ctx), &cancelCause)
		return fmt.Errorf("login to the server failed: %v. With loginFailExit enabled, no additional retries will be attempted", cancelCause.Err)
	}

	// penguin login to msgServer
	go svr.loginMsgServer()

	go svr.keepControllerWorking()

	<-svr.ctx.Done()
	svr.stop()
	return nil
}

func (svr *Service) keepControllerWorking() {
	<-svr.ctl.Done()

	// There is a situation where the login is successful but due to certain reasons,
	// the control immediately exits. It is necessary to limit the frequency of reconnection in this case.
	// The interval for the first three retries in 1 minute will be very short, and then it will increase exponentially.
	// The maximum interval is 20 seconds.
	wait.BackoffUntil(func() (bool, error) {
		// loopLoginUntilSuccess is another layer of loop that will continuously attempt to
		// login to the server until successful.
		svr.loopLoginUntilSuccess(20*time.Second, false)
		if svr.ctl != nil {
			<-svr.ctl.Done()
			return false, errors.New("control is closed and try another loop")
		}
		// If the control is nil, it means that the login failed and the service is also closed.
		return false, nil
	}, wait.NewFastBackoffManager(
		wait.FastBackoffOptions{
			Duration:        time.Second,
			Factor:          2,
			Jitter:          0.1,
			MaxDuration:     20 * time.Second,
			FastRetryCount:  3,
			FastRetryDelay:  200 * time.Millisecond,
			FastRetryWindow: time.Minute,
			FastRetryJitter: 0.5,
		},
	), true, svr.ctx.Done())
}

// login creates a connection to frps and registers it self as a client
// conn: control connection
// session: if it's not nil, using tcp mux
func (svr *Service) login() (conn net.Conn, connector Connector, err error) {
	xl := xlog.FromContextSafe(svr.ctx)
	connector = svr.connectorCreator(svr.ctx, svr.common)
	if err = connector.Open(); err != nil {
		return nil, nil, err
	}

	defer func() {
		if err != nil {
			_ = connector.Close()
		}
	}()

	conn, err = connector.Connect()
	if err != nil {
		return
	}

	loginMsg := &msg.Login{
		Arch:      runtime.GOARCH,
		Os:        runtime.GOOS,
		PoolCount: svr.common.Transport.PoolCount,
		User:      svr.common.User,
		Version:   version.Full(),
		Timestamp: time.Now().Unix(),
		RunID:     svr.runID,
		Metas:     svr.common.Metadatas,
	}
	if svr.clientSpec != nil {
		loginMsg.ClientSpec = *svr.clientSpec
	}

	// Add auth
	if err = svr.authSetter.SetLogin(loginMsg); err != nil {
		return
	}

	if err = msg.WriteMsg(conn, loginMsg); err != nil {
		return
	}

	var loginRespMsg msg.LoginResp
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = msg.ReadMsgInto(conn, &loginRespMsg); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if loginRespMsg.Error != "" {
		// 未登录 || 流量不足
		if strings.Contains(loginRespMsg.Error, "401") ||
			strings.Contains(loginRespMsg.Error, "403") {
			xl.Errorf("%s", loginRespMsg.Error)
			os.Exit(0)
		}
		xl.Errorf("%s", loginRespMsg.Error)
		return
	}

	svr.runID = loginRespMsg.RunID
	xl.AddPrefix(xlog.LogPrefix{Name: "runID", Value: svr.runID})

	xl.Infof("login to server success, get run id [%s]", loginRespMsg.RunID)
	return
}

func (svr *Service) loopLoginUntilSuccess(maxInterval time.Duration, firstLoginExit bool) {
	xl := xlog.FromContextSafe(svr.ctx)

	loginFunc := func() (bool, error) {
		xl.Infof("try to connect to server...")
		conn, connector, err := svr.login()
		if err != nil {
			xl.Warnf("connect to server error: %v", err)
			if firstLoginExit {
				svr.cancel(cancelErr{Err: err})
			}
			return false, err
		}

		svr.cfgMu.RLock()
		proxyCfgs := svr.proxyCfgs
		visitorCfgs := svr.visitorCfgs
		svr.cfgMu.RUnlock()
		connEncrypted := true
		if svr.clientSpec != nil && svr.clientSpec.Type == "ssh-tunnel" {
			connEncrypted = false
		}
		sessionCtx := &SessionContext{
			Common:        svr.common,
			RunID:         svr.runID,
			Conn:          conn,
			ConnEncrypted: connEncrypted,
			AuthSetter:    svr.authSetter,
			Connector:     connector,
		}
		ctl, err := NewControl(svr.ctx, sessionCtx)
		if err != nil {
			_ = conn.Close()
			xl.Errorf("NewControl error: %v", err)
			return false, err
		}
		ctl.SetInWorkConnCallback(svr.handleWorkConnCb)

		ctl.Run(proxyCfgs, visitorCfgs)
		// close and replace previous control
		svr.ctlMu.Lock()
		if svr.ctl != nil {
			_ = svr.ctl.Close()
		}
		svr.ctl = ctl
		svr.ctlMu.Unlock()
		return true, nil
	}

	// try to reconnect to server until success
	wait.BackoffUntil(loginFunc, wait.NewFastBackoffManager(
		wait.FastBackoffOptions{
			Duration:    time.Second,
			Factor:      2,
			Jitter:      0.1,
			MaxDuration: maxInterval,
		}), true, svr.ctx.Done())
}

// penguin init
func (svr *Service) loginMsgServer() {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf(err.(error).Error())
		}
	}()

	// 建立 TCP 连接
	conn, err := net.Dial("tcp", net.JoinHostPort(svr.common.ServerAddr, strconv.Itoa(svr.common.ServerMsgPort)))
	if err != nil {
		fmt.Println("Error connecting to server:", err)
		return
	}

	err = msg.WriteMsg(conn, &msg.MessageLogin{
		Version: version.Full(),
		RunID:   svr.runID,
	})
	if err != nil {
		log.Errorf("%s", err)
	}

	// 启动一个 goroutine 来持续接收服务端的消息
	timeout := 6

	go func() {
		defer func() {
			if err := recover(); err != nil {
				log.Errorf(err.(error).Error())
			}
		}()

		for {
			var realMsg msg.RealTimeMsg
			err = msg.ReadMsgInto(conn, &realMsg)
			if err != nil {
				continue
			}
			switch realMsg.Code {
			case 200:
				log.Infof("[服务端消息] %s , ( Code %d )", realMsg.Text, realMsg.Code)
				break
			case 665:
				log.Infof("[服务端消息] %s , ( 初始化成功 )", realMsg.Text)
				timeout = 6
				break
			case 666:
				log.Debugf("服务端心跳正常")
				timeout = 6
				break
			default:
				log.Errorf("[服务端消息] %s , ( Code %d )", realMsg.Text, realMsg.Code)
			}
		}
	}()

	connChan := make(chan net.Conn)
	errChan := make(chan error)
	go func() {
		for {
			// 写入空字节触发心跳机制
			if timeout <= 0 {
				log.Errorf("msg server 断连了。")

				go func() {
					_ = conn.Close()
					conn, err = net.Dial("tcp", net.JoinHostPort(svr.common.ServerAddr, strconv.Itoa(svr.common.ServerMsgPort)))
					if err != nil {
						errChan <- err
						return
					}
					connChan <- conn
				}()

				select {
				case conn = <-connChan:
					err = msg.WriteMsg(conn, &msg.MessageLogin{
						Version: version.Full(),
						RunID:   svr.runID,
					})
					if err != nil {
						log.Errorf("%s", err)
					} else {
						timeout = 6
					}
				case err = <-errChan:
					log.Errorf("msg server 重连失败: %s", err)
					continue
				case <-time.After(10 * time.Second):
					log.Errorf("连接超时")
					continue
				}
			}
			timeout--
			<-time.After(2 * time.Second)
		}
	}()

}

func (svr *Service) UpdateAllConfigure(proxyCfgs []v1.ProxyConfigure, visitorCfgs []v1.VisitorConfigurer) error {
	svr.cfgMu.Lock()
	svr.proxyCfgs = proxyCfgs
	svr.visitorCfgs = visitorCfgs
	svr.cfgMu.Unlock()

	svr.ctlMu.RLock()
	ctl := svr.ctl
	svr.ctlMu.RUnlock()

	if ctl != nil {
		return svr.ctl.UpdateAllConfigurer(proxyCfgs, visitorCfgs)
	}
	return nil
}

func (svr *Service) Close() {
	svr.GracefulClose(time.Duration(0))
}

func (svr *Service) GracefulClose(d time.Duration) {
	svr.gracefulShutdownDuration = d
	svr.cancel(nil)
}

func (svr *Service) stop() {
	svr.ctlMu.Lock()
	defer svr.ctlMu.Unlock()
	if svr.ctl != nil {
		_ = svr.ctl.GracefulClose(svr.gracefulShutdownDuration)
		svr.ctl = nil
	}
}

func (svr *Service) getProxyStatus(name string) (*proxy.WorkingStatus, bool) {
	svr.ctlMu.RLock()
	ctl := svr.ctl
	svr.ctlMu.RUnlock()

	if ctl == nil {
		return nil, false
	}
	return ctl.pm.GetProxyStatus(name)
}

func (svr *Service) StatusExporter() StatusExporter {
	return &statusExporterImpl{
		getProxyStatusFunc: svr.getProxyStatus,
	}
}

type StatusExporter interface {
	GetProxyStatus(name string) (*proxy.WorkingStatus, bool)
}

type statusExporterImpl struct {
	getProxyStatusFunc func(name string) (*proxy.WorkingStatus, bool)
}

func (s *statusExporterImpl) GetProxyStatus(name string) (*proxy.WorkingStatus, bool) {
	return s.getProxyStatusFunc(name)
}
