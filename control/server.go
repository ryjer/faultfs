package control

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
)

// Server 监听一个 unix socket，把每条 JSON [Req] 交给 handler 处理并回写 [Resp]。
// 它不持有任何业务状态——handler 由挂载方提供（通常是一个闭包，把 Req 翻译成对
// *faultfs.Injector 的调用），从而让 control 与 faultfs 无循环依赖。
type Server struct {
	socket  string
	handler func(Req) Resp
	ln      net.Listener
}

// NewServer 创建（尚未监听）一个控制服务。
func NewServer(socket string, handler func(Req) Resp) *Server {
	return &Server{socket: socket, handler: handler}
}

// Addr 返回 socket 路径。
func (s *Server) Addr() string { return s.socket }

// Listen 创建监听器（先确保父目录存在）。调用后客户端即可连接。
func (s *Server) Listen() error {
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		// 残留 socket 文件可能导致 bind 失败，清理后重试一次。
		_ = os.Remove(s.socket)
		ln, err = net.Listen("unix", s.socket)
		if err != nil {
			return err
		}
	}
	s.ln = ln
	return nil
}

// Serve 接受连接直到 [Server.Close]。每条连接：读一个 JSON Req → handler → 写
// 一个 JSON Resp，随后关闭连接（一请求一连接的简单协议，CLI 友好）。
func (s *Server) Serve() {
	if s.ln == nil {
		return
	}
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return // listener 已关闭
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	var req Req
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		_ = json.NewEncoder(c).Encode(Resp{OK: false, Err: "bad request: " + err.Error()})
		return
	}
	resp := s.handler(req)
	_ = json.NewEncoder(c).Encode(resp)
}

// Close 关闭监听器（使 Serve 退出）并移除 socket 文件。
func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	err := s.ln.Close()
	_ = os.Remove(s.socket)
	return err
}
