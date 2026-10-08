package server

import (
	"encoding/json"
	"net"
	"time"

	"sudogate/internal/proto"
)

type CtlRequest struct {
	Op       string `json:"op"`
	ID       string `json:"id,omitempty"`
	Password string `json:"password,omitempty"`
	Host     string `json:"host,omitempty"`
}

type CtlReply struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Pending  []EntryInfo    `json:"pending,omitempty"`
	Forwards []ForwardState `json:"forwards,omitempty"`
}

func CtlPath(dataPath string) string {
	return dataPath + ".ctl"
}

func (s *Server) handleCtl(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req CtlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	reply := &CtlReply{OK: true}
	switch req.Op {
	case "list":
		reply.Pending = s.ListPending()
	case "approve":
		if err := s.Approve(req.ID, req.Password); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
	case "deny":
		if err := s.Deny(req.ID); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
	case "forward-list":
		if s.fwd == nil {
			reply.OK = false
			reply.Error = "forward manager disabled"
		} else {
			reply.Forwards = s.fwd.List()
		}
	case "forward-add":
		if s.fwd == nil {
			reply.OK = false
			reply.Error = "forward manager disabled"
		} else if err := s.fwd.Add(req.Host); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
	case "forward-remove":
		if s.fwd == nil {
			reply.OK = false
			reply.Error = "forward manager disabled"
		} else if err := s.fwd.Remove(req.Host); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
	default:
		reply.OK = false
		reply.Error = "unknown op"
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	proto.WriteFrame(conn, reply)
}

func CtlCall(ctlPath string, req *CtlRequest) (*CtlReply, error) {
	conn, err := net.DialTimeout("unix", ctlPath, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := proto.WriteFrame(conn, req); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var reply CtlReply
	if err := proto.ReadFrame(conn, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
