/*
Copyright 2018-2019 Gravitational, Inc.

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

package local

import (
	"bytes"
	"context"
	"sort"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
)

// EventsService implements service to watch for events
type EventsService struct {
	*logrus.Entry
	backend          backend.Backend
	getClusterConfig getClusterConfigFunc
}

// NewEventsService returns new events service instance
func NewEventsService(b backend.Backend, getClusterConfig getClusterConfigFunc) *EventsService {
	return &EventsService{
		Entry:            logrus.WithFields(logrus.Fields{trace.Component: "Events"}),
		backend:          b,
		getClusterConfig: getClusterConfig,
	}
}

// NewWatcher returns a new event watcher
func (e *EventsService) NewWatcher(ctx context.Context, watch services.Watch) (services.Watcher, error) {
	if len(watch.Kinds) == 0 {
		return nil, trace.BadParameter("global watches are not supported yet")
	}
	var parsers []eventTranslator
	var prefixes [][]byte
	for _, kind := range watch.Kinds {
		if kind.Name != "" && kind.Kind != services.KindNamespace {
			return nil, trace.BadParameter("watch with Name is only supported for Namespace resource")
		}
		var parser eventTranslator
		switch kind.Kind {
		case services.KindCertAuthority:
			parser = newCertAuthorityParser(kind.LoadSecrets)
		case services.KindToken:
			parser = newProvisionTokenParser()
		case services.KindStaticTokens:
			parser = newStaticTokensParser()
		case services.KindClusterConfig:
			parser = newClusterConfigParser(e.getClusterConfig)
		case types.KindClusterNetworkingConfig:
			parser = newClusterNetworkingConfigParser(e.getClusterConfig)
		case types.KindClusterAuthPreference:
			parser = newAuthPreferenceParser()
		case types.KindSessionRecordingConfig:
			parser = newSessionRecordingConfigParser(e.getClusterConfig)
		case services.KindClusterName:
			parser = newClusterNameParser()
		case services.KindNamespace:
			parser = newNamespaceParser(kind.Name)
		case services.KindRole:
			parser = newRoleParser()
		case services.KindUser:
			parser = newUserParser()
		case services.KindNode:
			parser = newNodeParser()
		case services.KindProxy:
			parser = newProxyParser()
		case services.KindAuthServer:
			parser = newAuthServerParser()
		case services.KindTunnelConnection:
			parser = newTunnelConnectionParser()
		case services.KindReverseTunnel:
			parser = newReverseTunnelParser()
		case services.KindAccessRequest:
			p, err := newAccessRequestParser(kind.Filter)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			parser = p
		case services.KindAppServer:
			parser = newAppServerParser()
		case services.KindWebSession:
			switch kind.SubKind {
			case services.KindAppSession:
				parser = newAppSessionParser()
			case services.KindWebSession:
				parser = newWebSessionParser()
			default:
				return nil, trace.BadParameter("watcher on object subkind %q is not supported", kind.SubKind)
			}
		case services.KindWebToken:
			parser = newWebTokenParser()
		case services.KindRemoteCluster:
			parser = newRemoteClusterParser()
		case services.KindKubeService:
			parser = newKubeServiceParser()
		case types.KindDatabaseServer:
			parser = newDatabaseServerParser()
		default:
			return nil, trace.BadParameter("watcher on object kind %q is not supported", kind.Kind)
		}
		prefixes = append(prefixes, parser.prefix())
		parsers = append(parsers, parser)
	}
	// sort so that longer prefixes get first
	sort.Slice(parsers, func(i, j int) bool { return len(parsers[i].prefix()) > len(parsers[j].prefix()) })
	w, err := e.backend.NewWatcher(ctx, backend.Watch{
		Name:            watch.Name,
		Prefixes:        prefixes,
		QueueSize:       watch.QueueSize,
		MetricComponent: watch.MetricComponent,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return newWatcher(w, e.Entry, parsers), nil
}

func newWatcher(backendWatcher backend.Watcher, l *logrus.Entry, parsers []eventTranslator) *watcher {
	w := &watcher{
		backendWatcher: backendWatcher,
		Entry:          l,
		parsers:        parsers,
		eventsC:        make(chan services.Event),
	}
	go w.forwardEvents()
	return w
}

type watcher struct {
	*logrus.Entry
	parsers        []eventTranslator
	backendWatcher backend.Watcher
	eventsC        chan services.Event
}

func (w *watcher) Error() error {
	return nil
}

func (w *watcher) translate(e backend.Event) ([]services.Event, error) {
	for _, p := range w.parsers {
		if e.Type == backend.OpInit {
			return []services.Event{{Type: e.Type}}, nil
		}
		if p.match(e.Item.Key) {
			return p.translateMatching(e)
		}
	}
	return nil, trace.NotFound("no match found for %v %v", e.Type, string(e.Item.Key))
}

func (w *watcher) forwardEvents() {
	for {
		select {
		case <-w.backendWatcher.Done():
			return
		case event := <-w.backendWatcher.Events():
			convertedEvents, err := w.translate(event)
			if err != nil {
				// not found errors are expected, for example
				// when namespace prefix is watched, it captures
				// node events as well, and there could be no
				// handler registered for nodes, only for namespaces
				if !trace.IsNotFound(err) {
					w.Warning(trace.DebugReport(err))
				}
				continue
			}
			for _, converted := range convertedEvents {
				select {
				case w.eventsC <- converted:
				case <-w.backendWatcher.Done():
					return
				}
			}
		}
	}
}

// Events returns channel with events
func (w *watcher) Events() <-chan services.Event {
	return w.eventsC
}

// Done returns the channel signalling the closure
func (w *watcher) Done() <-chan struct{} {
	return w.backendWatcher.Done()
}

// Close closes the watcher and releases
// all associated resources
func (w *watcher) Close() error {
	return w.backendWatcher.Close()
}

// eventTranslator is an interface for translating matching events from backend
// event stream into higher level events.
type eventTranslator interface {
	// translateMatching translates a matching backend event into a list of
	// higher level events.
	translateMatching(event backend.Event) ([]services.Event, error)
	// match returns true if event key matches.
	match(key []byte) bool
	// prefix returns prefix to watch.
	prefix() []byte
}

// simpleMatcher is a partial implementation of eventTranslator for the most common
// resource types (stored under a static prefix).
type simpleMatcher struct {
	matchPrefix []byte
}

func (p simpleMatcher) prefix() []byte {
	return p.matchPrefix
}

func (p simpleMatcher) match(key []byte) bool {
	return bytes.HasPrefix(key, p.matchPrefix)
}

func newCertAuthorityParser(loadSecrets bool) *certAuthorityParser {
	return &certAuthorityParser{
		loadSecrets:   loadSecrets,
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(authoritiesPrefix)},
	}
}

type certAuthorityParser struct {
	simpleMatcher
	loadSecrets bool
}

func (p *certAuthorityParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		caType, name, err := baseTwoKeys(event.Item.Key)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, &services.ResourceHeader{
			Kind:    services.KindCertAuthority,
			SubKind: caType,
			Version: services.V2,
			Metadata: services.Metadata{
				Name:      name,
				Namespace: defaults.Namespace,
			},
		})
	case backend.OpPut:
		ca, err := services.UnmarshalCertAuthority(event.Item.Value,
			services.WithResourceID(event.Item.ID), services.WithExpires(event.Item.Expires), services.SkipValidation())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// never send private signing keys over event stream?
		// this might not be true
		setSigningKeys(ca, p.loadSecrets)
		return resourcesToEvents(event.Type, ca)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newProvisionTokenParser() *provisionTokenParser {
	return &provisionTokenParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(tokensPrefix)},
	}
}

type provisionTokenParser struct {
	simpleMatcher
}

func (p *provisionTokenParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindToken, services.V2, 0, "")
	case backend.OpPut:
		token, err := services.UnmarshalProvisionToken(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, token)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newStaticTokensParser() *staticTokensParser {
	return &staticTokensParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(clusterConfigPrefix, staticTokensPrefix)},
	}
}

type staticTokensParser struct {
	simpleMatcher
}

func (p *staticTokensParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindStaticTokens, services.V2, 0, services.MetaNameStaticTokens)
	case backend.OpPut:
		tokens, err := services.UnmarshalStaticTokens(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, tokens)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newClusterConfigParser(getClusterConfig getClusterConfigFunc) *clusterConfigParser {
	return &clusterConfigParser{
		simpleMatcher:    simpleMatcher{matchPrefix: backend.Key(clusterConfigPrefix, generalPrefix)},
		getClusterConfig: getClusterConfig,
	}
}

type clusterConfigParser struct {
	simpleMatcher
	getClusterConfig getClusterConfigFunc
}

func (p *clusterConfigParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindClusterConfig, services.V3, 0, services.MetaNameClusterConfig)
	case backend.OpPut:
		// To ensure backward compatibility, do not use the ClusterConfig
		// resource passed with the event but perform a separate get from the
		// backend. The resource fetched in this way is populated with all the
		// fields expected by legacy event consumers.  DELETE IN 8.0.0
		clusterConfig, err := p.getClusterConfig()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, clusterConfig)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newClusterNetworkingConfigParser(getClusterConfig getClusterConfigFunc) *clusterNetworkingConfigParser {
	return &clusterNetworkingConfigParser{
		simpleMatcher:    simpleMatcher{matchPrefix: backend.Key(clusterConfigPrefix, networkingPrefix)},
		getClusterConfig: getClusterConfig,
	}
}

type clusterNetworkingConfigParser struct {
	simpleMatcher
	getClusterConfig getClusterConfigFunc
}

func (p *clusterNetworkingConfigParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, types.KindClusterNetworkingConfig, services.V2, 0, types.MetaNameClusterNetworkingConfig)
	case backend.OpPut:
		netConfig, err := services.UnmarshalClusterNetworkingConfig(
			event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
			services.SkipValidation(),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// To ensure backward compatibility, also emit an event indicating
		// ClusterConfig change to legacy event consumers.  DELETE IN 8.0.0
		clusterConfig, err := p.getClusterConfig()
		if err != nil {
			logrus.WithError(err).Warn("Failed to get cluster config.")
			return resourcesToEvents(event.Type, netConfig)
		}
		return resourcesToEvents(event.Type, netConfig, clusterConfig)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newAuthPreferenceParser() *authPreferenceParser {
	return &authPreferenceParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(authPrefix, preferencePrefix, generalPrefix)},
	}
}

type authPreferenceParser struct {
	simpleMatcher
}

func (p *authPreferenceParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindClusterAuthPreference, services.V2, 0, services.MetaNameClusterAuthPreference)
	case backend.OpPut:
		ap, err := services.UnmarshalAuthPreference(
			event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
			services.SkipValidation(),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, ap)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newSessionRecordingConfigParser(getClusterConfig getClusterConfigFunc) *sessionRecordingConfigParser {
	return &sessionRecordingConfigParser{
		simpleMatcher:    simpleMatcher{matchPrefix: backend.Key(clusterConfigPrefix, sessionRecordingPrefix)},
		getClusterConfig: getClusterConfig,
	}
}

type sessionRecordingConfigParser struct {
	simpleMatcher
	getClusterConfig getClusterConfigFunc
}

func (p *sessionRecordingConfigParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, types.KindSessionRecordingConfig, services.V2, 0, types.MetaNameSessionRecordingConfig)
	case backend.OpPut:
		recConfig, err := services.UnmarshalSessionRecordingConfig(
			event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
			services.SkipValidation(),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// To ensure backward compatibility, also emit an event indicating
		// ClusterConfig change to legacy event consumers.  DELETE IN 8.0.0
		clusterConfig, err := p.getClusterConfig()
		if err != nil {
			logrus.WithError(err).Warn("Failed to get cluster config.")
			return resourcesToEvents(event.Type, recConfig)
		}
		return resourcesToEvents(event.Type, recConfig, clusterConfig)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newClusterNameParser() *clusterNameParser {
	return &clusterNameParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(clusterConfigPrefix, namePrefix)},
	}
}

type clusterNameParser struct {
	simpleMatcher
}

func (p *clusterNameParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindClusterName, services.V2, 0, services.MetaNameClusterName)
	case backend.OpPut:
		clusterName, err := services.UnmarshalClusterName(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, clusterName)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newNamespaceParser(name string) *namespaceParser {
	p := &namespaceParser{}
	if name == "" {
		p.matchPrefix = backend.Key(namespacesPrefix)
	} else {
		p.matchPrefix = backend.Key(namespacesPrefix, name, paramsPrefix)
	}
	return p
}

type namespaceParser struct {
	simpleMatcher
}

func (p *namespaceParser) match(key []byte) bool {
	// namespaces are stored under key '/namespaces/<namespace-name>/params'
	// and this code matches similar pattern
	return bytes.HasPrefix(key, p.matchPrefix) &&
		bytes.HasSuffix(key, []byte(paramsPrefix)) &&
		bytes.Count(key, []byte{backend.Separator}) == 3
}

func (p *namespaceParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindNamespace, services.V2, 1, "")
	case backend.OpPut:
		namespace, err := services.UnmarshalNamespace(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, namespace)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newRoleParser() *roleParser {
	return &roleParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(rolesPrefix)},
	}
}

type roleParser struct {
	simpleMatcher
}

func (p *roleParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindRole, services.V3, 1, "")
	case backend.OpPut:
		resource, err := services.UnmarshalRole(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newAccessRequestParser(m map[string]string) (*accessRequestParser, error) {
	var filter services.AccessRequestFilter
	if err := filter.FromMap(m); err != nil {
		return nil, trace.Wrap(err)
	}
	return &accessRequestParser{
		filter:      filter,
		matchPrefix: backend.Key(accessRequestsPrefix),
		matchSuffix: backend.Key(paramsPrefix),
	}, nil
}

type accessRequestParser struct {
	filter      services.AccessRequestFilter
	matchPrefix []byte
	matchSuffix []byte
}

func (p *accessRequestParser) prefix() []byte {
	return p.matchPrefix
}

func (p *accessRequestParser) match(key []byte) bool {
	if !bytes.HasPrefix(key, p.matchPrefix) {
		return false
	}
	if !bytes.HasSuffix(key, p.matchSuffix) {
		return false
	}
	return true
}

func (p *accessRequestParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindAccessRequest, services.V3, 1, "")
	case backend.OpPut:
		req, err := itemToAccessRequest(event.Item)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if !p.filter.Match(req) {
			return nil, nil
		}
		return resourcesToEvents(event.Type, req)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newUserParser() *userParser {
	return &userParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(webPrefix, usersPrefix)},
	}
}

type userParser struct {
	simpleMatcher
}

func (p *userParser) match(key []byte) bool {
	// users are stored under key '/web/users/<username>/params'
	// and this code matches similar pattern
	return bytes.HasPrefix(key, p.matchPrefix) &&
		bytes.HasSuffix(key, []byte(paramsPrefix)) &&
		bytes.Count(key, []byte{backend.Separator}) == 4
}

func (p *userParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindUser, services.V2, 1, "")
	case backend.OpPut:
		resource, err := services.UnmarshalUser(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newNodeParser() *nodeParser {
	return &nodeParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(nodesPrefix, defaults.Namespace)},
	}
}

type nodeParser struct {
	simpleMatcher
}

func (p *nodeParser) translateMatching(event backend.Event) ([]services.Event, error) {
	return parseServer(event, services.KindNode)
}

func newProxyParser() *proxyParser {
	return &proxyParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(proxiesPrefix)},
	}
}

type proxyParser struct {
	simpleMatcher
}

func (p *proxyParser) translateMatching(event backend.Event) ([]services.Event, error) {
	return parseServer(event, services.KindProxy)
}

func newAuthServerParser() *authServerParser {
	return &authServerParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(authServersPrefix)},
	}
}

type authServerParser struct {
	simpleMatcher
}

func (p *authServerParser) translateMatching(event backend.Event) ([]services.Event, error) {
	return parseServer(event, services.KindAuthServer)
}

func newTunnelConnectionParser() *tunnelConnectionParser {
	return &tunnelConnectionParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(tunnelConnectionsPrefix)},
	}
}

type tunnelConnectionParser struct {
	simpleMatcher
}

func (p *tunnelConnectionParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		clusterName, name, err := baseTwoKeys(event.Item.Key)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, &services.ResourceHeader{
			Kind:    services.KindTunnelConnection,
			SubKind: clusterName,
			Version: services.V2,
			Metadata: services.Metadata{
				Name:      name,
				Namespace: defaults.Namespace,
			},
		})
	case backend.OpPut:
		resource, err := services.UnmarshalTunnelConnection(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newReverseTunnelParser() *reverseTunnelParser {
	return &reverseTunnelParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(reverseTunnelsPrefix)},
	}
}

type reverseTunnelParser struct {
	simpleMatcher
}

func (p *reverseTunnelParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindReverseTunnel, services.V2, 0, "")
	case backend.OpPut:
		resource, err := services.UnmarshalReverseTunnel(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newAppServerParser() *appServerParser {
	return &appServerParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(appsPrefix, serversPrefix, defaults.Namespace)},
	}
}

type appServerParser struct {
	simpleMatcher
}

func (p *appServerParser) translateMatching(event backend.Event) ([]services.Event, error) {
	return parseServer(event, services.KindAppServer)
}

func newAppSessionParser() *webSessionParser {
	return &webSessionParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(appsPrefix, sessionsPrefix)},
		hdr: services.ResourceHeader{
			Kind:    services.KindWebSession,
			SubKind: services.KindAppSession,
			Version: services.V2,
		},
	}
}

func newWebSessionParser() *webSessionParser {
	return &webSessionParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(webPrefix, sessionsPrefix)},
		hdr: services.ResourceHeader{
			Kind:    services.KindWebSession,
			SubKind: services.KindWebSession,
			Version: services.V2,
		},
	}
}

type webSessionParser struct {
	simpleMatcher
	hdr services.ResourceHeader
}

func (p *webSessionParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeaderWithTemplate(event, p.hdr, 0)
	case backend.OpPut:
		resource, err := services.UnmarshalWebSession(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newWebTokenParser() *webTokenParser {
	return &webTokenParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(webPrefix, tokensPrefix)},
	}
}

type webTokenParser struct {
	simpleMatcher
}

func (p *webTokenParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindWebToken, services.V1, 0, "")
	case backend.OpPut:
		resource, err := services.UnmarshalWebToken(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newKubeServiceParser() *kubeServiceParser {
	return &kubeServiceParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(kubeServicesPrefix)},
	}
}

type kubeServiceParser struct {
	simpleMatcher
}

func (p *kubeServiceParser) translateMatching(event backend.Event) ([]services.Event, error) {
	return parseServer(event, services.KindKubeService)
}

func newDatabaseServerParser() *databaseServerParser {
	return &databaseServerParser{
		simpleMatcher: simpleMatcher{matchPrefix: backend.Key(dbServersPrefix, defaults.Namespace)},
	}
}

type databaseServerParser struct {
	simpleMatcher
}

func (p *databaseServerParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		hostID, name, err := baseTwoKeys(event.Item.Key)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, &types.DatabaseServerV3{
			Kind:    types.KindDatabaseServer,
			Version: types.V3,
			Metadata: services.Metadata{
				Name:        name,
				Namespace:   defaults.Namespace,
				Description: hostID, // Pass host ID via description field for the cache.
			},
		})
	case backend.OpPut:
		resource, err := services.UnmarshalDatabaseServer(
			event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
			services.SkipValidation())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func parseServer(event backend.Event, kind string) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, kind, services.V2, 0, "")
	case backend.OpPut:
		resource, err := services.UnmarshalServer(event.Item.Value,
			kind,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
			services.SkipValidation(),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}
}

func newRemoteClusterParser() *remoteClusterParser {
	return &remoteClusterParser{
		matchPrefix: backend.Key(remoteClustersPrefix),
	}
}

type remoteClusterParser struct {
	matchPrefix []byte
}

func (p *remoteClusterParser) prefix() []byte {
	return p.matchPrefix
}

func (p *remoteClusterParser) match(key []byte) bool {
	return bytes.HasPrefix(key, p.matchPrefix)
}

func (p *remoteClusterParser) translateMatching(event backend.Event) ([]services.Event, error) {
	switch event.Type {
	case backend.OpDelete:
		return resourceHeader(event, services.KindRemoteCluster, services.V3, 0, "")
	case backend.OpPut:
		resource, err := services.UnmarshalRemoteCluster(event.Item.Value,
			services.WithResourceID(event.Item.ID),
			services.WithExpires(event.Item.Expires),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resourcesToEvents(event.Type, resource)
	default:
		return nil, trace.BadParameter("event %v is not supported", event.Type)
	}

	// 	resource, err := b.parseResource(event)
	// 	if err != nil {
	// 		return nil, trace.Wrap(err)
	// 	}
	// 	// If resource is nil, then it was well-formed but is being filtered out.
	// 	if resource == nil {
	// 		return nil, nil
	// 	}
	// 	return []services.Event{{Type: event.Type, Resource: resource}}, nil
}

func resourceHeader(event backend.Event, kind, version string, offset int, name string) ([]services.Event, error) {
	if name == "" {
		var err error
		name, err = base(event.Item.Key, offset)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return resourcesToEvents(event.Type, &services.ResourceHeader{
		Kind:    kind,
		Version: version,
		Metadata: services.Metadata{
			Name:      name,
			Namespace: defaults.Namespace,
		},
	})
}

func resourceHeaderWithTemplate(event backend.Event, hdr services.ResourceHeader, offset int) ([]services.Event, error) {
	name, err := base(event.Item.Key, offset)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return resourcesToEvents(event.Type, &services.ResourceHeader{
		Kind:    hdr.Kind,
		SubKind: hdr.SubKind,
		Version: hdr.Version,
		Metadata: services.Metadata{
			Name:      name,
			Namespace: defaults.Namespace,
		},
	})
}

func resourcesToEvents(eventType types.OpType, resources ...services.Resource) ([]services.Event, error) {
	events := []services.Event{}
	for _, resource := range resources {
		// If resource is nil, then it was well-formed but is being filtered out.
		if resource == nil {
			continue
		}
		events = append(events, services.Event{Type: eventType, Resource: resource})
	}
	return events, nil
}

// WaitForEvent waits for the event matched by the specified event matcher in the given watcher.
func WaitForEvent(ctx context.Context, watcher services.Watcher, m EventMatcher, clock clockwork.Clock) (services.Resource, error) {
	tick := clock.NewTicker(defaults.WebHeadersTimeout)
	defer tick.Stop()

	select {
	case event := <-watcher.Events():
		if event.Type != backend.OpInit {
			return nil, trace.BadParameter("expected init event, got %v instead", event.Type)
		}
	case <-watcher.Done():
		// Watcher closed, probably due to a network error.
		return nil, trace.ConnectionProblem(watcher.Error(), "watcher is closed")
	case <-tick.Chan():
		return nil, trace.LimitExceeded("timed out waiting for initialize event")
	}

	for {
		select {
		case event := <-watcher.Events():
			res, err := m.Match(event)
			if err == nil {
				return res, nil
			}
			if !trace.IsCompareFailed(err) {
				logrus.WithError(err).Debug("Failed to match event.")
			}
		case <-watcher.Done():
			// Watcher closed, probably due to a network error.
			return nil, trace.ConnectionProblem(watcher.Error(), "watcher is closed")
		case <-tick.Chan():
			return nil, trace.LimitExceeded("timed out waiting for event")
		}
	}
}

// Match matches the specified resource event by applying itself
func (r EventMatcherFunc) Match(event services.Event) (services.Resource, error) {
	return r(event)
}

// EventMatcherFunc matches the specified resource event.
// Implements EventMatcher
type EventMatcherFunc func(services.Event) (services.Resource, error)

// EventMatcher matches a specific resource event
type EventMatcher interface {
	// Match matches the specified event.
	// Returns the matched resource if successful.
	// Returns trace.CompareFailedError for no match.
	Match(services.Event) (services.Resource, error)
}

// base returns last element delimited by separator, index is
// is an index of the key part to get counting from the end
func base(key []byte, offset int) (string, error) {
	parts := bytes.Split(key, []byte{backend.Separator})
	if len(parts) < offset+1 {
		return "", trace.NotFound("failed parsing %v", string(key))
	}
	return string(parts[len(parts)-offset-1]), nil
}

// baseTwoKeys returns two last keys
func baseTwoKeys(key []byte) (string, string, error) {
	parts := bytes.Split(key, []byte{backend.Separator})
	if len(parts) < 2 {
		return "", "", trace.NotFound("failed parsing %v", string(key))
	}
	return string(parts[len(parts)-2]), string(parts[len(parts)-1]), nil
}

// getClusterConfigFunc gets ClusterConfig to facilitate backward compatible
// transition to standalone configuration resources.  DELETE IN 8.0.0
type getClusterConfigFunc func(...services.MarshalOption) (services.ClusterConfig, error)
