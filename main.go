// Copyright 2023 Ross Light
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//		 https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"robpike.io/ivy/config"
	"robpike.io/ivy/exec"
	"robpike.io/ivy/parse"
	ivyrun "robpike.io/ivy/run"
	"robpike.io/ivy/scan"
	"robpike.io/ivy/value"
	"zombiezen.com/go/bass/sigterm"
	"zombiezen.com/go/batchio"
	"zombiezen.com/go/jupyter-ivy/internal/zmq"
	"zombiezen.com/go/log"
)

type kernelConfig struct {
	Transport     string `json:"transport"`
	IP            string `json:"ip"`
	ControlPort   int    `json:"control_port"`
	ShellPort     int    `json:"shell_port"`
	IOPubPort     int    `json:"iopub_port"`
	StdinPort     int    `json:"stdin_port"`
	HeartbeatPort int    `json:"hb_port"`

	SignatureScheme string `json:"signature_scheme"`
	Key             string `json:"key"`
}

func (cfg *kernelConfig) Authentication() Authentication {
	return Authentication{
		SignatureScheme: cfg.SignatureScheme,
		Key:             []byte(cfg.Key),
	}
}

func run(ctx context.Context, cfg *kernelConfig) error {
	sessionID, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	z, err := zmq.New(nil)
	if err != nil {
		return err
	}
	defer z.Close()

	heartbeatSocket, err := z.NewRep(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.HeartbeatPort))
	if err != nil {
		return err
	}
	defer heartbeatSocket.Close()
	iopubSocket, err := z.NewPub(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.IOPubPort))
	if err != nil {
		return err
	}
	defer iopubSocket.Close()
	controlSocket, err := z.NewRouter(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.ControlPort))
	if err != nil {
		return err
	}
	defer controlSocket.Close()
	stdinSocket, err := z.NewRouter(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.StdinPort))
	if err != nil {
		return err
	}
	defer stdinSocket.Close()
	shellSocket, err := z.NewRouter(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.ShellPort))
	if err != nil {
		return err
	}
	defer shellSocket.Close()

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		heartbeatLoop(ctx, heartbeatSocket)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		drainSocket(ctx, iopubSocket)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		drainSocket(ctx, stdinSocket)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleControl(ctx, cfg.Authentication(), controlSocket, cancel)
	}()

	log.Infof(ctx, "Kernel ready (Ivy %s)", ivyVersion())
	h := &handler{
		auth:        cfg.Authentication(),
		sessionID:   sessionID.String(),
		shellSocket: shellSocket,
		iopubSocket: iopubSocket,
		ivyContext:  exec.NewContext(new(config.Config)),
	}
	buf := make([]byte, 1<<20) // 1 MiB
	for {
		rawMessage, err := shellSocket.RecvMessage(ctx, buf)
		if err != nil {
			log.Debugf(ctx, "Shell socket disconnected: %v", err)
			return nil
		}
		log.Debugf(ctx, "Request payload: %s", rawMessage)
		msg := new(WireMessage)
		if err := msg.Unmarshal(cfg.Authentication(), rawMessage); err != nil {
			log.Errorf(ctx, "%v", err)
			continue
		}
		hdr, err := msg.Header()
		if err != nil {
			log.Errorf(ctx, "%v", err)
			continue
		}
		log.Debugf(ctx, "Received %q shell message", hdr.Type)

		switch hdr.Type {
		case "execute_request":
			if err := h.execute(ctx, msg); err != nil {
				log.Errorf(ctx, "Responding to execute request: %v", err)
			}
		case "kernel_info_request":
			if err := h.kernelInfo(ctx, msg); err != nil {
				log.Errorf(ctx, "Responding to kernel info request: %v", err)
			}
		case "is_complete_request":
			err := h.reply(ctx, h.shellSocket, "is_complete_reply", msg.Identities, msg.RawHeader, map[string]any{
				"status": "unknown",
			})
			if err != nil {
				log.Errorf(ctx, "Responding to unhandled %q: %v", hdr.Type, err)
			}
			err = h.reply(ctx, h.iopubSocket, "status", nil, msg.RawHeader, StatusResponse{
				ExecutionState: "idle",
			})
			if err != nil {
				return err
			}
		default:
			log.Warnf(ctx, "Unhandled shell request type %q", hdr.Type)
			base, ok := strings.CutSuffix(hdr.Type, "_request")
			if !ok {
				continue
			}
			err := h.reply(ctx, h.shellSocket, base+"_reply", msg.Identities, msg.RawHeader, &ErrorReply{
				ExceptionName: "NotImplementedError",
			})
			if err != nil {
				log.Errorf(ctx, "Responding to unhandled %q: %v", hdr.Type, err)
			}
			err = h.reply(ctx, h.iopubSocket, "status", nil, msg.RawHeader, StatusResponse{
				ExecutionState: "idle",
			})
			if err != nil {
				return err
			}
		}
	}
}

type handler struct {
	sessionID   string
	auth        Authentication
	shellSocket *zmq.Socket
	iopubSocket *zmq.Socket
	ivyContext  value.Context

	executionCounter int
}

func (h *handler) execute(ctx context.Context, req *WireMessage) error {
	var reqContent ExecuteRequest
	if err := json.Unmarshal(req.Content, &reqContent); err != nil {
		return err
	}

	err := h.reply(ctx, h.iopubSocket, "status", nil, req.RawHeader, StatusResponse{
		ExecutionState: "busy",
	})
	if err != nil {
		return err
	}
	executionCount := h.executionCounter + 1
	if !reqContent.Silent && reqContent.StoreHistory {
		h.executionCounter++
	}
	err = h.reply(ctx, h.iopubSocket, "execute_input", nil, req.RawHeader, map[string]any{
		"code":            reqContent.Code,
		"execution_count": executionCount,
	})
	if err != nil {
		return err
	}

	const outBatchSize = 4096
	const outBatchTime = 250 * time.Millisecond
	ivyInput := reqContent.Code
	if !strings.HasSuffix(ivyInput, "\n") {
		ivyInput += "\n"
	}
	scanner := scan.New(h.ivyContext, " ", strings.NewReader(ivyInput))
	parser := parse.NewParser(" ", scanner, h.ivyContext)

	var stdout, stderr *batchio.Writer
	if reqContent.Silent {
		h.ivyContext.Config().SetOutput(io.Discard)
		h.ivyContext.Config().SetErrOutput(io.Discard)
	} else {
		stdout = batchio.NewWriter(outputStream{
			ctx:          ctx,
			streamName:   "stdout",
			parentHeader: req.RawHeader,
			handler:      h,
		}, outBatchSize, outBatchTime)
		h.ivyContext.Config().SetOutput(stdout)
		stderr = batchio.NewWriter(outputStream{
			ctx:          ctx,
			streamName:   "stderr",
			parentHeader: req.RawHeader,
			handler:      h,
		}, outBatchSize, outBatchTime)
		h.ivyContext.Config().SetErrOutput(stderr)
	}

	for !ivyrun.Run(parser, h.ivyContext, false) {
	}

	if stdout != nil {
		h.ivyContext.Config().SetOutput(io.Discard)
		h.ivyContext.Config().SetErrOutput(io.Discard)
		stdout.Flush()
		stderr.Flush()
	}

	err = h.reply(ctx, h.iopubSocket, "status", nil, req.RawHeader, StatusResponse{
		ExecutionState: "idle",
	})
	if err != nil {
		return err
	}
	err = h.reply(ctx, h.shellSocket, "execute_reply", req.Identities, req.RawHeader, ExecuteReply{
		Status:         "ok",
		ExecutionCount: executionCount,
	})
	if err != nil {
		return err
	}

	return nil
}

func (h *handler) kernelInfo(ctx context.Context, req *WireMessage) error {
	err := h.reply(ctx, h.shellSocket, "kernel_info_reply", req.Identities, req.RawHeader, map[string]any{
		"protocol_version": protocolVersion,

		"implementation":         "ivy",
		"implementation_version": "0.0.1",

		"language_info": map[string]any{
			"name":           "ivy",
			"version":        ivyVersion(),
			"file_extension": ".ivy",
		},

		"banner": "",
	})
	if err != nil {
		return err
	}
	err = h.reply(ctx, h.iopubSocket, "status", nil, req.RawHeader, StatusResponse{
		ExecutionState: "idle",
	})
	if err != nil {
		return err
	}

	return nil
}

func (h *handler) reply(ctx context.Context, send *zmq.Socket, typ string, identities [][]byte, parentHeader json.RawMessage, content any) error {
	response, err := NewWireMessage(typ, h.sessionID, content)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	response.Identities = identities
	response.RawParentHeader = parentHeader
	responseMessage, err := response.Marshal(h.auth)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	switch send {
	case h.shellSocket:
		log.Debugf(ctx, "Replying with %q message to shell socket", typ)
	case h.iopubSocket:
		log.Debugf(ctx, "Replying with %q message to I/O publish socket", typ)
	default:
		log.Debugf(ctx, "Replying with %q message to unknown socket", typ)
	}
	log.Debugf(ctx, "Payload: %s", responseMessage)
	if err := send.SendMessage(ctx, responseMessage); err != nil {
		return err
	}
	log.Debugf(ctx, "Sent %q message", typ)
	return nil
}

type outputStream struct {
	ctx          context.Context
	streamName   string
	parentHeader json.RawMessage
	handler      *handler
}

func (s outputStream) Write(p []byte) (n int, err error) {
	err = s.handler.reply(s.ctx, s.handler.iopubSocket, "stream", nil, s.parentHeader, StreamResponse{
		Name: s.streamName,
		Text: string(p),
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func heartbeatLoop(ctx context.Context, sock *zmq.Socket) {
	buf := make([]byte, 4096)
	for {
		n, more, err := sock.Recv(ctx, buf)
		if zmq.IsTruncated(err) {
			log.Errorf(ctx, "Heartbeat: %v", err)
			continue
		}
		if err != nil {
			log.Debugf(ctx, "No longer receiving heartbeats: %v", err)
			return
		}
		if _, err := sock.Send(ctx, buf[:n], more); err != nil {
			log.Debugf(ctx, "Failed to send heartbeat: %v", err)
		}
	}
}

func handleControl(ctx context.Context, auth Authentication, sock *zmq.Socket, cancel context.CancelFunc) {
	buf := make([]byte, 4096)
	for {
		rawMessage, err := sock.RecvMessage(ctx, buf)
		if err != nil {
			log.Errorf(ctx, "Control socket disconnected: %v", err)
			return
		}
		msg := new(WireMessage)
		if err := msg.Unmarshal(auth, rawMessage); err != nil {
			log.Errorf(ctx, "Control message: %v", err)
			continue
		}
		hdr, err := msg.Header()
		if err != nil {
			log.Errorf(ctx, "Control message: %v", err)
			continue
		}
		log.Debugf(ctx, "Received %q control message", hdr.Type)
		if hdr.Type == "shutdown_request" {
			cancel()
			return
		}
	}
}

func drainSocket(ctx context.Context, sock *zmq.Socket) {
	for {
		if _, _, err := sock.Recv(ctx, nil); err != nil && !zmq.IsTruncated(err) {
			return
		}
	}
}

func main() {
	c := &cobra.Command{
		Use:                   "jupyter-ivy [flags] [CONFIG]",
		DisableFlagsInUseLine: true,
		Short:                 "Jupyter kernel for Ivy",
		SilenceErrors:         true,
		SilenceUsage:          true,
		Args:                  cobra.MaximumNArgs(1),
	}

	showDebug := c.PersistentFlags().Bool("debug", false, "show debugging output")
	c.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		initLogging(*showDebug)
		return nil
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		cfg := &kernelConfig{
			Transport:       "tcp",
			IP:              "127.0.0.1",
			SignatureScheme: "hmac-sha256",
			Key:             "deadbeef",
		}
		if len(args) > 0 {
			cfgJSON, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			*cfg = kernelConfig{}
			if err := json.Unmarshal(cfgJSON, cfg); err != nil {
				return err
			}
		}
		return run(cmd.Context(), cfg)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), sigterm.Signals()...)
	err := c.ExecuteContext(ctx)
	cancel()
	if err != nil {
		initLogging(*showDebug)
		log.Errorf(ctx, "%v", err)
		os.Exit(1)
	}
}

func ivyVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == "robpike.io/ivy" {
			return dep.Version
		}
	}
	return ""
}

var initLogOnce sync.Once

func initLogging(showDebug bool) {
	initLogOnce.Do(func() {
		minLogLevel := log.Info
		if showDebug {
			minLogLevel = log.Debug
		}
		log.SetDefault(&log.LevelFilter{
			Min:    minLogLevel,
			Output: log.New(os.Stderr, "jupyter-ivy: ", log.StdFlags, nil),
		})
	})
}
