/*
 * Copyright 2014-2015 Jason Woods.
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

package tcp

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"regexp"
	"time"

	"github.com/driskell/log-courier/lc-lib/addresspool"
	"github.com/driskell/log-courier/lc-lib/config"
	"github.com/driskell/log-courier/lc-lib/core"
	"github.com/driskell/log-courier/lc-lib/transports"
)

const (
	defaultReconnect    time.Duration = 0 * time.Second
	defaultReconnectMax time.Duration = 300 * time.Second
)

// TransportTCPFactory holds the configuration from the configuration file
// It allows creation of TransportTCP instances that use this configuration
type TransportTCPFactory struct {
	// Constructor
	config         *config.Config
	transport      string
	hostportRegexp *regexp.Regexp

	// Configuration
	Reconnect      time.Duration `config:"reconnect backoff"`
	ReconnectMax   time.Duration `config:"reconnect backoff max"`
	SSLCertificate string        `config:"ssl certificate"`
	SSLKey         string        `config:"ssl key"`
	SSLCA          string        `config:"ssl ca"`

	// Internal
	certificate     *tls.Certificate
	certificateList []*x509.Certificate
	caList          []*x509.Certificate
}

// NewTransportTCPFactory create a new TransportTCPFactory from the provided
// configuration data, reporting back any configuration errors it discovers.
func NewTransportTCPFactory(p *config.Parser, configPath string, unUsed map[string]interface{}, name string) (transports.TransportFactory, error) {
	var err error

	ret := &TransportTCPFactory{
		config:         p.Config(),
		transport:      name,
		hostportRegexp: regexp.MustCompile(`^\[?([^]]+)\]?:([0-9]+)$`),
	}

	if err = p.Populate(ret, unUsed, configPath, true); err != nil {
		return nil, err
	}

	// Only allow SSL configurations if using TLS
	if name == TransportTCPTLS {
		if len(ret.SSLCertificate) > 0 || len(ret.SSLKey) > 0 {
			if len(ret.SSLCertificate) == 0 {
				return nil, errors.New("'tls' transport 'ssl key' is only valid with a matching 'ssl certificate' option")
			}

			if len(ret.SSLKey) == 0 {
				return nil, errors.New("'tls' transport 'ssl key' must be specified when 'ssl certificate' is specified")
			}

			certificate, err := tls.LoadX509KeyPair(ret.SSLCertificate, ret.SSLKey)
			if err != nil {
				return nil, fmt.Errorf("failed loading 'tls' transport 'ssl certificate': %s", err)
			}

			ret.certificate = &certificate

			for _, certBytes := range ret.certificate.Certificate {
				thisCert, err := x509.ParseCertificate(certBytes)
				if err != nil {
					return nil, fmt.Errorf("failed loading 'tls' transport 'ssl certificate': %s", err)
				}
				ret.certificateList = append(ret.certificateList, thisCert)
			}
		}

		if len(ret.SSLCA) == 0 {
			return nil, errors.New("'ssl ca' is required when transport is 'tls'")
		}

		pemdata, err := ioutil.ReadFile(ret.SSLCA)
		if err != nil {
			return nil, fmt.Errorf("failure loading 'tls' transport 'ssl ca': %s", err)
		}
		rest := pemdata
		var block *pem.Block
		var pemBlockNum = 1
		for {
			block, rest = pem.Decode(rest)
			if block != nil {
				if block.Type != "CERTIFICATE" {
					return nil, fmt.Errorf("failure loading 'tls' transport 'ssl ca': block %d does not contain a certificate", pemBlockNum)
				}
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, fmt.Errorf("failure loading 'tls' transport 'ssl ca': failed to parse CA certificate in block %d", pemBlockNum)
				}
				ret.caList = append(ret.caList, cert)
				pemBlockNum++
			} else {
				break
			}
		}
	} else {
		if len(ret.SSLCertificate) > 0 || len(ret.SSLKey) > 0 || len(ret.SSLCA) > 0 {
			return nil, fmt.Errorf("'tcp' transport does not support 'ssl certificate', 'ssl key' or 'ssl ca' options")
		}
	}

	return ret, nil
}

// Defaults sets the default configuration values
func (f *TransportTCPFactory) Defaults() {
	f.Reconnect = defaultReconnect
	f.ReconnectMax = defaultReconnectMax
}

// NewTransport returns a new Transport interface using the settings from the
// TransportTCPFactory.
func (f *TransportTCPFactory) NewTransport(context interface{}, pool *addresspool.Pool, eventChan chan<- transports.Event, finishOnFail bool) transports.Transport {
	ret := &transportTCP{
		config:         f,
		netConfig:      transports.FetchConfig(f.config),
		finishOnFail:   finishOnFail,
		context:        context,
		pool:           pool,
		eventChan:      eventChan,
		controllerChan: make(chan error, 1),
		connectionChan: make(chan *socketMessage),
		backoff:        core.NewExpBackoff(pool.Server()+" Reconnect", f.Reconnect, f.ReconnectMax),
	}

	ret.startController()
	return ret
}

// Register the transports
func init() {
	transports.RegisterTransport(TransportTCPTCP, NewTransportTCPFactory)
	transports.RegisterTransport(TransportTCPTLS, NewTransportTCPFactory)
}