/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mongo

import (
	"context"
	"crypto/tls"
	"io"
	"net"

	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/db/common"
	"github.com/gravitational/teleport/lib/srv/db/mongo/protocol"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
)

// Engine implements the MySQL database service that accepts client
// connections coming over reverse tunnel from the proxy and proxies
// them between the proxy and the MySQL database instance.
//
// Implements common.Engine.
type Engine struct {
	// Auth handles database access authentication.
	Auth common.Auth
	// Audit emits database access audit events.
	Audit common.Audit
	// Context is the database server close context.
	Context context.Context
	// Clock is the clock interface.
	Clock clockwork.Clock
	// Log is used for logging.
	Log logrus.FieldLogger
}

// HandleConnection processes the connection from MySQL proxy coming
// over reverse tunnel.
//
// It handles all necessary startup actions, authorization and acts as a
// middleman between the proxy and the database intercepting and interpreting
// all messages i.e. doing protocol parsing.
func (e *Engine) HandleConnection(ctx context.Context, sessionCtx *common.Session, clientConn net.Conn) (err error) {
	// Perform authorization checks.
	err = e.checkAccess(sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	// Establish connection to the MongoDB server.
	serverConn, err := e.connect(ctx, sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		err := serverConn.Close()
		if err != nil {
			e.Log.WithError(err).Error("Failed to close connection to MongoDB server.")
		}
	}()
	e.Audit.OnSessionStart(e.Context, sessionCtx, nil)
	defer e.Audit.OnSessionEnd(e.Context, sessionCtx)
	// Copy between the connections.
	clientErrCh := make(chan error, 1)
	serverErrCh := make(chan error, 1)
	go e.receiveFromClient(clientConn, serverConn, clientErrCh, sessionCtx)
	go e.receiveFromServer(serverConn, clientConn, serverErrCh)
	select {
	case err := <-clientErrCh:
		e.Log.WithError(err).Debug("Client done.")
	case err := <-serverErrCh:
		e.Log.WithError(err).Debug("Server done.")
	case <-ctx.Done():
		e.Log.Debug("Context canceled.")
	}
	return nil
}

// checkAccess does authorization check for MySQL connection about to be established.
func (e *Engine) checkAccess(sessionCtx *common.Session) error {
	ap, err := e.Auth.GetAuthPreference()
	if err != nil {
		return trace.Wrap(err)
	}
	mfaParams := services.AccessMFAParams{
		Verified:       sessionCtx.Identity.MFAVerified != "",
		AlwaysRequired: ap.GetRequireSessionMFA(),
	}
	// Only the username is checked upon initial connection. MongoDB sends
	// database name with each protocol message (for query, update, etc.)
	// so it is checked when we receive a message from client.
	err = sessionCtx.Checker.CheckAccessToDatabase(sessionCtx.Server, mfaParams,
		&services.DatabaseLabelsMatcher{Labels: sessionCtx.Server.GetAllLabels()},
		&services.DatabaseUserMatcher{User: sessionCtx.DatabaseUser})
	if err != nil {
		e.Audit.OnSessionStart(e.Context, sessionCtx, err)
		return trace.Wrap(err)
	}
	return nil
}

// connect establishes connection to MongoDB database.
func (e *Engine) connect(ctx context.Context, sessionCtx *common.Session) (*tls.Conn, error) {
	tlsConfig, err := e.Auth.GetTLSConfig(ctx, sessionCtx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsConn, err := tls.Dial("tcp", sessionCtx.Server.GetURI(), tlsConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return tlsConn, nil
}

// receiveFromClient relays protocol messages received from MongoDB client
// to MongoDB database server.
func (e *Engine) receiveFromClient(clientConn, serverConn net.Conn, clientErrCh chan<- error, sessionCtx *common.Session) {
	log := e.Log.WithFields(logrus.Fields{
		"from":   "client",
		"client": clientConn.RemoteAddr(),
		"server": serverConn.RemoteAddr(),
	})
	defer func() {
		log.Debug("Stop receiving from client.")
		close(clientErrCh)
	}()
	for {
		message, err := protocol.ReadMessage(clientConn)
		if err != nil {
			if utils.IsOKNetworkError(err) {
				log.Debug("Client connection closed.")
				return
			}
			log.WithError(err).Error("Failed to read MongoDB client message.")
			clientErrCh <- err
			return
		}
		switch msg := message.(type) {
		case *protocol.MessageOpMsg:
			var documents []string
			for _, document := range msg.GetDocuments() {
				documents = append(documents, document.String())
			}
			e.Audit.OnQuery(e.Context, sessionCtx, common.Query{
				Database:  msg.GetDatabase(),
				Documents: documents,
			})
			// TODO(r0mant): Check access to the database name here. This will
			// require forming an error response wire message in case of access
			// denied and sending it back to the client.
		case *protocol.MessageUnknown:
			log.Debugf("=== %v", msg)
		}
		_, err = serverConn.Write(message.GetBytes())
		if err != nil {
			log.WithError(err).Error("Failed to write MongoDB server message.")
			clientErrCh <- err
			return
		}
	}
}

// receiveFromServer relays protocol messages received from MongoDB database
// server to MongoDB client.
func (e *Engine) receiveFromServer(serverConn, clientConn net.Conn, serverErrCh chan<- error) {
	log := e.Log.WithFields(logrus.Fields{
		"from":   "server",
		"client": clientConn.RemoteAddr(),
		"server": serverConn.RemoteAddr(),
	})
	defer func() {
		log.Debug("Stop receiving from server.")
		close(serverErrCh)
	}()
	_, err := io.Copy(clientConn, serverConn)
	if err != nil {
		if utils.IsOKNetworkError(err) {
			log.Debug("Server connection closed.")
			return
		}
		serverErrCh <- trace.Wrap(err)
	}
}
