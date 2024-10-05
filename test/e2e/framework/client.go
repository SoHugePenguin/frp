package framework

import (
	clientsdk "github.com/SoHugePenguin/frp/pkg/sdk/client"
)

func (f *Framework) APIClientForFrpc(port int) *clientsdk.Client {
	return clientsdk.New("127.0.0.1", port)
}
