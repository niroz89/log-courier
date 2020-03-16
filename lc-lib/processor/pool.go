/*
 * Copyright 2012-2020 Jason Woods and contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package processor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/driskell/log-courier/lc-lib/config"
	"github.com/driskell/log-courier/lc-lib/core"
	"github.com/driskell/log-courier/lc-lib/event"
)

// Pool manages routines that perform sequences of mutations on events
type Pool struct {
	input        chan []*event.Event
	output       chan<- []*event.Event
	shutdownChan <-chan struct{}
	configChan   <-chan *config.Config

	pipelines *Config
	sequencer *event.Sequencer
	fanout    chan *event.Bundle
	collector chan *event.Bundle
}

// NewPool creates a new processor pool
func NewPool(app *core.App) *Pool {
	return &Pool{
		input:     make(chan []*event.Event, 16), // TODO: Make configurable?
		sequencer: event.NewSequencer(),
		fanout:    make(chan *event.Bundle, 4),
		collector: make(chan *event.Bundle, 4),
	}
}

// Input returns the channel to send events to the processors with
func (p *Pool) Input() chan<- []*event.Event {
	return p.input
}

// SetOutput sets the output channel
func (p *Pool) SetOutput(output chan<- []*event.Event) {
	p.output = output
}

// SetShutdownChan sets the shutdown channel
func (p *Pool) SetShutdownChan(shutdownChan <-chan struct{}) {
	p.shutdownChan = shutdownChan
}

// SetConfigChan sets the config channel
func (p *Pool) SetConfigChan(configChan <-chan *config.Config) {
	p.configChan = configChan
}

// Init does nothing as nothing to initialise
func (p *Pool) Init(cfg *config.Config) error {
	p.pipelines = FetchConfig(cfg)
	return nil
}

// Run starts the processing routines
func (p *Pool) Run() {
	for {
		var newConfig *config.Config
		ctx, shutdownFunc := context.WithCancel(context.Background())
		shutdown := false
		routineCount := 4

		for i := 0; i < routineCount; i++ {
			go p.processorRoutine(ctx)
		}

	PipelineLoop:
		for {
			select {
			case <-p.shutdownChan:
				shutdown = true
			case newConfig = <-p.configChan:
				// Request shutdown so we can restart with new configuration
				shutdownFunc()
			case events := <-p.input:
				// Closed input means shutting down gracefully
				if events == nil {
					shutdown = true
					close(p.fanout)
					continue
				}
				bundle := event.NewBundle(events)
				p.sequencer.Track(bundle)
				select {
				case <-p.shutdownChan:
				case p.fanout <- bundle:
				}
			case bundle := <-p.collector:
				if bundle == nil {
					// A routine shutdown
					routineCount--
				}
				if routineCount == 0 {
					// All routines complete
					break PipelineLoop
				}
				result := p.sequencer.Enforce(bundle)
				for _, bundle := range result {
					select {
					case <-p.shutdownChan:
						shutdown = true
					case p.output <- bundle.Events():
					}
				}
			}
		}

		if shutdown {
			shutdownFunc()
			break
		}

		p.pipelines = FetchConfig(newConfig)
	}

	close(p.output)
	log.Info("Processor exiting")
}

// processorRoutine runs a single routine for processing
func (p *Pool) processorRoutine(ctx context.Context) *config.Config {
	for {
		select {
		case <-p.shutdownChan:
			return nil
		case <-ctx.Done():
			return nil
		case bundle := <-p.fanout:
			if bundle == nil {
				p.collector <- nil
				return nil
			}

			start := time.Now()
			events := bundle.Events()
			for idx, event := range events {
				events[idx] = p.processEvent(event)
			}
			log.Debugf("Processed %d events in %v", bundle.Len(), time.Since(start))

			select {
			case <-p.shutdownChan:
				return nil
			case p.collector <- bundle:
			}
		}
	}
}

// processEvent processes a single event
func (p *Pool) processEvent(evnt *event.Event) *event.Event {
	for _, entry := range p.pipelines.AST {
		evnt = entry.Process(evnt)
	}
	evnt.ClearCache()
	eventJSON, _ := json.Marshal(evnt.Data())
	log.Debugf("Final event: %s", eventJSON)
	return evnt
}
