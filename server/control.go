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
	"context"
	"fmt"
	"github.com/SoHugePenguin/frp/pkg/util/log"
	"github.com/SoHugePenguin/frp/server/models/mysql"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samber/lo"

	"github.com/SoHugePenguin/frp/pkg/auth"
	"github.com/SoHugePenguin/frp/pkg/config"
	v1 "github.com/SoHugePenguin/frp/pkg/config/v1"
	pkgerr "github.com/SoHugePenguin/frp/pkg/errors"
	"github.com/SoHugePenguin/frp/pkg/msg"
	plugin "github.com/SoHugePenguin/frp/pkg/plugin/server"
	"github.com/SoHugePenguin/frp/pkg/transport"
	netpkg "github.com/SoHugePenguin/frp/pkg/util/net"
	"github.com/SoHugePenguin/frp/pkg/util/util"
	"github.com/SoHugePenguin/frp/pkg/util/version"
	"github.com/SoHugePenguin/frp/pkg/util/wait"
	"github.com/SoHugePenguin/frp/pkg/util/xlog"
	"github.com/SoHugePenguin/frp/server/controller"
	"github.com/SoHugePenguin/frp/server/metrics"
	"github.com/SoHugePenguin/frp/server/proxy"
)

type ControlManager struct {
	// controls indexed by run id
	ctlsByRunID map[string]*Control

	mu sync.RWMutex
}

func NewControlManager() *ControlManager {
	return &ControlManager{
		ctlsByRunID: make(map[string]*Control),
	}
}

func (cm *ControlManager) Add(runID string, ctl *Control) (old *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	var ok bool
	old, ok = cm.ctlsByRunID[runID]
	if ok {
		old.Replaced(ctl)
	}
	cm.ctlsByRunID[runID] = ctl
	return
}

// Del we should make sure if it's the same control to prevent delete a new one
func (cm *ControlManager) Del(runID string, ctl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if c, ok := cm.ctlsByRunID[runID]; ok && c == ctl {
		delete(cm.ctlsByRunID, runID)
	}
}

func (cm *ControlManager) GetByID(runID string) (ctl *Control, ok bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctl, ok = cm.ctlsByRunID[runID]
	return
}

func (cm *ControlManager) Close() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, ctl := range cm.ctlsByRunID {
		_ = ctl.Close()
	}
	cm.ctlsByRunID = make(map[string]*Control)
	return nil
}

type Control struct {
	// all resource managers and controllers
	rc *controller.ResourceController

	// proxy manager
	pxyManager *proxy.Manager

	// plugin manager
	pluginManager *plugin.Manager

	// verifies authentication based on selected method
	authVerifier auth.Verifier

	// other components can use this to communicate with client
	msgTransporter transport.MessageTransporter

	// msgDispatcher is a wrapper for control connection.
	// It provides a channel for sending messages, and you can register handlers to process messages based on their respective types.
	msgDispatcher *msg.Dispatcher

	// login message
	loginMsg *msg.Login

	// control connection
	conn net.Conn

	// work connections
	workConnCh chan net.Conn

	// proxies in one client
	proxies map[string]proxy.Proxy

	// pool count
	poolCount int

	// ports used, for limitations
	portsUsedNum int

	// last time got the Ping message
	lastPing atomic.Value

	// A new run id will be generated when a new client login.
	// If run id got from login message has same run id, it means it's the same client, so we can
	// replace old controller instantly.
	runID string

	mu sync.RWMutex

	// Server configuration information
	serverCfg *v1.ServerConfig

	xl     *xlog.Logger
	ctx    context.Context
	doneCh chan struct{}
}

// NewControl TODO(fatedier): Referencing the implementation of frpc, encapsulate the input parameters as SessionContext.
func NewControl(
	ctx context.Context,
	rc *controller.ResourceController,
	pxyManager *proxy.Manager,
	pluginManager *plugin.Manager,
	authVerifier auth.Verifier,
	ctlConn net.Conn,
	ctlConnEncrypted bool,
	loginMsg *msg.Login,
	serverCfg *v1.ServerConfig,
) (*Control, error) {
	poolCount := loginMsg.PoolCount
	if poolCount > int(serverCfg.Transport.MaxPoolCount) {
		poolCount = int(serverCfg.Transport.MaxPoolCount)
	}
	ctl := &Control{
		rc:            rc,
		pxyManager:    pxyManager,
		pluginManager: pluginManager,
		authVerifier:  authVerifier,
		conn:          ctlConn,
		loginMsg:      loginMsg,
		workConnCh:    make(chan net.Conn, poolCount+10),
		proxies:       make(map[string]proxy.Proxy),
		poolCount:     poolCount,
		portsUsedNum:  0,
		runID:         loginMsg.RunID,
		serverCfg:     serverCfg,
		xl:            xlog.FromContextSafe(ctx),
		ctx:           ctx,
		doneCh:        make(chan struct{}),
	}
	ctl.lastPing.Store(time.Now())

	if ctlConnEncrypted {
		cryptoRW, err := netpkg.NewCryptoReadWriter(ctl.conn, []byte(ctl.serverCfg.Auth.Token))
		if err != nil {
			return nil, err
		}
		ctl.msgDispatcher = msg.NewDispatcher(cryptoRW)
	} else {
		ctl.msgDispatcher = msg.NewDispatcher(ctl.conn)
	}
	ctl.registerMsgHandlers()
	ctl.msgTransporter = transport.NewMessageTransporter(ctl.msgDispatcher.SendChannel())
	return ctl, nil
}

// Start send a login success message to client and start working.
func (ctl *Control) Start() {
	loginRespMsg := &msg.LoginResp{
		Version: version.Full(),
		RunID:   ctl.runID,
		Error:   "",
	}
	_ = msg.WriteMsg(ctl.conn, loginRespMsg)

	go func() {
		for i := 0; i < ctl.poolCount; i++ {
			// ignore error here, that means that this control is closed
			_ = ctl.msgDispatcher.Send(&msg.ReqWorkConn{})
		}
	}()
	go ctl.worker()
}

func (ctl *Control) Close() error {
	_ = ctl.conn.Close()
	return nil
}

func (ctl *Control) Replaced(newCtl *Control) {
	xl := ctl.xl
	xl.Infof("Replaced by client [%s]", newCtl.runID)
	ctl.runID = ""
	_ = ctl.conn.Close()
}

func (ctl *Control) RegisterWorkConn(conn net.Conn) error {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Errorf("panic error: %v", err)
			xl.Errorf(string(debug.Stack()))
		}
	}()

	select {
	case ctl.workConnCh <- conn:
		xl.Debugf("new work connection registered")
		return nil
	default:
		xl.Debugf("work connection pool is full, discarding")
		return fmt.Errorf("work connection pool is full, discarding")
	}
}

// GetWorkConn When frps get one user connection, we get one work connection from the pool and return it.
// If no workConn available in the pool, send message to frpc to get one or more
// and wait until it is available.
// return an error if wait timeout
func (ctl *Control) GetWorkConn() (workConn net.Conn, err error) {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Errorf("panic error: %v", err)
			xl.Errorf(string(debug.Stack()))
		}
	}()

	var ok bool
	// get a work connection from the pool
	select {
	case workConn, ok = <-ctl.workConnCh:
		if !ok {
			err = pkgerr.ErrCtlClosed
			return
		}
		xl.Debugf("get work connection from pool")
	default:
		// no work connections available in the poll, send message to frpc to get more
		if err := ctl.msgDispatcher.Send(&msg.ReqWorkConn{}); err != nil {
			return nil, fmt.Errorf("control is already closed")
		}

		select {
		case workConn, ok = <-ctl.workConnCh:
			if !ok {
				err = pkgerr.ErrCtlClosed
				xl.Warnf("no work connections available, %v", err)
				return
			}

		case <-time.After(time.Duration(ctl.serverCfg.UserConnTimeout) * time.Second):
			err = fmt.Errorf("timeout trying to get work connection")
			xl.Warnf("%v", err)
			return
		}
	}

	// When we get a work connection from pool, replace it with a new one.
	_ = ctl.msgDispatcher.Send(&msg.ReqWorkConn{})
	return
}

func (ctl *Control) heartbeatWorker() {
	if ctl.serverCfg.Transport.HeartbeatTimeout <= 0 {
		return
	}

	xl := ctl.xl
	go wait.Until(func() {
		if time.Since(ctl.lastPing.Load().(time.Time)) > time.Duration(ctl.serverCfg.Transport.HeartbeatTimeout)*time.Second {
			xl.Warnf("heartbeat timeout")
			_ = ctl.conn.Close()
			return
		}
	}, time.Second, ctl.doneCh)
}

// WaitClosed block until Control closed
func (ctl *Control) WaitClosed() {
	<-ctl.doneCh
}

func (ctl *Control) worker() {
	xl := ctl.xl

	go ctl.heartbeatWorker()
	go ctl.msgDispatcher.Run()

	<-ctl.msgDispatcher.Done()
	_ = ctl.conn.Close()

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	close(ctl.workConnCh)
	for workConn := range ctl.workConnCh {
		_ = workConn.Close()
	}

	for _, pxy := range ctl.proxies {
		pxy.Close()
		ctl.pxyManager.Del(pxy.GetName())
		metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConfigure().GetBaseConfig().Type)

		notifyContent := &plugin.CloseProxyContent{
			User: plugin.UserInfo{
				User:  ctl.loginMsg.User,
				Metas: ctl.loginMsg.Metas,
				RunID: ctl.loginMsg.RunID,
			},
			CloseProxy: msg.CloseProxy{
				ProxyName: pxy.GetName(),
			},
		}
		go func() {
			_ = ctl.pluginManager.CloseProxy(notifyContent)
		}()
	}

	metrics.Server.CloseClient()
	xl.Infof("client exit success")
	close(ctl.doneCh)
}

func (ctl *Control) registerMsgHandlers() {
	ctl.msgDispatcher.RegisterHandler(&msg.NewProxy{}, ctl.handleNewProxy)
	ctl.msgDispatcher.RegisterHandler(&msg.Ping{}, ctl.handlePing)
	ctl.msgDispatcher.RegisterHandler(&msg.NatHoleVisitor{}, msg.AsyncHandler(ctl.handleNatHoleVisitor))
	ctl.msgDispatcher.RegisterHandler(&msg.NatHoleClient{}, msg.AsyncHandler(ctl.handleNatHoleClient))
	ctl.msgDispatcher.RegisterHandler(&msg.NatHoleReport{}, msg.AsyncHandler(ctl.handleNatHoleReport))
	ctl.msgDispatcher.RegisterHandler(&msg.CloseProxy{}, ctl.handleCloseProxy)
}

func (ctl *Control) handleNewProxy(m msg.Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Debugf(r.(error).Error())
		}
	}()

	// 大坑，首次运行没有conn，登录成功后才有，所以要等2秒。
	if ctl.serverCfg.RunIdList[ctl.runID].Conn == nil {
		<-time.After(2 * time.Second)
	}

	// 从创建代理开始就要判断是否为黑名单用户，减少资源浪费。
	userToken := ctl.loginMsg.Metas["token"]
	_, exist := ctl.serverCfg.Blacklist[userToken]
	if exist {
		log.Infof("用户 [%s] 在黑名单中试图建立连接，已被拒绝。", ctl.loginMsg.Metas["token"])
		return
	}

	// 查看用户token是否存在与数据库t_user中
	var user mysql.User
	var myProxy mysql.Proxy
	db := ctl.serverCfg.MysqlDBConnect
	db.Select("id").Where("user_token = ?", userToken).First(&user)
	if user.ID == 0 {
		log.Errorf("用户 [%s] 不存在，已拒绝建立代理连接", userToken)
		return
	}

	xl := ctl.xl
	inMsg := m.(*msg.NewProxy)

	// 仅允许localAddr 和 localPort 可以自定义
	db.Where("proxy_name = ?", inMsg.ProxyName).First(&myProxy)
	// 未从数据库查到
	if myProxy.ID == 0 {
		_ = msg.WriteRealtimeMsgByConfig(ctl.serverCfg.RunIdList, ctl.runID,
			fmt.Sprintf("代理token不正确，请检查你的 [%s] 代理配置!", inMsg.ProxyName), 403)
		log.Errorf("用户 [%s] 申请的代理连接ProxyName与数据库不匹配", userToken)
		return
	}
	if inMsg.ProxyType != myProxy.ProxyType {
		log.Errorf("用户 [%s] 申请的代理连接ProxyType与数据库不匹配", userToken)
		return
	}
	if inMsg.RemotePort != myProxy.RemotePort {
		log.Errorf("用户 [%s] 申请的代理连接RemotePort与数据库不匹配", userToken)
		return
	}

	// success
	log.Warnf("用户 [%s] 成功建立代理连接 {name:%s, type:%s, port:%d}",
		userToken, myProxy.ProxyName, myProxy.ProxyType, myProxy.RemotePort)

	// 流量速率限制初始化
	inMsg.MaxInRate = myProxy.MaxInRate
	inMsg.MaxOutRate = myProxy.MaxOutRate

	content := &plugin.NewProxyContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		NewProxy: *inMsg,
	}
	var remoteAddr string
	retContent, err := ctl.pluginManager.NewProxy(content)
	if err == nil {
		inMsg = &retContent.NewProxy
		remoteAddr, err = ctl.RegisterProxy(inMsg)
	}

	// register proxy in this control
	resp := &msg.NewProxyResp{
		ProxyName: inMsg.ProxyName,
	}
	if err != nil {
		xl.Warnf("new proxy [%s] type [%s] error: %v", inMsg.ProxyName, inMsg.ProxyType, err)
		resp.Error = util.GenerateResponseErrorString(fmt.Sprintf("new proxy [%s] error", inMsg.ProxyName),
			err, lo.FromPtr(ctl.serverCfg.DetailedErrorsToClient))
	} else {
		resp.RemoteAddr = remoteAddr
		//xl.Infof("new proxy [%s] type [%s] success", inMsg.ProxyName, inMsg.ProxyType)
		metrics.Server.NewProxy(inMsg.ProxyName, inMsg.ProxyType)
	}
	_ = ctl.msgDispatcher.Send(resp)
}

func (ctl *Control) handlePing(m msg.Message) {
	xl := ctl.xl
	inMsg := m.(*msg.Ping)

	content := &plugin.PingContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		Ping: *inMsg,
	}
	retContent, err := ctl.pluginManager.Ping(content)
	if err == nil {
		inMsg = &retContent.Ping
		err = ctl.authVerifier.VerifyPing(inMsg)
	}
	if err != nil {
		xl.Warnf("received invalid ping: %v", err)
		_ = ctl.msgDispatcher.Send(&msg.Pong{
			Error: util.GenerateResponseErrorString("invalid ping", err, lo.FromPtr(ctl.serverCfg.DetailedErrorsToClient)),
		})
		return
	}
	ctl.lastPing.Store(time.Now())
	xl.Debugf("receive heartbeat")
	_ = ctl.msgDispatcher.Send(&msg.Pong{})
}

func (ctl *Control) handleNatHoleVisitor(m msg.Message) {
	inMsg := m.(*msg.NatHoleVisitor)
	ctl.rc.NatHoleController.HandleVisitor(inMsg, ctl.msgTransporter, ctl.loginMsg.User)
}

func (ctl *Control) handleNatHoleClient(m msg.Message) {
	inMsg := m.(*msg.NatHoleClient)
	ctl.rc.NatHoleController.HandleClient(inMsg, ctl.msgTransporter)
}

func (ctl *Control) handleNatHoleReport(m msg.Message) {
	inMsg := m.(*msg.NatHoleReport)
	ctl.rc.NatHoleController.HandleReport(inMsg)
}

func (ctl *Control) handleCloseProxy(m msg.Message) {
	xl := ctl.xl
	inMsg := m.(*msg.CloseProxy)
	_ = ctl.CloseProxy(inMsg)
	xl.Infof("close proxy [%s] success", inMsg.ProxyName)
}

func (ctl *Control) RegisterProxy(pxyMsg *msg.NewProxy) (remoteAddr string, err error) {
	var pxyConf v1.ProxyConfigure
	// Load configures from NewProxy message and validate.
	pxyConf, err = config.NewProxyConfigurerFromMsg(pxyMsg, ctl.serverCfg)
	if err != nil {
		return
	}

	// User info
	userInfo := plugin.UserInfo{
		User:  ctl.loginMsg.User,
		Metas: ctl.loginMsg.Metas,
		RunID: ctl.runID,
	}

	// NewProxy will return an interface Proxy.
	// In fact, it creates different proxies based on the proxy type. We just call run() here.
	pxy, err := proxy.NewProxy(ctl.ctx, &proxy.Options{
		UserInfo:           userInfo,
		LoginMsg:           ctl.loginMsg,
		PoolCount:          ctl.poolCount,
		ResourceController: ctl.rc,
		GetWorkConnFn:      ctl.GetWorkConn,
		Configure:          pxyConf,
		ServerCfg:          ctl.serverCfg,
		MaxInRate:          pxyMsg.MaxInRate,
		MaxOutRate:         pxyMsg.MaxOutRate,
		CtlRunId:           ctl.runID,
	})
	if err != nil {
		return remoteAddr, err
	}

	// Check ports used number in each client
	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.mu.Lock()
		if ctl.portsUsedNum+pxy.GetUsedPortsNum() > int(ctl.serverCfg.MaxPortsPerClient) {
			ctl.mu.Unlock()
			err = fmt.Errorf("exceed the max_ports_per_client")
			return
		}
		ctl.portsUsedNum += pxy.GetUsedPortsNum()
		ctl.mu.Unlock()

		defer func() {
			if err != nil {
				ctl.mu.Lock()
				ctl.portsUsedNum -= pxy.GetUsedPortsNum()
				ctl.mu.Unlock()
			}
		}()
	}

	if ctl.pxyManager.Exist(pxyMsg.ProxyName) {
		err = fmt.Errorf("proxy [%s] already exists", pxyMsg.ProxyName)
		return
	}

	// 注册速率重置定时器
	pxy.InitTimer()
	remoteAddr, err = pxy.Run()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			pxy.Close()
		}
	}()

	err = ctl.pxyManager.Add(pxyMsg.ProxyName, pxy)
	if err != nil {
		return
	}

	ctl.mu.Lock()
	ctl.proxies[pxy.GetName()] = pxy
	ctl.mu.Unlock()
	return
}

func (ctl *Control) CloseProxy(closeMsg *msg.CloseProxy) (err error) {
	ctl.mu.Lock()
	pxy, ok := ctl.proxies[closeMsg.ProxyName]
	if !ok {
		ctl.mu.Unlock()
		return
	}

	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.portsUsedNum -= pxy.GetUsedPortsNum()
	}
	pxy.Close()
	ctl.pxyManager.Del(pxy.GetName())
	delete(ctl.proxies, closeMsg.ProxyName)
	ctl.mu.Unlock()

	metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConfigure().GetBaseConfig().Type)

	notifyContent := &plugin.CloseProxyContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		CloseProxy: msg.CloseProxy{
			ProxyName: pxy.GetName(),
		},
	}
	go func() {
		_ = ctl.pluginManager.CloseProxy(notifyContent)
	}()
	return
}
