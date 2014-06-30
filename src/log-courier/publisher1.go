/*
 * Copyright 2014 Jason Woods.
 *
 * This file is a modification of code from Logstash Forwarder.
 * Copyright 2012-2013 Jordan Sissel and contributors.
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

package main

import (
  "bytes"
  "compress/zlib"
  "encoding/binary"
  "encoding/json"
  "errors"
  "fmt"
  "io"
  "log"
  "math/rand"
  "os"
  "time"
)

const (
  default_publisher_hostname string        = "localhost.localdomain"
  keepalive_timeout          time.Duration = 900 * time.Second
  max_pending_payloads       int           = 100
)

type PendingPayload struct {
  next          *PendingPayload
  nonce         string
  events        []*FileEvent
  num_events    int
  ack_events    int
  payload_start int
  payload       []byte
  timeout       *time.Time
}

func NewPendingPayload(events []*FileEvent, nonce string, hostname string) (*PendingPayload, error) {
  payload := &PendingPayload{
    events:     events,
    nonce:      nonce,
    num_events: len(events),
  }

  if err := payload.Generate(hostname); err != nil {
    return nil, err
  }

  return payload, nil
}

func (pp *PendingPayload) Generate(hostname string) (err error) {
  var buffer bytes.Buffer

  // Begin with the nonce
  if _, err = buffer.Write([]byte(pp.nonce)); err != nil {
    return
  }

  var compressor *zlib.Writer
  if compressor, err = zlib.NewWriterLevel(&buffer, 3); err != nil {
    return
  }

  // Append all the events
  for _, event := range pp.events[pp.ack_events:] {
    // Add host field
    event.Event["host"] = hostname
    if err = pp.bufferJdatDataEvent(compressor, event); err != nil {
      return
    }
  }

  compressor.Close()

  pp.payload = buffer.Bytes()
  pp.payload_start = pp.ack_events

  return
}

func (pp *PendingPayload) bufferJdatDataEvent(output io.Writer, event *FileEvent) (err error) {
  var value []byte
  value, err = json.Marshal(event.Event)
  if err != nil {
    log.Printf("JSON event encoding error: %s\n", err)

    if err = binary.Write(output, binary.BigEndian, 2); err != nil {
      return
    }
    if _, err = output.Write([]byte("{}")); err != nil {
      return
    }

    return
  }

  if err = binary.Write(output, binary.BigEndian, uint32(len(value))); err != nil {
    return
  }
  if _, err = output.Write(value); err != nil {
    return
  }

  return nil
}

type Publisher struct {
  shutdown         *LogCourierShutdown
  config           *NetworkConfig
  transport        Transport
  hostname         string
  can_send         <-chan int
  pending_ping     bool
  pending_payloads map[string]*PendingPayload
  first_payload    *PendingPayload
  last_payload     *PendingPayload
  num_payloads     int
  out_of_sync      int
}

func NewPublisher(config *NetworkConfig, shutdown *LogCourierShutdown) *Publisher {
  return &Publisher{
    shutdown: shutdown,
    config: config,
  }
}

func (p *Publisher) Init() error {
  var err error

  p.hostname, err = os.Hostname()
  if err != nil {
    log.Printf("Failed to determine the FQDN; using localhost.localdomain.\n")
    p.hostname = default_publisher_hostname
  }

  // Set up the selected transport (currently only TLS)
  if p.transport, err = p.config.transport.NewTransport(p.config); err != nil {
    return err
  }

  p.pending_payloads = make(map[string]*PendingPayload)

  return nil
}

func (p *Publisher) Publish(input <-chan []*FileEvent, registrar *Registrar) {
  defer func() {
    p.shutdown.Done()
  }()

  var input_toggle <-chan []*FileEvent
  var retry_payload *PendingPayload
  var err error
  var shutdown bool

  // Connect to registrar
  registrar_chan := registrar.Connect()

  // TODO(driskell): Make the idle timeout configurable like the network timeout is?
  timer := time.NewTimer(keepalive_timeout)

  // TODO: We should still obey network timeout if we've sent events and not yet received response
  //       as its the quickest way to detect a connection problem after idle

PublishLoop:
  for {
    if err = p.transport.Connect(); err != nil {
      log.Printf("Connect attempt failed: %s\n", err)
      // TODO: implement shutdown select
      select {
      case <-time.After(p.config.Reconnect):
        continue
      case <-p.shutdown.Signal():
        // TODO: Persist pending payloads and resume? Quicker shutdown
        if p.num_payloads == 0 {
          break PublishLoop
        }

        shutdown = true
      }
    }
    p.can_send = p.transport.CanSend()
    input_toggle = nil

  SelectLoop:
    for {
      // TODO: implement shutdown select
      select {
      case <-p.can_send:
        // Resend payloads from full retry first
        if retry_payload != nil {
          // Do we need to regenerate the payload?
          if retry_payload.payload == nil {
            if err = retry_payload.Generate(p.hostname); err != nil {
              break SelectLoop
            }
          }

          // Reset timeout
          retry_payload.timeout = nil

          // Send the payload again
          if err = p.transport.Write("JDAT", retry_payload.payload); err != nil {
            break SelectLoop
          }

          // Move to next non-empty payload
          for {
            retry_payload = retry_payload.next
            if retry_payload == nil || retry_payload.ack_events != len(retry_payload.events) {
              break
            }
          }

          // Expect an ACK within network timeout
          if p.first_payload.timeout != nil {
            timer.Reset(p.first_payload.timeout.Sub(time.Now()))
          } else {
            timer.Reset(p.config.Timeout)
          }
          break
        } else if p.out_of_sync != 0 {
          var resent bool
          if resent, err = p.checkResend(); err != nil {
            break SelectLoop
          } else if resent {
            // Expect an ACK within network timeout
            timer.Reset(p.config.Timeout)
            break
          }
        }

        // No pending payloads, are we shutting down? Skip if so
        if shutdown {
          break
        }

        // Enable event wait
        input_toggle = input
      case events := <-input_toggle:
        // Send
        if err = p.sendNewPayload(events); err != nil {
          break SelectLoop
        }

        // Wait for send signal again
        input_toggle = nil

        if p.num_payloads >= max_pending_payloads {
          // Too many pending payloads, disable send temporarily
          p.can_send = nil
        }

        // Expect an ACK within network timeout
        if p.first_payload.timeout != nil {
          timer.Reset(p.first_payload.timeout.Sub(time.Now()))
        } else {
          timer.Reset(p.config.Timeout)
        }
      case data := <-p.transport.Read():
        var signature, message []byte

        // Error? Or data?
        switch data.(type) {
        case error:
          err = data.(error)
          break SelectLoop
        default:
          signature = data.([][]byte)[0]
          message = data.([][]byte)[1]
        }

        switch {
        case bytes.Compare(signature, []byte("PONG")) == 0:
          if err = p.processPong(message); err != nil {
            break SelectLoop
          }
        case bytes.Compare(signature, []byte("ACKN")) == 0:
          if err = p.processAck(message, registrar_chan); err != nil {
            break SelectLoop
          }
        default:
          err = errors.New(fmt.Sprintf("Unknown message received: % X", signature))
          break SelectLoop
        }

        // If no more pending payloads, set keepalive, otherwise reset to network timeout
        if p.num_payloads == 0 {
          // Handle shutdown
          if shutdown {
            break PublishLoop
          }
          timer.Reset(keepalive_timeout)
        } else if p.first_payload.timeout != nil {
          timer.Reset(p.first_payload.timeout.Sub(time.Now()))
        } else {
          timer.Reset(p.config.Timeout)
        }
      case <-timer.C:
        // Do we need to resend first payload?
        if p.out_of_sync != 0 {
          var resent bool
          if resent, err = p.checkResend(); err != nil {
            break SelectLoop
          } else if resent {
            // Expect an ACK within network timeout
            timer.Reset(p.config.Timeout)
            break
          }
        }

        // If we have pending payloads, we should've received something by now
        if p.num_payloads != 0 || input_toggle == nil {
          err = errors.New("Server did not respond within network timeout")
          break SelectLoop
        }

        // If we haven't received a PONG yet this is a timeout
        if p.pending_ping {
          err = errors.New("Server did not respond to PING")
          break SelectLoop
        }

        // Send a ping and expect a pong back (eventually)
        // If we receive an ACK first, that's fine we'll reset timer
        // But after those ACKs we should get a PONG
        if err = p.transport.Write("PING", nil); err != nil {
          break SelectLoop
        }

        // We may have just filled the send buffer
        input_toggle = nil

        // Allow network timeout to receive something
        timer.Reset(p.config.Timeout)
      case <-p.shutdown.Signal():
        // If no pending payloads, simply end
        // TODO: Persist pending payloads and resume? Quicker shutdown
        if p.num_payloads == 0 {
          break PublishLoop
        }

        // Flag shutdown for when we finish pending payloads
        shutdown = true
      }
    }

    // Disconnect and retry payloads
    log.Printf("Transport error, will reconnect: %s\n", err)
    p.transport.Disconnect()

    retry_payload = p.first_payload

    time.Sleep(p.config.Reconnect)
  }

  p.transport.Disconnect()

  // Disconnect from registrar
  registrar.Disconnect()

  log.Printf("Publisher shutdown complete\n")
}

func (p *Publisher) checkResend() (bool, error) {
  // We're out of sync (received ACKs for later payloads but not earlier ones)
  // Check timeouts of earlier payloads and resend if necessary
  if payload := p.first_payload; payload.timeout.Before(time.Now()) {
    // Do we need to regenerate the payload?
    if payload.payload == nil {
      if err := payload.Generate(p.hostname); err != nil {
        return false, err
      }
    }

    // Update timeout
    timeout := time.Now().Add(p.config.Timeout)
    payload.timeout = &timeout

    // Send the payload again
    if err := p.transport.Write("JDAT", payload.payload); err != nil {
      return false, err
    }

    return true, nil
  }

  return false, nil
}

func (p *Publisher) generateNonce() string {
  // This could maybe be made a bit more efficient
  nonce := make([]byte, 16)
  for i := 0; i < 16; i++ {
    nonce[i] = byte(rand.Intn(255))
  }
  return string(nonce)
}

func (p *Publisher) sendNewPayload(events []*FileEvent) (err error) {
  // Calculate a nonce
  nonce := p.generateNonce()
  for {
    if _, found := p.pending_payloads[nonce]; !found {
      break
    }
    // Collision - generate again - should be extremely rare
    nonce = p.generateNonce()
  }

  var payload *PendingPayload
  if payload, err = NewPendingPayload(events, nonce, p.hostname); err != nil {
    return
  }

  // Save pending payload until we receive ack, and discard buffer
  p.pending_payloads[nonce] = payload
  if p.first_payload == nil {
    p.first_payload = payload
  } else {
    p.last_payload.next = payload
  }
  p.last_payload = payload
  p.num_payloads++

  return p.transport.Write("JDAT", payload.payload)
}

func (p *Publisher) processPong(message []byte) error {
  if len(message) != 0 {
    return errors.New(fmt.Sprintf("PONG message overflow (%d)", len(message)))
  }

  // Were we pending a ping?
  if !p.pending_ping {
    return errors.New("Unexpected PONG received")
  }

  p.pending_ping = false
  return nil
}

func (p *Publisher) processAck(message []byte, registrar_chan chan<- []RegistrarEvent) (err error) {
  if len(message) != 20 {
    err = errors.New(fmt.Sprintf("ACKN message corruption (%d)", len(message)))
    return
  }

  // Read the nonce and sequence number acked
  nonce, sequence := string(message[:16]), binary.BigEndian.Uint32(message[16:20])

  // Grab the payload the ACK corresponds to by using nonce
  payload, found := p.pending_payloads[nonce]
  if !found {
    // Don't fail here in case we had temporary issues and resend a payload, only for us to receive duplicate ACKN
    return
  }

  ack_events := payload.ack_events

  // Full ACK?
  // TODO: Protocol error if sequence is too large?
  if int(sequence) >= payload.num_events-payload.payload_start {
    // No more events left for this payload, free the payload memory
    payload.ack_events = len(payload.events)
    payload.payload = nil
    delete(p.pending_payloads, nonce)
  } else {
    // Only process the ACK if something was actually processed
    if int(sequence) > payload.num_events-payload.ack_events {
      payload.ack_events = int(sequence) + payload.payload_start
      // If we need to resend, we'll need to regenerate payload, so free that memory early
      payload.payload = nil
    }
  }

  // We potentially receive out-of-order ACKs due to payloads distributed across servers
  // This is where we enforce ordering again to ensure registrar receives ACK in order
  if payload == p.first_payload {
    out_of_sync := p.out_of_sync + 1
    for payload.ack_events != 0 {
      if payload.ack_events != len(payload.events) {
        registrar_chan <- []RegistrarEvent{&EventsEvent{Events: payload.events[:payload.ack_events]}}
        payload.events = payload.events[payload.ack_events:]
        payload.num_events = len(payload.events)
        payload.ack_events = 0
        payload.payload_start = 0
        break
      }

      registrar_chan <- []RegistrarEvent{&EventsEvent{Events: payload.events}}
      payload = payload.next
      p.first_payload = payload
      p.num_payloads--
      out_of_sync--
      p.out_of_sync = out_of_sync

      // Resume sending if we stopped due to excessive pending payload count
      if p.can_send == nil {
        p.can_send = p.transport.CanSend()
      }

      if payload == nil {
        break
      }
    }
  } else if ack_events == 0 {
    // Mark out of sync so we resend earlier packets in case they were lost
    p.out_of_sync++
  }

  // Set a timeout of the first payload if out of sync as we should be expecting it any time
  if p.out_of_sync != 0 && p.first_payload.timeout == nil {
    timeout := time.Now().Add(p.config.Timeout)
    p.first_payload.timeout = &timeout
  }

  return
}