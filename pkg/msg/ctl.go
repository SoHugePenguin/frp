// Copyright 2018 fatedier, fatedier@gmail.com
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

package msg

import (
	"github.com/SoHugePenguin/frp/pkg/util/log"
	"github.com/SoHugePenguin/frp/pkg/util/version"
	"io"
	"net"

	jsonMsg "github.com/SoHugePenguin/golib/msg/json"
)

type Message = jsonMsg.Message

var msgCtl *jsonMsg.MsgCtl

func init() {
	msgCtl = jsonMsg.NewMsgCtl()
	for typeByte, msg := range msgTypeMap {
		msgCtl.RegisterMsg(typeByte, msg)
	}
}

// SetMaxMsgLength 设置一次连接(比如udp包)的最大消息内容长度，在goLib中，默认值defaultMaxMsgLength = 10240
func SetMaxMsgLength(length int64) {
	msgCtl.SetMaxMsgLength(length)
}

func ReadMsg(c io.Reader) (msg Message, err error) {
	return msgCtl.ReadMsg(c)
}

func ReadMsgInto(c io.Reader, msg Message) (err error) {
	return msgCtl.ReadMsgInto(c, msg)
}

func WriteMsg(c io.Writer, msg interface{}) (err error) {
	return msgCtl.WriteMsg(c, msg)
}

type ConnMsg struct {
	Token string
	Conn  net.Conn
}

// WriteRealtimeMsgByConfig 输入无效时不做任何处理
func WriteRealtimeMsgByConfig(connMap map[string]*ConnMsg, runID string, text string, code int) error {
	// 避免程序报错崩溃
	defer func() {
		if r := recover(); r != nil {
			log.Debugf(r.(error).Error())
		}
	}()

	err := WriteMsg(connMap[runID].Conn, &RealTimeMsg{
		Version: version.Full(),
		Text:    text,
		Code:    code,
	})
	return err
}

func WriteRealtimeMsg(c io.Writer, text string, code int) {
	_ = WriteMsg(c, &RealTimeMsg{
		Version: version.Full(),
		Text:    text,
		Code:    code,
	})
}
