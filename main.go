package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/zeromq/goczmq"
	"robpike.io/ivy/config"
	"robpike.io/ivy/exec"
	ivyrun "robpike.io/ivy/run"
	"robpike.io/ivy/value"
	"zombiezen.com/go/bass/sigterm"
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

	heartbeatSocket := goczmq.NewRepChanneler(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.HeartbeatPort))
	defer heartbeatSocket.Destroy()
	iopubSocket := goczmq.NewPubChanneler(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.IOPubPort))
	defer iopubSocket.Destroy()
	controlSocket := goczmq.NewRouterChanneler(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.ControlPort))
	defer controlSocket.Destroy()
	stdinSocket := goczmq.NewRouterChanneler(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.StdinPort))
	defer stdinSocket.Destroy()
	shellSocket := goczmq.NewRouterChanneler(fmt.Sprintf("%s://%s:%d", cfg.Transport, cfg.IP, cfg.ShellPort))
	defer shellSocket.Destroy()

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
		drainSocket(ctx, iopubSocket.RecvChan)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		drainSocket(ctx, stdinSocket.RecvChan)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleControl(ctx, cfg.Authentication(), controlSocket, cancel)
	}()

	log.Infof(ctx, "Kernel ready (Ivy %s)", ivyVersion())
	h := &handler{
		auth:            cfg.Authentication(),
		sessionID:       sessionID.String(),
		shellSocketSend: shellSocket.SendChan,
		iopubSocketSend: iopubSocket.SendChan,
		ivyContext:      exec.NewContext(new(config.Config)),
	}
	for {
		var rawMessage [][]byte
		select {
		case rawMessage = <-shellSocket.RecvChan:
		case <-ctx.Done():
			return nil
		}
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
		case "history_request":
			log.Debugf(ctx, "Unhandled shell history request")
		default:
			log.Warnf(ctx, "Unhandled shell request type %q", hdr.Type)
		}
	}
}

type handler struct {
	sessionID       string
	auth            Authentication
	shellSocketSend chan<- [][]byte
	iopubSocketSend chan<- [][]byte
	ivyContext      value.Context

	executionCounter int
}

func (h *handler) execute(ctx context.Context, req *WireMessage) error {
	var reqContent ExecuteRequest
	if err := json.Unmarshal(req.Content, &reqContent); err != nil {
		return err
	}

	err := h.reply(ctx, h.iopubSocketSend, "status", nil, req.RawHeader, StatusResponse{
		ExecutionState: "busy",
	})
	if err != nil {
		return err
	}
	executionCount := h.executionCounter + 1
	if reqContent.StoreHistory {
		h.executionCounter++
	}
	err = h.reply(ctx, h.iopubSocketSend, "execute_input", nil, req.RawHeader, map[string]any{
		"code":            reqContent.Code,
		"execution_count": executionCount,
	})
	if err != nil {
		return err
	}

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	ivyrun.Ivy(h.ivyContext, reqContent.Code, stdout, stderr)

	if stderr.Len() > 0 {
		err := h.reply(ctx, h.iopubSocketSend, "stream", nil, req.RawHeader, StreamResponse{
			Name: "stderr",
			Text: stderr.String(),
		})
		if err != nil {
			return err
		}
	}
	err = h.reply(ctx, h.iopubSocketSend, "execute_result", nil, req.RawHeader, ExecuteResult{
		ExecutionCount: executionCount,
		DisplayData: DisplayData{
			Data: map[string]any{
				"text/plain": stdout.String(),
			},
		},
	})
	if err != nil {
		return err
	}
	err = h.reply(ctx, h.iopubSocketSend, "status", nil, req.RawHeader, StatusResponse{
		ExecutionState: "idle",
	})
	if err != nil {
		return err
	}
	err = h.reply(ctx, h.shellSocketSend, "execute_reply", req.Identities, req.RawHeader, ExecuteReply{
		Status:         "ok",
		ExecutionCount: executionCount,
	})
	if err != nil {
		return err
	}

	return nil
}

func (h *handler) kernelInfo(ctx context.Context, req *WireMessage) error {
	err := h.reply(ctx, h.shellSocketSend, "kernel_info_reply", req.Identities, req.RawHeader, map[string]any{
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
	err = h.reply(ctx, h.iopubSocketSend, "status", nil, req.RawHeader, StatusResponse{
		ExecutionState: "idle",
	})
	if err != nil {
		return err
	}

	return nil
}

func (h *handler) reply(ctx context.Context, send chan<- [][]byte, typ string, identities [][]byte, parentHeader json.RawMessage, content any) error {
	response, err := NewWireMessage(typ, h.sessionID, content)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	response.RawParentHeader = parentHeader
	responseMessage, err := response.Marshal(h.auth)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	switch send {
	case h.shellSocketSend:
		log.Debugf(ctx, "Replying with %q message to shell socket", typ)
	case h.iopubSocketSend:
		log.Debugf(ctx, "Replying with %q message to I/O publish socket", typ)
	default:
		log.Debugf(ctx, "Replying with %q message to unknown socket", typ)
	}
	log.Debugf(ctx, "Payload: %s", responseMessage)
	select {
	case send <- responseMessage:
		log.Debugf(ctx, "Sent %q message", typ)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("send message: %w", err)
	}
}

func heartbeatLoop(ctx context.Context, sock *goczmq.Channeler) {
	defer log.Debugf(ctx, "Heartbeat finished")
	for {
		select {
		case frame, ok := <-sock.RecvChan:
			if !ok {
				return
			}
			log.Debugf(ctx, "Heartbeat")
			select {
			case sock.SendChan <- frame:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func handleControl(ctx context.Context, auth Authentication, sock *goczmq.Channeler, cancel context.CancelFunc) {
	for {
		var rawMessage [][]byte
		var ok bool
		select {
		case rawMessage, ok = <-sock.RecvChan:
			if !ok {
				return
			}
		case <-ctx.Done():
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

func drainSocket(ctx context.Context, sock <-chan [][]byte) {
	for {
		select {
		case _, ok := <-sock:
			if !ok {
				return
			}
		case <-ctx.Done():
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
			Output: log.New(os.Stderr, "jupypter-ivy: ", log.StdFlags, nil),
		})
	})
}
