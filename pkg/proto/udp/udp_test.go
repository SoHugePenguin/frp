package udp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUdpPacket(t *testing.T) {
	assert := assert.New(t)
	buf := []byte("你好，企鹅")
	udpMsg := NewUDPPacket(buf, nil, nil)
	newBuf := GetContent(udpMsg)
	assert.EqualValues(buf, newBuf)
}
