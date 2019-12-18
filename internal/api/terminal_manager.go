/*
Copyright (c) 2019 the Octant contributors. All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/

package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/vmware-tanzu/octant/internal/config"
	"github.com/vmware-tanzu/octant/internal/octant"
	"github.com/vmware-tanzu/octant/internal/terminal"
	"github.com/vmware-tanzu/octant/pkg/action"
)

const (
	readBufferSize            = 4096
	RequestTerminalScrollback = "sendTerminalScrollback"
	RequestTerminalCommand    = "sendTerminalCommand"
	RequestTerminalResize     = "sendTerminalResize"
)

type terminalStateManager struct {
	config config.Dash
	poller Poller

	sendScrollback sync.Map
}

type terminalOutput struct {
	Scrollback  []byte `json:"scrollback,omitempty"`
	Line        []byte `json:"line,omitempty"`
	ExitMessage []byte `json:"exitMessage,omitempty"`
}

var _ StateManager = (*terminalStateManager)(nil)

// NewTerminalStateManager returns a terminal state manager.
func NewTerminalStateManager(cfg config.Dash) StateManager {
	return &terminalStateManager{
		config: cfg,
		poller: NewInterruptiblePoller("terminal"),
	}
}

// Handlers returns a slice of handlers.
func (s *terminalStateManager) Handlers() []octant.ClientRequestHandler {
	return []octant.ClientRequestHandler{
		{
			RequestType: RequestTerminalScrollback,
			Handler:     s.SendTerminalScrollback,
		},
		{
			RequestType: RequestTerminalCommand,
			Handler:     s.SendTerminalCommand,
		},
		{
			RequestType: RequestTerminalResize,
			Handler:     s.SendTerminalResize,
		},
	}
}

func (s *terminalStateManager) SendTerminalResize(state octant.State, payload action.Payload) error {
	terminalID, err := payload.String("terminalID")
	if err != nil {
		return errors.Wrap(err, "extract terminal ID from payload")
	}

	rows, err := payload.Uint16("rows")
	if err != nil {
		return errors.Wrap(err, "extract rows from payload")
	}

	cols, err := payload.Uint16("cols")
	if err != nil {
		return errors.Wrap(err, "extract cols from payload")
	}

	tm := s.config.TerminalManager()
	t, ok := tm.Get(terminalID)
	if !ok {
		return errors.New(fmt.Sprintf("terminal %s not found", terminalID))
	}

	if t.Active() {
		t.Resize(cols, rows)
	}
	return nil
}

func (s *terminalStateManager) SendTerminalCommand(state octant.State, payload action.Payload) error {
	terminalID, err := payload.String("terminalID")
	if err != nil {
		return errors.Wrap(err, "extract terminal ID from payload")
	}

	key, err := payload.String("key")
	if err != nil {
		return errors.Wrap(err, "extract key from payload")
	}

	tm := s.config.TerminalManager()
	t, ok := tm.Get(terminalID)
	if !ok {
		return errors.New(fmt.Sprintf("terminal %s not found", terminalID))
	}
	return t.Write([]byte(key))
}

func (s *terminalStateManager) SendTerminalScrollback(state octant.State, payload action.Payload) error {
	terminalID, err := payload.String("terminalID")
	if err != nil {
		return errors.Wrap(err, "extract terminal ID from payload")
	}
	s.setSendScrollback(terminalID, true)
	return nil
}

func (s *terminalStateManager) setSendScrollback(id string, v bool) {
	s.sendScrollback.Store(id, v)
	s.config.TerminalManager().ForceUpdate(id)
}

func (s *terminalStateManager) Start(ctx context.Context, state octant.State, client OctantClient) {
	ch := make(chan struct{}, 1)
	defer func() {
		close(ch)
	}()

	go func() {
		tm := s.config.TerminalManager()
	outer:
		for {
			select {
			case <-ctx.Done():
				break outer
			case t := <-tm.Select(ctx):
				event, err := s.newEvent(t)
				if err != nil {
					s.config.Logger().Debugf("newEvent: %s", err)
					break
				}
				client.Send(event)
			case <-time.After(3 * time.Millisecond):
				break
			}
		}
	}()
}

func (s *terminalStateManager) newEvent(t terminal.Instance) (octant.Event, error) {
	line, err := t.Read(readBufferSize)
	if err != nil {
		t.SetExitMessage(fmt.Sprintf("%v\n", err))
		t.Stop()
		return octant.Event{}, errors.Wrap(err, "read error")
	}

	sendScrollback, ok := s.sendScrollback.Load(t.ID())
	if line == nil && (!ok || !sendScrollback.(bool)) {
		return octant.Event{}, errors.New("no scrollback or line")
	}

	key := t.Key()
	eventType := octant.EventType(fmt.Sprintf("terminals/namespace/%s/pod/%s/container/%s/%s", key.Namespace, key.Name, t.Container(), t.ID()))
	data := terminalOutput{Line: line}

	if ok && sendScrollback.(bool) {
		data.Scrollback = t.Scrollback()
		msg := t.ExitMessage()
		if msg != "" {
			data.Scrollback = append(data.Scrollback, []byte("\n"+msg)...)
		}
		s.setSendScrollback(t.ID(), false)
	}

	return octant.Event{
		Type: eventType,
		Data: data,
		Err:  nil,
	}, nil
}