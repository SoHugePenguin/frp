// Copyright 2019 fatedier, fatedier@gmail.com
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
	errors2 "errors"
	"fmt"
	"github.com/SoHugePenguin/frp/pkg/util/log"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/SoHugePenguin/golib/errors"
	libio "github.com/SoHugePenguin/golib/io"

	v1 "github.com/SoHugePenguin/frp/pkg/config/v1"
	"github.com/SoHugePenguin/frp/pkg/msg"
	"github.com/SoHugePenguin/frp/pkg/proto/udp"
	"github.com/SoHugePenguin/frp/pkg/util/limit"
	netpkg "github.com/SoHugePenguin/frp/pkg/util/net"
	"github.com/SoHugePenguin/frp/server/metrics"
)

func init() {
	RegisterProxyFactory(reflect.TypeOf(&v1.UDPProxyConfig{}), NewUDPProxy)
}

type UDPProxy struct {
	*BaseProxy
	cfg *v1.UDPProxyConfig

	realBindPort int

	// udpConn is the listener of udp packages
	udpConn *net.UDPConn

	// there are always only one workConn at the same time
	// get another one if it closed
	workConn net.Conn

	// sendCh is used for sending packages to workConn
	sendCh chan *msg.UDPPacket

	// readCh is used for reading packages from workConn
	readCh chan *msg.UDPPacket

	// checkCloseCh is used for watching if workConn is closed
	checkCloseCh chan int

	isClosed bool
}

func NewUDPProxy(baseProxy *BaseProxy) Proxy {
	unwrapped, ok := baseProxy.GetConfigure().(*v1.UDPProxyConfig)
	if !ok {
		return nil
	}
	baseProxy.usedPortsNum = 1
	return &UDPProxy{
		BaseProxy: baseProxy,
		cfg:       unwrapped,
	}
}

func (pxy *UDPProxy) Run() (remoteAddr string, err error) {
	xl := pxy.xl
	pxy.realBindPort, err = pxy.rc.UDPPortManager.Acquire(pxy.name, pxy.cfg.RemotePort)
	if err != nil {
		return "", fmt.Errorf("acquire port %d error: %v", pxy.cfg.RemotePort, err)
	}
	defer func() {
		if err != nil {
			pxy.rc.UDPPortManager.Release(pxy.realBindPort)
		}
	}()

	remoteAddr = fmt.Sprintf(":%d", pxy.realBindPort)
	pxy.cfg.RemotePort = pxy.realBindPort
	addr, errRet := net.ResolveUDPAddr("udp", net.JoinHostPort(pxy.serverCfg.ProxyBindAddr, strconv.Itoa(pxy.realBindPort)))
	if errRet != nil {
		err = errRet
		return
	}
	udpConn, errRet := net.ListenUDP("udp", addr)
	if errRet != nil {
		err = errRet
		xl.Warnf("listen udp port error: %v", err)
		return
	}
	//xl.Infof("udp proxy listen port [%d]", pxy.cfg.RemotePort)

	pxy.udpConn = udpConn
	pxy.sendCh = make(chan *msg.UDPPacket, 4096)
	pxy.readCh = make(chan *msg.UDPPacket, 4096)
	pxy.checkCloseCh = make(chan int)

	// read message from workConn, if it returns any error, notify proxy to start a new workConn
	workConnReaderFn := func(conn net.Conn) {
		for {
			var (
				rawMsg msg.Message
				errRet error
			)
			xl.Tracef("loop waiting message from udp workConn")
			// client will send heartbeat in workConn for keeping alive
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(60) * time.Second))
			if rawMsg, errRet = msg.ReadMsg(conn); errRet != nil {
				xl.Warnf("read from workConn for udp error: %v", errRet)
				_ = conn.Close()
				// notify proxy to start a new work connection
				// ignore error here, it means the proxy is closed
				_ = errors.PanicToError(func() {
					pxy.checkCloseCh <- 1
				})
				return
			}
			if err := conn.SetReadDeadline(time.Time{}); err != nil {
				xl.Warnf("set read deadline error: %v", err)
			}
			switch m := rawMsg.(type) {
			case *msg.Ping:
				xl.Tracef("udp work conn get ping message")
				continue
			case *msg.UDPPacket:
				if errRet := errors.PanicToError(func() {
					xl.Tracef("get udp message from workConn: %s", m.Content)

					for *pxy.maxOutRate <= 0 {
						time.Sleep(100 * time.Millisecond)
					}

					pxy.readCh <- m
					//xl.Infof("%d , %+v", int64(len(m.Content)), m)

					// 限速+统计速率
					*pxy.maxOutRate -= int64(len(m.Content))
					metrics.Server.AddTrafficOut(
						pxy.GetName(),
						pxy.GetConfigure().GetBaseConfig().Type,
						int64(len(m.Content)),
					)
				}); errRet != nil {
					_ = conn.Close()
					xl.Infof("reader goroutine for udp work connection closed")
					return
				}
			}
		}
	}

	// send message to workConn
	workConnSenderFn := func(conn net.Conn, ctx context.Context) {
		var errRet error
		for {
			select {
			case udpMsg, ok := <-pxy.sendCh:
				if !ok {
					xl.Infof("sender goroutine for udp work connection closed")
					return
				}

				for *pxy.maxInRate <= 0 {
					time.Sleep(100 * time.Millisecond)
				}

				if errRet = msg.WriteMsg(conn, udpMsg); errRet != nil {
					xl.Infof("sender goroutine for udp work connection closed: %v", errRet)
					_ = conn.Close()
					return
				}
				xl.Tracef("send message to udp workConn: %s", udpMsg.Content)

				// 限速+统计速率
				*pxy.maxInRate -= int64(len(udpMsg.Content))
				metrics.Server.AddTrafficIn(
					pxy.GetName(),
					pxy.GetConfigure().GetBaseConfig().Type,
					int64(len(udpMsg.Content)),
				)
				continue
			case <-ctx.Done():
				xl.Infof("sender goroutine for udp work connection closed")
				return
			}
		}
	}

	go func() {
		// Sleep a while for waiting control send the NewProxyResp to client.
		time.Sleep(500 * time.Millisecond)
		for {
			workConn, err := pxy.GetWorkConnFromPool(nil, nil)
			if err != nil {
				time.Sleep(1 * time.Second)
				// check if proxy is closed
				select {
				case _, ok := <-pxy.checkCloseCh:
					if !ok {
						return
					}
				default:
				}
				continue
			}
			// close the old workConn and replace it with a new one
			if pxy.workConn != nil {
				_ = pxy.workConn.Close()
			}

			var rwc io.ReadWriteCloser = workConn
			if pxy.cfg.Transport.UseEncryption {
				rwc, err = libio.WithEncryption(rwc, []byte(pxy.serverCfg.Auth.Token))
				if err != nil {
					xl.Errorf("create encryption stream error: %v", err)
					_ = workConn.Close()
					continue
				}
			}
			if pxy.cfg.Transport.UseCompression {
				rwc = libio.WithCompression(rwc)
			}

			if pxy.GetLimiter() != nil {
				rwc = libio.WrapReadWriteCloser(limit.NewReader(rwc, pxy.GetLimiter()), limit.NewWriter(rwc, pxy.GetLimiter()), func() error {
					return rwc.Close()
				})
			}

			pxy.workConn = netpkg.WrapReadWriteCloserToConn(rwc, workConn)
			ctx, cancel := context.WithCancel(context.Background())
			go workConnReaderFn(pxy.workConn)
			go workConnSenderFn(pxy.workConn, ctx)
			_, ok := <-pxy.checkCloseCh
			cancel()
			if !ok {
				return
			}
		}
	}()

	// Read from user connections and send wrapped udp message to sendCh (forwarded by workConn).
	// Client will transfor udp message to local udp service and waiting for response for a while.
	// Response will be wrapped to be forwarded by work connection to server.
	// Close readCh and sendCh at the end.
	go func() {
		err = udp.ForwardUserConn(udpConn, pxy.readCh, pxy.sendCh, int(pxy.serverCfg.UDPPacketSize))
		var opErr *net.OpError
		if errors2.As(err, &opErr) && strings.Contains(opErr.Error(), "larger") {
			_ = msg.WriteRealtimeMsgByConfig(pxy.serverCfg.RunIdList, pxy.ctlRunId,
				fmt.Sprintf("UDP数据包太长了！需要在 %d 以内", pxy.serverCfg.UDPPacketSize), 403)
			log.Errorf("%s", err.Error())
		}
		pxy.Close()
	}()
	return remoteAddr, nil
}

func (pxy *UDPProxy) Close() {
	pxy.mu.Lock()
	defer pxy.mu.Unlock()
	if !pxy.isClosed {
		pxy.isClosed = true

		pxy.BaseProxy.Close()
		if pxy.workConn != nil {
			_ = pxy.workConn.Close()
		}
		_ = pxy.udpConn.Close()

		// all channels only closed here
		close(pxy.checkCloseCh)
		close(pxy.readCh)
		close(pxy.sendCh)
	}
	pxy.rc.UDPPortManager.Release(pxy.realBindPort)
}
