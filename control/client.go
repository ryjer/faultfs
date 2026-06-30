package control

import (
	"encoding/json"
	"net"
)

// Send 拨号 socket，发送一条 [Req] 并读取一条 [Resp]（一请求一连接）。
func Send(socket string, req Req) (Resp, error) {
	var resp Resp
	c, err := net.Dial("unix", socket)
	if err != nil {
		return resp, err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return resp, err
	}
	return resp, nil
}
