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

package proxy

import (
	"context"
	"fmt"
	"github.com/SoHugePenguin/frp/pkg/metrics/mem"
	"github.com/SoHugePenguin/frp/server/models/mysql"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	libio "github.com/SoHugePenguin/golib/io"
	"golang.org/x/time/rate"

	"github.com/SoHugePenguin/frp/pkg/config/types"
	v1 "github.com/SoHugePenguin/frp/pkg/config/v1"
	"github.com/SoHugePenguin/frp/pkg/msg"
	plugin "github.com/SoHugePenguin/frp/pkg/plugin/server"
	"github.com/SoHugePenguin/frp/pkg/util/limit"
	netpkg "github.com/SoHugePenguin/frp/pkg/util/net"
	"github.com/SoHugePenguin/frp/pkg/util/xlog"
	"github.com/SoHugePenguin/frp/server/controller"
	"github.com/SoHugePenguin/frp/server/metrics"
)

var proxyFactoryRegistry = map[reflect.Type]func(*BaseProxy) Proxy{}

func RegisterProxyFactory(proxyConfType reflect.Type, factory func(*BaseProxy) Proxy) {
	proxyFactoryRegistry[proxyConfType] = factory
}

type GetWorkConnFn func() (net.Conn, error)

type Proxy interface {
	Context() context.Context
	Run() (remoteAddr string, err error)
	GetName() string
	GetConfigure() v1.ProxyConfigure
	GetWorkConnFromPool(src, dst net.Addr) (workConn net.Conn, err error)
	GetUsedPortsNum() int
	GetResourceController() *controller.ResourceController
	GetUserInfo() plugin.UserInfo
	GetLimiter() *rate.Limiter
	GetLoginMsg() *msg.Login
	Close()
	InitTimer()
}

type BaseProxy struct {
	name          string
	rc            *controller.ResourceController
	listeners     []net.Listener
	usedPortsNum  int
	poolCount     int
	getWorkConnFn GetWorkConnFn
	serverCfg     *v1.ServerConfig
	limiter       *rate.Limiter
	userInfo      plugin.UserInfo
	loginMsg      *msg.Login
	configure     v1.ProxyConfigure

	mu  sync.RWMutex
	xl  *xlog.Logger
	ctx context.Context

	// penguin add
	maxInRate  *int64 // 每秒最多传输多少字节数据
	maxOutRate *int64
	ctlRunId   string
	userId     uint
	proxyId    uint
}

func (pxy *BaseProxy) GetName() string {
	return pxy.name
}

func (pxy *BaseProxy) Context() context.Context {
	return pxy.ctx
}

func (pxy *BaseProxy) GetUsedPortsNum() int {
	return pxy.usedPortsNum
}

func (pxy *BaseProxy) GetResourceController() *controller.ResourceController {
	return pxy.rc
}

func (pxy *BaseProxy) GetUserInfo() plugin.UserInfo {
	return pxy.userInfo
}

func (pxy *BaseProxy) GetLoginMsg() *msg.Login {
	return pxy.loginMsg
}

func (pxy *BaseProxy) GetLimiter() *rate.Limiter {
	return pxy.limiter
}

func (pxy *BaseProxy) GetConfigure() v1.ProxyConfigure {
	return pxy.configure
}

// InitTimer 设置proxy速率限制计时器
func (pxy *BaseProxy) InitTimer() {
	// 整个隧道的共享最大速率限制
	maxInRate := *pxy.maxInRate
	maxOutRate := *pxy.maxOutRate
	go func() {
		for {
			*pxy.maxInRate = maxInRate
			*pxy.maxOutRate = maxOutRate
			select {
			case <-time.After(1 * time.Second):
			case <-pxy.ctx.Done():
				return
			}
		}
	}()

	go func() {
		var oldTrafficIn, oldTrafficOut, lastMinuteTrafficIn, lastMinuteTrafficOut, maxRateIn, maxRateOut int64
		for {
			var proxyTrafficInfo *mem.ProxyTrafficInfo
			var currentIn, currentOut int64

			for i := 0; i < 60; i++ {
				select {
				case <-time.After(1 * time.Second):
				case <-pxy.ctx.Done():
					return
				}
				proxyTrafficInfo = mem.StatsCollector.GetProxyTraffic(pxy.name)
				if proxyTrafficInfo == nil {
					continue
				}

				currentIn = proxyTrafficInfo.TrafficIn[0]
				currentOut = proxyTrafficInfo.TrafficOut[0]

				inDiff := currentIn - oldTrafficIn
				outDiff := currentOut - oldTrafficOut

				// 隔天流量刷新，需要全部重置
				if inDiff < 0 || outDiff < 0 {
					i = -1
					oldTrafficIn = 0
					oldTrafficOut = 0
					lastMinuteTrafficIn = 0
					lastMinuteTrafficOut = 0
					maxRateIn = 0
					maxRateOut = 0
					continue
				}

				if maxRateIn < inDiff {
					maxRateIn = inDiff
				}

				if maxRateOut < outDiff {
					maxRateOut = outDiff
				}

				oldTrafficIn = currentIn
				oldTrafficOut = currentOut
			}

			// 平均速率 BPS
			avgRateIn := (currentIn - lastMinuteTrafficIn) / 60
			avgRateOut := (currentOut - lastMinuteTrafficOut) / 60

			// 一分钟用了多少流量 B
			totalTrafficIn := currentIn - lastMinuteTrafficIn
			totalTrafficOut := currentOut - lastMinuteTrafficOut
			lastMinuteTrafficIn = currentIn
			lastMinuteTrafficOut = currentOut

			if avgRateIn <= 0 && avgRateOut <= 0 && maxRateIn <= 0 && maxRateOut <= 0 {
				continue
			}
			history := mysql.ProxyTrafficHistory{
				ProxyID:         pxy.proxyId,
				AvgRateIn:       avgRateIn,
				AvgRateOut:      avgRateOut,
				MaxRateIn:       maxRateIn,
				MaxRateOut:      maxRateOut,
				TotalTrafficIn:  totalTrafficIn,
				TotalTrafficOut: totalTrafficOut,
			}
			maxRateIn = 0
			maxRateOut = 0
			db := pxy.serverCfg.MysqlDBConnect
			db.Create(&history)
		}
	}()

}

func (pxy *BaseProxy) Close() {
	xl := xlog.FromContextSafe(pxy.ctx)
	xl.Infof("proxy closing")
	for _, l := range pxy.listeners {
		_ = l.Close()
	}
}

// GetWorkConnFromPool try to get a new work connections from pool
// for quickly response, we immediately send the StartWorkConn message to frpc after take out one from pool
func (pxy *BaseProxy) GetWorkConnFromPool(src, dst net.Addr) (workConn net.Conn, err error) {
	xl := xlog.FromContextSafe(pxy.ctx)
	// try all connections from the pool
	for i := 0; i < pxy.poolCount+1; i++ {
		if workConn, err = pxy.getWorkConnFn(); err != nil {
			xl.Warnf("failed to get work connection: %v", err)
			return
		}
		xl.Debugf("get a new work connection: [%s]", workConn.RemoteAddr().String())
		xl.Spawn().AppendPrefix(pxy.GetName())
		workConn = netpkg.NewContextConn(pxy.ctx, workConn)

		var (
			srcAddr    string
			dstAddr    string
			srcPortStr string
			dstPortStr string
			srcPort    int
			dstPort    int
		)

		if src != nil {
			srcAddr, srcPortStr, _ = net.SplitHostPort(src.String())
			srcPort, _ = strconv.Atoi(srcPortStr)
		}
		if dst != nil {
			dstAddr, dstPortStr, _ = net.SplitHostPort(dst.String())
			dstPort, _ = strconv.Atoi(dstPortStr)
		}
		err := msg.WriteMsg(workConn, &msg.StartWorkConn{
			ProxyName: pxy.GetName(),
			SrcAddr:   srcAddr,
			SrcPort:   uint16(srcPort),
			DstAddr:   dstAddr,
			DstPort:   uint16(dstPort),
			Error:     "",
		})
		if err != nil {
			xl.Warnf("failed to send message to work connection from pool: %v, times: %d", err, i)
			_ = workConn.Close()
		} else {
			break
		}
	}
	return
}

// startCommonTCPListenersHandler start a goroutine handler for each listener.
func (pxy *BaseProxy) startCommonTCPListenersHandler() {
	xl := xlog.FromContextSafe(pxy.ctx)
	for _, listener := range pxy.listeners {
		go func(l net.Listener) {
			var tempDelay time.Duration // how long to sleep on accept failure

			for {
				// block
				// if listener is closed, err returned
				c, err := l.Accept()
				if err != nil {
					if err, ok := err.(interface{ Temporary() bool }); ok && err.Temporary() {
						if tempDelay == 0 {
							tempDelay = 5 * time.Millisecond
						} else {
							tempDelay *= 2
						}
						if maxD := 1 * time.Second; tempDelay > maxD {
							tempDelay = maxD
						}
						xl.Infof("met temporary error: %s, sleep for %s ...", err, tempDelay)
						time.Sleep(tempDelay)
						continue
					}

					xl.Warnf("listener is closed: %s", err)
					return
				}
				xl.Infof("get a user connection [%s]", c.RemoteAddr().String())
				go pxy.handleUserTCPConnection(c)
			}
		}(listener)
	}
}

// HandleUserTCPConnection is used for incoming user TCP connections.
func (pxy *BaseProxy) handleUserTCPConnection(userConn net.Conn) {
	xl := xlog.FromContextSafe(pxy.Context())
	defer func() {
		_ = userConn.Close()
	}()

	serverCfg := pxy.serverCfg
	cfg := pxy.configure.GetBaseConfig()
	// server plugin hook
	rc := pxy.GetResourceController()
	content := &plugin.NewUserConnContent{
		User:       pxy.GetUserInfo(),
		ProxyName:  pxy.GetName(),
		ProxyType:  cfg.Type,
		RemoteAddr: userConn.RemoteAddr().String(),
	}
	_, err := rc.PluginManager.NewUserConn(content)
	if err != nil {
		xl.Warnf("the user conn [%s] was rejected, err:%v", content.RemoteAddr, err)
		return
	}

	// try all connections from the pool
	workConn, err := pxy.GetWorkConnFromPool(userConn.RemoteAddr(), userConn.LocalAddr())
	if err != nil {
		return
	}
	defer func() {
		_ = workConn.Close()
	}()

	var local io.ReadWriteCloser = workConn
	xl.Tracef("handler user tcp connection, use_encryption: %t, use_compression: %t",
		cfg.Transport.UseEncryption, cfg.Transport.UseCompression)
	if cfg.Transport.UseEncryption {
		local, err = libio.WithEncryption(local, []byte(serverCfg.Auth.Token))
		if err != nil {
			xl.Errorf("create encryption stream error: %v", err)
			return
		}
	}
	if cfg.Transport.UseCompression {
		var recycleFn func()
		local, recycleFn = libio.WithCompressionFromPool(local)
		defer recycleFn()
	}

	if pxy.GetLimiter() != nil {
		local = libio.WrapReadWriteCloser(limit.NewReader(local, pxy.GetLimiter()), limit.NewWriter(local, pxy.GetLimiter()), func() error {
			return local.Close()
		})
	}

	xl.Debugf("join connections, workConn(l[%s] r[%s]) userConn(l[%s] r[%s])", workConn.LocalAddr().String(),
		workConn.RemoteAddr().String(), userConn.LocalAddr().String(), userConn.RemoteAddr().String())

	name := pxy.GetName()
	proxyType := cfg.Type
	metrics.Server.OpenConnection(name, proxyType)

	var inCount, outCount, oldInCount, oldOutCount int64
	var connClose bool
	defer func() {
		connClose = true
	}()
	go func() {
		for {
			metrics.Server.AddTrafficIn(name, proxyType, inCount-oldInCount)
			metrics.Server.AddTrafficOut(name, proxyType, outCount-oldOutCount)
			oldInCount = inCount
			oldOutCount = outCount
			if connClose {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}()

	_ = libio.Join(local, userConn, &inCount, &outCount, pxy.maxInRate, pxy.maxOutRate)
	connClose = true
	metrics.Server.CloseConnection(name, proxyType)
	xl.Debugf("join connections closed")
}

type Options struct {
	UserInfo           plugin.UserInfo
	LoginMsg           *msg.Login
	PoolCount          int
	ResourceController *controller.ResourceController
	GetWorkConnFn      GetWorkConnFn
	Configure          v1.ProxyConfigure
	ServerCfg          *v1.ServerConfig
	MaxInRate          int64
	MaxOutRate         int64
	CtlRunId           string
	UserId             uint
	ProxyId            uint
}

func NewProxy(ctx context.Context, options *Options) (pxy Proxy, err error) {
	configure := options.Configure
	xl := xlog.FromContextSafe(ctx).Spawn().AppendPrefix(configure.GetBaseConfig().Name)

	var limiter *rate.Limiter
	limitBytes := configure.GetBaseConfig().Transport.BandwidthLimit.Bytes()
	if limitBytes > 0 && configure.GetBaseConfig().Transport.BandwidthLimitMode == types.BandwidthLimitModeServer {
		limiter = rate.NewLimiter(rate.Limit(limitBytes), int(limitBytes))
	}

	basePxy := BaseProxy{
		name:          configure.GetBaseConfig().Name,
		rc:            options.ResourceController,
		listeners:     make([]net.Listener, 0),
		poolCount:     options.PoolCount,
		getWorkConnFn: options.GetWorkConnFn,
		serverCfg:     options.ServerCfg,
		limiter:       limiter,
		xl:            xl,
		ctx:           xlog.NewContext(ctx, xl),
		userInfo:      options.UserInfo,
		loginMsg:      options.LoginMsg,
		configure:     configure,
		maxInRate:     &options.MaxInRate,
		maxOutRate:    &options.MaxOutRate,
		ctlRunId:      options.CtlRunId,
		userId:        options.UserId,
		proxyId:       options.ProxyId,
	}

	factory := proxyFactoryRegistry[reflect.TypeOf(configure)]
	if factory == nil {
		return pxy, fmt.Errorf("proxy type not support")
	}
	pxy = factory(&basePxy)
	if pxy == nil {
		return nil, fmt.Errorf("proxy not created")
	}
	return pxy, nil
}

type Manager struct {
	// proxies indexed by proxy name
	pxys map[string]Proxy

	mu sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{
		pxys: make(map[string]Proxy),
	}
}

func (pm *Manager) Add(name string, pxy Proxy) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, ok := pm.pxys[name]; ok {
		return fmt.Errorf("proxy name [%s] is already in use", name)
	}

	pm.pxys[name] = pxy
	return nil
}

func (pm *Manager) Exist(name string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	_, ok := pm.pxys[name]
	return ok
}

func (pm *Manager) Del(name string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.pxys, name)
}

func (pm *Manager) GetByName(name string) (pxy Proxy, ok bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	pxy, ok = pm.pxys[name]
	return
}
