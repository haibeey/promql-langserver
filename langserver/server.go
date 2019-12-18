// Copyright 2019 Tobias Guggenmos
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This File includes code from the go/tools project which is governed by the following license:
// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package langserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/prometheus/client_golang/api"
	"github.com/slrtbtfs/promql-lsp/langserver/cache"
	"github.com/slrtbtfs/promql-lsp/vendored/go-tools/jsonrpc2"
	"github.com/slrtbtfs/promql-lsp/vendored/go-tools/lsp/protocol"
)

// Server wraps language server instance that can connect to exactly one client
type Server struct {
	server *server
}

// server is a language server instance that can connect to exactly one client
type server struct {
	Conn   *jsonrpc2.Conn
	client protocol.Client

	state   serverState
	stateMu sync.Mutex

	cache cache.DocumentCache

	config *Config

	prometheus api.Client

	lifetime context.Context
	exit     func()
}

type serverState int

const (
	serverCreated = serverState(iota)
	serverInitializing
	serverInitialized // set once the server has received "initialized" request
	serverShutDown
)

// Run starts the language server instance
func (s Server) Run() error {
	return s.server.Conn.Run(s.server.lifetime)
}

// ServerFromStream generates a Server from a jsonrpc2.Stream
func ServerFromStream(ctx context.Context, stream jsonrpc2.Stream, config *Config) (context.Context, Server) {
	s := &server{}

	switch config.RPCTrace {
	case "text":
		stream = protocol.LoggingStream(stream, os.Stderr)
	case "json":
		stream = JSONLogStream(stream, os.Stderr)
	}

	if config.PrometheusURL != "" {
		var err error

		s.prometheus, err = api.NewClient(api.Config{Address: config.PrometheusURL})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to connect to prometheus %s\n", config.PrometheusURL)
		}

		fmt.Fprintln(os.Stderr, "Prometheus: ", config.PrometheusURL)
	} else {
		fmt.Fprintln(os.Stderr, "No Prometheus")
	}

	ctx, s.Conn, s.client = protocol.NewServer(ctx, stream, s)
	s.config = config

	s.lifetime, s.exit = context.WithCancel(ctx)

	return ctx, Server{s}
}

// TCPServer generates a Server listening on the provided TCP Address, creating a new language Server
// instance for every connection
func RunTCPServers(ctx context.Context, addr string, config *Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		go ServerFromStream(ctx, jsonrpc2.NewHeaderStream(conn, conn), config)
	}
}

// StdioServer generates a Server talking to stdio
func StdioServer(ctx context.Context, config *Config) (context.Context, Server) {
	stream := jsonrpc2.NewHeaderStream(os.Stdin, os.Stdout)
	return ServerFromStream(ctx, stream, config)
}
