# Copyright 2014 Jason Woods.
#
# This file is a modification of code from Logstash Forwarder.
# Copyright 2012-2013 Jordan Sissel and contributors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

require 'thread'
require 'timeout'
require 'zlib'
require 'json'

module Lumberjack
  class ShutdownSignal < StandardError; end
  class ProtocolError < StandardError; end

  # Implementation of the server
  class Server
    attr_reader :port

    def initialize(options = {})
      @options = {
        :logger => nil,
        :transport => 'tls'
      }.merge(options)

      @logger = @options[:logger]

      case @options[:transport]
      when 'tls'
        require 'lumberjack/server_tls'
        @server = ServerTls.new(@options)
      when 'zmq'
        require 'lumberjack/server_zmq'
        @server = ServerZmq.new(@options)
      else
        raise '[Lumberjack] \'transport\' must be either tls or zmq'
      end

      # Grab the port back
      @port = @server.port
    end

    def run(&block)
      # TODO: Make queue size configurable
      event_queue = SizedQueue.new 10
      spooler_thread = nil

      begin
        # Why a spooler thread? Well we don't know what &block is! We want connection threads to be non-blocking so they DON'T timeout
        # Non-blocking means we can keep clients informed of progress, and response in a timely fashion. We could create this with
        # a timeout wrapper around the &block call but we'd then be generating exceptions in someone else's code
        # So we allow the caller to block us - but only our spooler thread - our other threads are safe and we can use timeout
        spooler_thread = Thread.new do
          loop do
            events = event_queue.pop
            break if events.nil?
            events.each do |event|
              block.call event
            end
          end
        end

        # Receive messages and process them
        @server.run do |signature, message, comm|
          case signature
          when 'PING'
            process_ping message, comm
          when 'JDAT'
            process_jdat message, comm, event_queue
          else
            @logger.warn("[LumberjackServer] Unknown message received from #{comm.peer}") unless @logger.nil?
            # Don't kill a client that sends a bad message
            # Just reject it and let it send it again, potentially to another server
            comm.send '????', ''
          end
        end
      ensure
        # Signal the spooler thread to stop
        unless spooler_thread.nil?
          event_queue << nil
          spooler_thread.join
        end
      end
    end

    def process_ping(message, comm)
      # Size of message should be 0
      if message.length != 0
        # TODO: log something
        raise ProtocolError
      end

      # PONG!
      comm.send 'PONG', ''
    end

    def process_jdat(message, comm, event_queue)
      # Now we have the data, aim to respond within 5 seconds
      reset_ack_timeout

      # OK - first is a nonce - we send this back with sequence acks
      # This allows the client to know what is being acknowledged
      # Nonce is 16 so check we have enough
      if message.length < 17
        # TODO: log something
        raise ProtocolError
      end

      nonce = message[0...16]

      # The remainder of the message is the compressed data block
      message = Zlib::Inflate.inflate(message[16...message.length])

      # Message now contains JSON encoded events
      # They are aligned as [length][event]... so on
      # We acknowledge them by their 1-index position in the stream
      # A 0 sequence acknowledgement means we haven't processed any yet
      p = 0
      sequence = 0
      events = []
      while p < message.length
        if message.length - p < 4
          # TODO: log something
          raise ProtocolError
        end

        length = message[p...p + 4].unpack('N').first
        p += 4

        # Check length is valid
        if message.length - p < length
          # TODO: log something
          raise ProtocolError
        end

        # Extract message, and force UTF-8 to ensure we don't break anything, replacing invalid sequences
        data = message[p...p + length].encode('utf-8', 'binary', :invalid => :replace, :undef => :replace, :replace => '?').force_encoding('UTF-8')
        p += length

        # Decode the JSON
        begin
          event = JSON.parse(data)
        rescue JSON::ParserError => e
          @logger.warn("[LumberjackServer] JSON parse failure, falling back to plain-text: #{e}") unless @logger.nil?
          event = { 'message' => data }
        end

        events << event

        sequence += 1
      end

      # Queue the events
      begin
        Timeout.timeout(@ack_timeout - Time.now.to_i) do
          event_queue << events
        end
      rescue Timeout::Error
        # Full pipeline, partial ack
        comm.send('ACKN', [nonce, sequence].pack('A*N'))
        reset_ack_timeout
        retry
      end

      # Acknowledge the full message
      comm.send('ACKN', [nonce, sequence].pack('A*N'))
    end

    def reset_ack_timeout
      # TODO: Make a constant or configurable
      @ack_timeout = Time.now.to_i + 5
    end
  end
end
