// Copyright 2018 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package raftforwarder

import (
	"time"

	"github.com/hashicorp/raft"
	"github.com/juju/errors"
	"github.com/juju/pubsub"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/juju/worker.v1/catacomb"

	"github.com/juju/juju/core/raftlease"
)

const applyTimeout = 5 * time.Second

// This worker receives raft commands forwarded over the hub and
// applies them to the raft node.

// RaftApplier allows applying a command to the raft FSM.
type RaftApplier interface {
	Apply(cmd []byte, timeout time.Duration) raft.ApplyFuture
}

// Logger specifies the interface we use from loggo.Logger.
type Logger interface {
	Errorf(string, ...interface{})
	Warningf(string, ...interface{})
	Tracef(string, ...interface{})
}

// Config defines the resources the worker needs to run.
type Config struct {
	Hub    *pubsub.StructuredHub
	Raft   RaftApplier
	Logger Logger
	Topic  string
	Target raftlease.NotifyTarget
}

// Validate checks that this config can be used.
func (config Config) Validate() error {
	if config.Hub == nil {
		return errors.NotValidf("nil Hub")
	}
	if config.Raft == nil {
		return errors.NotValidf("nil Raft")
	}
	if config.Logger == nil {
		return errors.NotValidf("nil Logger")
	}
	if config.Topic == "" {
		return errors.NotValidf("empty Topic")
	}
	if config.Target == nil {
		return errors.NotValidf("nil Target")
	}
	return nil
}

// NewWorker creates and starts a worker that will forward leadership
// claims from non-raft-leader machines.
func NewWorker(config Config) (worker.Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	w := &forwarder{
		config: config,
	}
	unsubscribe, err := w.config.Hub.Subscribe(w.config.Topic, w.handleRequest)
	if err != nil {
		return nil, errors.Annotatef(err, "subscribing to %q", w.config.Topic)
	}
	w.unsubscribe = unsubscribe
	if err := catacomb.Invoke(catacomb.Plan{
		Site: &w.catacomb,
		Work: w.loop,
	}); err != nil {
		unsubscribe()
		return nil, errors.Trace(err)
	}
	return w, nil
}

type forwarder struct {
	catacomb    catacomb.Catacomb
	config      Config
	unsubscribe func()
	id          int
}

// Kill is part of the worker.Worker interface.
func (w *forwarder) Kill() {
	w.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (w *forwarder) Wait() error {
	return w.catacomb.Wait()
}

func (w *forwarder) loop() error {
	defer w.unsubscribe()
	<-w.catacomb.Dying()
	return w.catacomb.ErrDying()
}

func (w *forwarder) handleRequest(_ string, req raftlease.ForwardRequest, err error) {
	w.id++
	w.config.Logger.Tracef("%d: received %#v, err: %s", w.id, req, err)
	defer w.config.Logger.Tracef("%d: done", w.id)
	if err != nil {
		// This should never happen, so treat it as fatal.
		w.catacomb.Kill(errors.Annotate(err, "requests callback failed"))
		return
	}
	response, err := w.processRequest(req.Command)
	if err != nil {
		w.catacomb.Kill(errors.Annotate(err, "applying command"))
		return
	}
	_, err = w.config.Hub.Publish(req.ResponseTopic, response)
	if err != nil {
		w.catacomb.Kill(errors.Annotate(err, "publishing response"))
		return
	}
}

func (w *forwarder) processRequest(command string) (raftlease.ForwardResponse, error) {
	var empty raftlease.ForwardResponse
	future := w.config.Raft.Apply([]byte(command), applyTimeout)
	w.config.Logger.Tracef("%d: after apply", w.id)
	if err := future.Error(); err != nil {
		return empty, errors.Trace(err)
	}
	respValue := future.Response()
	response, ok := respValue.(raftlease.FSMResponse)
	if !ok {
		return empty, errors.Errorf("expected an FSMResponse, got %T: %#v", respValue, respValue)
	}
	w.config.Logger.Tracef("%d: after fsm response", w.id)
	response.Notify(w.config.Target)
	w.config.Logger.Tracef("%d: after notify", w.id)
	return responseFromError(response.Error()), nil
}

func responseFromError(err error) raftlease.ForwardResponse {
	return raftlease.ForwardResponse{
		Error: raftlease.AsResponseError(err),
	}
}
