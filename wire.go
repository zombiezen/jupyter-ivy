package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zeromq/goczmq"
	"golang.org/x/exp/slices"
)

// Reference: https://jupyter-client.readthedocs.io/en/latest/messaging.html
// Reference Implementation: https://github.com/dsblank/simple_kernel/blob/master/simple_kernel.py

const delim = "<IDS|MSG>"

const protocolVersion = "5.0"

type Authentication struct {
	SignatureScheme string
	Key             []byte
}

type WireMessage struct {
	Identities      [][]byte
	RawHeader       json.RawMessage
	RawParentHeader json.RawMessage
	Metadata        json.RawMessage
	Content         json.RawMessage
	Buffers         [][]byte
}

func NewWireMessage(typ, session string, content any) (*WireMessage, error) {
	hdr, err := NewMessageHeader(typ, session)
	if err != nil {
		return nil, fmt.Errorf("create jupyter wire message: %v", err)
	}
	msg := &WireMessage{
		RawParentHeader: json.RawMessage("{}"),
		Metadata:        json.RawMessage("{}"),
	}
	if err := msg.SetHeader(hdr); err != nil {
		return nil, fmt.Errorf("create jupyter wire message: %v", err)
	}
	if content == nil {
		msg.Content = json.RawMessage("{}")
	} else {
		var err error
		msg.Content, err = json.Marshal(content)
		if err != nil {
			return nil, fmt.Errorf("create jupyter wire message: marshaling content: %v", err)
		}
	}
	return msg, nil
}

func (msg *WireMessage) Marshal(auth Authentication) ([][]byte, error) {
	sig, err := msg.Sign(auth)
	if err != nil {
		return nil, fmt.Errorf("marshal jupyter wire message: %v", err)
	}
	sigHex := make([]byte, hex.EncodedLen(len(sig)))
	hex.Encode(sigHex, sig)

	result := make([][]byte, 0, len(msg.Identities)+6+len(msg.Buffers))
	result = append(result, msg.Identities...)
	result = append(result,
		[]byte(delim),
		sigHex,
		msg.RawHeader,
		msg.RawParentHeader,
		msg.Metadata,
		msg.Content,
	)
	result = append(result, msg.Buffers...)
	return result, nil
}

func (msg *WireMessage) Unmarshal(auth Authentication, message [][]byte) error {
	i := slices.IndexFunc(message, func(frame []byte) bool {
		return string(frame) == delim
	})
	if i == -1 {
		return fmt.Errorf("unmarshal jupyter wire message: delimiter not found")
	}
	msg.Identities = message[:i]
	tail := message[i+1:]
	if len(tail) < 5 {
		return fmt.Errorf("unmarshal jupyter wire message: too few frames")
	}
	sig := make([]byte, hex.DecodedLen(len(tail[0])))
	if _, err := hex.Decode(sig, tail[0]); err != nil {
		return fmt.Errorf("unmarshal jupyter wire message: signature: %v", err)
	}
	if err := json.Unmarshal(tail[1], &msg.RawHeader); err != nil {
		return fmt.Errorf("unmarshal jupyter wire message: header: %v", err)
	}
	if err := json.Unmarshal(tail[2], &msg.RawParentHeader); err != nil {
		return fmt.Errorf("unmarshal jupyter wire message: parent header: %v", err)
	}
	if err := json.Unmarshal(tail[3], &msg.Metadata); err != nil {
		return fmt.Errorf("unmarshal jupyter wire message: metadata: %v", err)
	}
	if err := json.Unmarshal(tail[4], &msg.Content); err != nil {
		return fmt.Errorf("unmarshal jupyter wire message: content: %v", err)
	}
	msg.Buffers = tail[5:]

	if want, err := msg.Sign(auth); err != nil {
		return fmt.Errorf("unmarshal jupyter wire message: %v", err)
	} else if !bytes.Equal(want, sig) {
		return fmt.Errorf("unmarshal jupyter wire message: invalid signature")
	}
	return nil
}

func (msg *WireMessage) Sign(auth Authentication) ([]byte, error) {
	if len(auth.Key) == 0 {
		return nil, nil
	}

	var hf func() hash.Hash
	switch auth.SignatureScheme {
	case "hmac-sha256":
		hf = sha256.New
	default:
		return nil, fmt.Errorf("sign jupyter message: unknown signature scheme %q", auth.SignatureScheme)
	}
	h := hmac.New(hf, auth.Key)
	h.Write(msg.RawHeader)
	h.Write(msg.RawParentHeader)
	h.Write(msg.Metadata)
	h.Write(msg.Content)
	for _, extra := range msg.Buffers {
		h.Write(extra)
	}
	return h.Sum(nil), nil
}

func (msg *WireMessage) Header() (*MessageHeader, error) {
	h := new(MessageHeader)
	if err := json.Unmarshal(msg.RawHeader, h); err != nil {
		return nil, fmt.Errorf("unmarshal jupyter message header: %v", err)
	}
	return h, nil
}

func (msg *WireMessage) SetHeader(hdr *MessageHeader) error {
	var err error
	msg.RawHeader, err = json.Marshal(hdr)
	return err
}

func (msg *WireMessage) ParentHeader() (*MessageHeader, error) {
	h := new(MessageHeader)
	if err := json.Unmarshal(msg.RawParentHeader, h); err != nil {
		return nil, fmt.Errorf("unmarshal jupyter message header: %v", err)
	}
	return h, nil
}

type MessageHeader struct {
	ID       string    `json:"msg_id"`
	Session  string    `json:"session"`
	Username string    `json:"username"`
	Date     time.Time `json:"date"`
	Type     string    `json:"msg_type"`
	Version  string    `json:"version"`
}

func NewMessageHeader(typ string, session string) (*MessageHeader, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generate jupyter message ID: %v", err)
	}
	return &MessageHeader{
		ID:       id.String(),
		Session:  session,
		Username: "kernel",
		Date:     time.Now().UTC(),
		Type:     typ,
		Version:  protocolVersion,
	}, nil
}

type ExecuteRequest struct {
	Code            string            `json:"code"`
	Silent          bool              `json:"silent"`
	StoreHistory    bool              `json:"store_history"`
	UserExpressions map[string]string `json:"user_expressions"`
	AllowStdin      bool              `json:"allow_stdin"`
	StopOnError     bool              `json:"stop_on_error"`
}

type StatusResponse struct {
	ExecutionState string `json:"execution_state"`
}

type StreamResponse struct {
	Name string `json:"name"` // stdout or stderr
	Text string `json:"text"`
}

type DisplayData struct {
	Data     map[string]any `json:"data"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type ExecuteResult struct {
	ExecutionCount int `json:"execution_count"`
	DisplayData
}

type ExecuteReply struct {
	Status          string            `json:"status"` // ok, error, or aborted
	ExecutionCount  int               `json:"execution_count"`
	UserExpressions map[string]string `json:"user_expressions,omitempty"`
}

type ErrorReply struct {
	ExceptionName  string
	ExceptionValue string
	Traceback      []string
}

func (reply *ErrorReply) MarshalJSON() ([]byte, error) {
	var normalized struct {
		Status         string   `json:"status"`
		ExceptionName  string   `json:"ename"`
		ExceptionValue string   `json:"evalue"`
		Traceback      []string `json:"traceback,omitempty"`
	}
	normalized.Status = "error"
	normalized.ExceptionName = reply.ExceptionName
	normalized.ExceptionValue = reply.ExceptionValue
	normalized.Traceback = reply.Traceback
	if len(normalized.Traceback) == 0 {
		normalized.Traceback = nil
	}
	return json.Marshal(normalized)
}

type concurrent0MQSocket struct {
	poll *goczmq.Poller

	mu     sync.Mutex
	socket *goczmq.Sock
}

func wrapSocket(socket *goczmq.Sock) *concurrent0MQSocket {
	poll, err := goczmq.NewPoller(socket)
	if err != nil {
		panic(err)
	}
	return &concurrent0MQSocket{
		socket: socket,
		poll:   poll,
	}
}

func (s *concurrent0MQSocket) RecvMessage(ctx context.Context) ([][]byte, error) {
	s.mu.Lock()
	b, err := s.socket.RecvMessageNoWait()
	s.mu.Unlock()
	if err == nil || !errors.Is(err, goczmq.ErrRecvMessage) {
		return b, err
	}

	const maxWait = 2 * time.Second
	for waitTime := 1 * time.Millisecond; ; {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("receive message: %w", ctx.Err())
		}
		// TODO(now): Does this race? Not holding mutex.
		if s.poll.Wait(int(waitTime/time.Millisecond)) != nil {
			break
		}
		waitTime *= 2
		if waitTime > maxWait {
			waitTime = maxWait
		}
	}

	s.mu.Lock()
	b, err = s.socket.RecvMessage()
	s.mu.Unlock()
	return b, err
}

func (s *concurrent0MQSocket) SendMessage(ctx context.Context, msg [][]byte) error {
	// TODO(someday):
	s.mu.Lock()
	err := s.socket.SendMessage(msg)
	s.mu.Unlock()
	return err
}

func (s *concurrent0MQSocket) Close() error {
	s.mu.Lock()
	s.poll.Destroy()
	s.socket.Destroy()
	s.mu.Unlock()
	return nil
}
