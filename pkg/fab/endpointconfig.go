/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package fab

import (
	"crypto/tls"
	"crypto/x509"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/multi"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/logging"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/core"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	commtls "github.com/hyperledger/fabric-sdk-go/pkg/core/config/comm/tls"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config/cryptoutil"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config/endpoint"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config/lookup"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/cryptosuite"
	"github.com/hyperledger/fabric-sdk-go/pkg/util/pathvar"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

var logger = logging.NewLogger("fabsdk/fab")

const (
	defaultEndorserConnectionTimeout      = time.Second * 10
	defaultPeerResponseTimeout            = time.Minute * 3
	defaultDiscoveryGreylistExpiryTimeout = time.Second * 10
	defaultEventHubConnectionTimeout      = time.Second * 15
	defaultEventRegTimeout                = time.Second * 15
	defaultOrdererConnectionTimeout       = time.Second * 15
	defaultOrdererResponseTimeout         = time.Minute * 2
	defaultQueryTimeout                   = time.Minute * 3
	defaultExecuteTimeout                 = time.Minute * 3
	defaultResMgmtTimeout                 = time.Minute * 3
	defaultDiscoveryConnectionTimeout     = time.Second * 15
	defaultDiscoveryResponseTimeout       = time.Second * 15
	defaultConnIdleInterval               = time.Second * 30
	defaultEventServiceIdleInterval       = time.Minute * 2
	defaultChannelConfigRefreshInterval   = time.Second * 90
	defaultChannelMemshpRefreshInterval   = time.Second * 60
	defaultDiscoveryRefreshInterval       = time.Second * 5
	defaultSelectionRefreshInterval       = time.Minute * 1
	defaultCacheSweepInterval             = time.Second * 15
)

//ConfigFromBackend returns endpoint config implementation for given backend
func ConfigFromBackend(coreBackend ...core.ConfigBackend) (fab.EndpointConfig, error) {

	config := &EndpointConfig{
		backend: lookup.New(coreBackend...),
	}

	if err := config.loadEndpointConfiguration(); err != nil {
		return nil, errors.WithMessage(err, "network configuration load failed")
	}

	//print deprecated warning
	detectDeprecatedNetworkConfig(config)

	return config, nil
}

// EndpointConfig represents the endpoint configuration for the client
type EndpointConfig struct {
	backend                  *lookup.ConfigLookup
	networkConfig            *fab.NetworkConfig
	tlsCertPool              fab.CertPool
	entityMatchers           *entityMatchers
	peerConfigsByOrg         map[string][]fab.PeerConfig
	networkPeers             []fab.NetworkPeer
	ordererConfigs           []fab.OrdererConfig
	channelPeersByChannel    map[string][]fab.ChannelPeer
	channelOrderersByChannel map[string][]fab.OrdererConfig
	tlsClientCerts           []tls.Certificate
	peerMatchers             []matcherEntry
	ordererMatchers          []matcherEntry
	channelMatchers          []matcherEntry
}

//endpointConfigEntity contains endpoint config elements needed by endpointconfig
type endpointConfigEntity struct {
	Client        ClientConfig
	Channels      map[string]ChannelEndpointConfig
	Organizations map[string]OrganizationConfig
	Orderers      map[string]OrdererConfig
	Peers         map[string]PeerConfig
}

//entityMatchers for endpoint configuration
type entityMatchers struct {
	matchers map[string][]MatchConfig
}

//matcher entry mapping regex to match config
type matcherEntry struct {
	regex       *regexp.Regexp
	matchConfig MatchConfig
}

// Timeout reads timeouts for the given timeout type, if type is not found in the config
// then default is set as per the const value above for the corresponding type
func (c *EndpointConfig) Timeout(tType fab.TimeoutType) time.Duration {
	return c.getTimeout(tType)
}

// OrderersConfig returns a list of defined orderers
func (c *EndpointConfig) OrderersConfig() []fab.OrdererConfig {
	return c.ordererConfigs
}

// OrdererConfig returns the requested orderer
func (c *EndpointConfig) OrdererConfig(nameOrURL string) (*fab.OrdererConfig, bool) {

	matchingOrdererConfig := c.tryMatchingOrdererConfig(nameOrURL, true)

	if matchingOrdererConfig == nil {
		logger.Debugf("Could not find Orderer for [%s] ", nameOrURL)
		return nil, false
	}

	return matchingOrdererConfig, true
}

// PeersConfig Retrieves the fabric peers for the specified org from the
// config file provided
func (c *EndpointConfig) PeersConfig(org string) ([]fab.PeerConfig, bool) {
	peerConfigs, ok := c.peerConfigsByOrg[strings.ToLower(org)]
	return peerConfigs, ok
}

// PeerConfig Retrieves a specific peer from the configuration by name or url
func (c *EndpointConfig) PeerConfig(nameOrURL string) (*fab.PeerConfig, bool) {

	matchPeerConfig := c.tryMatchingPeerConfig(nameOrURL, true)
	if matchPeerConfig == nil {
		logger.Debugf("Could not find Peer for [%s] ", nameOrURL)
		return nil, false
	}

	return matchPeerConfig, true
}

// NetworkConfig returns the network configuration defined in the config file
func (c *EndpointConfig) NetworkConfig() *fab.NetworkConfig {
	return c.networkConfig
}

// NetworkPeers returns the network peers configuration, all the peers from all the orgs in config.
func (c *EndpointConfig) NetworkPeers() []fab.NetworkPeer {
	return c.networkPeers
}

// MappedChannelName will return channelName if it is an original channel name in the config
// if it is not, then it will try to find a channelMatcher and return its MappedName.
// If more than one matcher is found, then the first matcher in the list will be used.
func (c *EndpointConfig) mappedChannelName(networkConfig *fab.NetworkConfig, channelName string) string {

	// if channelName is the original key found in the Channels map config, then return it as is
	_, ok := networkConfig.Channels[strings.ToLower(channelName)]
	if ok {
		return channelName
	}

	// if !ok, then find a channelMatcher for channelName

	//Return if no channelMatchers are configured
	if len(c.channelMatchers) == 0 {
		return ""
	}

	//loop over channelMatchers to find the matching channel name
	for _, matcher := range c.channelMatchers {
		if matcher.regex.MatchString(channelName) {
			// get the matching matchConfig from the index number
			return matcher.matchConfig.MappedName
		}
	}

	// not matchers found, return empty
	return ""
}

// ChannelConfig returns the channel configuration
func (c *EndpointConfig) ChannelConfig(name string) (*fab.ChannelEndpointConfig, bool) {

	// get the mapped channel Name
	mappedChannelName := c.mappedChannelName(c.networkConfig, name)
	if mappedChannelName == "" {
		return nil, false
	}

	//look up in network config by channelName
	ch, ok := c.networkConfig.Channels[strings.ToLower(mappedChannelName)]
	return &ch, ok
}

// ChannelPeers returns the channel peers configuration
func (c *EndpointConfig) ChannelPeers(name string) ([]fab.ChannelPeer, bool) {

	//get mapped channel name
	mappedChannelName := c.mappedChannelName(c.networkConfig, name)
	if mappedChannelName == "" {
		return nil, false
	}

	//look up in dictionary
	peers, ok := c.channelPeersByChannel[strings.ToLower(mappedChannelName)]
	return peers, ok
}

// ChannelOrderers returns a list of channel orderers
func (c *EndpointConfig) ChannelOrderers(name string) ([]fab.OrdererConfig, bool) {
	//get mapped channel name
	mappedChannelName := c.mappedChannelName(c.networkConfig, name)
	if mappedChannelName == "" {
		return nil, false
	}

	//look up in dictionary
	orderers, ok := c.channelOrderersByChannel[strings.ToLower(mappedChannelName)]
	return orderers, ok
}

// TLSCACertPool returns the configured cert pool. If a certConfig
// is provided, the certificate is added to the pool
func (c *EndpointConfig) TLSCACertPool() fab.CertPool {
	return c.tlsCertPool
}

// EventServiceType returns the type of event service client to use
func (c *EndpointConfig) EventServiceType() fab.EventServiceType {
	etype := c.backend.GetString("client.eventService.type")
	switch etype {
	case "eventhub":
		return fab.EventHubEventServiceType
	case "deliver":
		return fab.DeliverEventServiceType
	default:
		return fab.AutoDetectEventServiceType
	}
}

// TLSClientCerts loads the client's certs for mutual TLS
func (c *EndpointConfig) TLSClientCerts() []tls.Certificate {
	return c.tlsClientCerts
}

func (c *EndpointConfig) loadPrivateKeyFromConfig(clientConfig *ClientConfig, clientCerts tls.Certificate, cb []byte) ([]tls.Certificate, error) {

	kb := clientConfig.TLSCerts.Client.Key.Bytes()

	// load the key/cert pair from []byte
	clientCerts, err := tls.X509KeyPair(cb, kb)
	if err != nil {
		return nil, errors.Errorf("Error loading cert/key pair as TLS client credentials: %s", err)
	}

	logger.Debug("pk read from config successfully")

	return []tls.Certificate{clientCerts}, nil
}

// CryptoConfigPath ...
func (c *EndpointConfig) CryptoConfigPath() string {
	return pathvar.Subst(c.backend.GetString("client.cryptoconfig.path"))
}

func (c *EndpointConfig) getTimeout(tType fab.TimeoutType) time.Duration { //nolint
	var timeout time.Duration
	switch tType {
	case fab.EndorserConnection:
		timeout = c.backend.GetDuration("client.peer.timeout.connection")
		if timeout == 0 {
			timeout = defaultEndorserConnectionTimeout
		}
	case fab.PeerResponse:
		timeout = c.backend.GetDuration("client.peer.timeout.response")
		if timeout == 0 {
			timeout = defaultPeerResponseTimeout
		}
	case fab.DiscoveryGreylistExpiry:
		timeout = c.backend.GetDuration("client.peer.timeout.discovery.greylistExpiry")
		if timeout == 0 {
			timeout = defaultDiscoveryGreylistExpiryTimeout
		}
	case fab.EventHubConnection:
		timeout = c.backend.GetDuration("client.eventService.timeout.connection")
		if timeout == 0 {
			timeout = defaultEventHubConnectionTimeout
		}
	case fab.EventReg:
		timeout = c.backend.GetDuration("client.eventService.timeout.registrationResponse")
		if timeout == 0 {
			timeout = defaultEventRegTimeout
		}
	case fab.OrdererConnection:
		timeout = c.backend.GetDuration("client.orderer.timeout.connection")
		if timeout == 0 {
			timeout = defaultOrdererConnectionTimeout
		}
	case fab.OrdererResponse:
		timeout = c.backend.GetDuration("client.orderer.timeout.response")
		if timeout == 0 {
			timeout = defaultOrdererResponseTimeout
		}
	case fab.DiscoveryConnection:
		timeout = c.backend.GetDuration("client.discovery.timeout.connection")
		if timeout == 0 {
			timeout = defaultDiscoveryConnectionTimeout
		}
	case fab.DiscoveryResponse:
		timeout = c.backend.GetDuration("client.discovery.timeout.response")
		if timeout == 0 {
			timeout = defaultDiscoveryResponseTimeout
		}
	case fab.Query:
		timeout = c.backend.GetDuration("client.global.timeout.query")
		if timeout == 0 {
			timeout = defaultQueryTimeout
		}
	case fab.Execute:
		timeout = c.backend.GetDuration("client.global.timeout.execute")
		if timeout == 0 {
			timeout = defaultExecuteTimeout
		}
	case fab.ResMgmt:
		timeout = c.backend.GetDuration("client.global.timeout.resmgmt")
		if timeout == 0 {
			timeout = defaultResMgmtTimeout
		}
	case fab.ConnectionIdle:
		timeout = c.backend.GetDuration("client.global.cache.connectionIdle")
		if timeout == 0 {
			timeout = defaultConnIdleInterval
		}
	case fab.EventServiceIdle:
		timeout = c.backend.GetDuration("client.global.cache.eventServiceIdle")
		if timeout == 0 {
			timeout = defaultEventServiceIdleInterval
		}
	case fab.ChannelConfigRefresh:
		timeout = c.backend.GetDuration("client.global.cache.channelConfig")
		if timeout == 0 {
			timeout = defaultChannelConfigRefreshInterval
		}
	case fab.ChannelMembershipRefresh:
		timeout = c.backend.GetDuration("client.global.cache.channelMembership")
		if timeout == 0 {
			timeout = defaultChannelMemshpRefreshInterval
		}
	case fab.DiscoveryServiceRefresh:
		timeout = c.backend.GetDuration("client.global.cache.discovery")
		if timeout == 0 {
			timeout = defaultDiscoveryRefreshInterval
		}
	case fab.SelectionServiceRefresh:
		timeout = c.backend.GetDuration("client.global.cache.selection")
		if timeout == 0 {
			timeout = defaultSelectionRefreshInterval
		}

	case fab.CacheSweepInterval: // EXPERIMENTAL - do we need this to be configurable?
		timeout = c.backend.GetDuration("client.cache.interval.sweep")
		if timeout == 0 {
			timeout = defaultCacheSweepInterval
		}
	}

	return timeout
}

func (c *EndpointConfig) loadEndpointConfiguration() error {

	endpointConfigEntity := endpointConfigEntity{}

	err := c.backend.UnmarshalKey("client", &endpointConfigEntity.Client)
	logger.Debugf("Client is: %+v", endpointConfigEntity.Client)
	if err != nil {
		return errors.WithMessage(err, "failed to parse 'client' config item to endpointConfigEntity.Client type")
	}

	err = c.backend.UnmarshalKey("channels", &endpointConfigEntity.Channels, lookup.WithUnmarshalHookFunction(peerChannelConfigHookFunc()))
	logger.Debugf("channels are: %+v", endpointConfigEntity.Channels)
	if err != nil {
		return errors.WithMessage(err, "failed to parse 'channels' config item to endpointConfigEntity.Channels type")
	}

	err = c.backend.UnmarshalKey("organizations", &endpointConfigEntity.Organizations)
	logger.Debugf("organizations are: %+v", endpointConfigEntity.Organizations)
	if err != nil {
		return errors.WithMessage(err, "failed to parse 'organizations' config item to endpointConfigEntity.Organizations type")
	}

	err = c.backend.UnmarshalKey("orderers", &endpointConfigEntity.Orderers)
	logger.Debugf("orderers are: %+v", endpointConfigEntity.Orderers)
	if err != nil {
		return errors.WithMessage(err, "failed to parse 'orderers' config item to endpointConfigEntity.Orderers type")
	}

	err = c.backend.UnmarshalKey("peers", &endpointConfigEntity.Peers)
	logger.Debugf("peers are: %+v", endpointConfigEntity.Peers)
	if err != nil {
		return errors.WithMessage(err, "failed to parse 'peers' config item to endpointConfigEntity.Peers type")
	}

	//load all endpointconfig entities
	err = c.loadEndpointConfigEntities(&endpointConfigEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to load channel configs")
	}

	return nil
}

func (c *EndpointConfig) loadEndpointConfigEntities(configEntity *endpointConfigEntity) error {

	//Compile the entityMatchers
	matchError := c.compileMatchers()
	if matchError != nil {
		return matchError
	}

	//load network config
	err := c.loadNetworkConfig(configEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to network config")
	}

	//load peer configs by org dictionary
	c.loadPeerConfigsByOrg()

	//load network peers
	c.loadNetworkPeers()

	//load orderer configs
	err = c.loadOrdererConfigs()
	if err != nil {
		return errors.WithMessage(err, "failed to load orderer configs")
	}

	//load channel peers
	err = c.loadChannelPeers()
	if err != nil {
		return errors.WithMessage(err, "failed to load channel peers")
	}

	//load channel orderers
	err = c.loadChannelOrderers()
	if err != nil {
		return errors.WithMessage(err, "failed to load channel orderers")
	}

	//load tls cert pool
	err = c.loadTLSCertPool()
	if err != nil {
		return errors.WithMessage(err, "failed to load TLS cert pool")
	}

	return nil
}

func (c *EndpointConfig) loadNetworkConfig(configEntity *endpointConfigEntity) error {

	//load all TLS configs, before building network config
	err := c.loadAllTLSConfig(configEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to load network TLSConfig")
	}

	networkConfig := fab.NetworkConfig{}

	//Channels
	networkConfig.Channels = make(map[string]fab.ChannelEndpointConfig)
	for chID, chNwCfg := range configEntity.Channels {

		chPeers := make(map[string]fab.PeerChannelConfig)
		for chPeer, chPeerCfg := range chNwCfg.Peers {
			chPeers[chPeer] = fab.PeerChannelConfig{
				EndorsingPeer:  chPeerCfg.EndorsingPeer,
				ChaincodeQuery: chPeerCfg.ChaincodeQuery,
				LedgerQuery:    chPeerCfg.LedgerQuery,
				EventSource:    chPeerCfg.EventSource,
			}
		}

		networkConfig.Channels[chID] = fab.ChannelEndpointConfig{
			Peers:    chPeers,
			Orderers: chNwCfg.Orderers,
			Policies: fab.ChannelPolicies{
				QueryChannelConfig: fab.QueryChannelConfigPolicy{
					RetryOpts:    chNwCfg.Policies.QueryChannelConfig.RetryOpts,
					MaxTargets:   chNwCfg.Policies.QueryChannelConfig.MaxTargets,
					MinResponses: chNwCfg.Policies.QueryChannelConfig.MinResponses,
				},
			},
		}
	}

	//Organizations
	networkConfig.Organizations = make(map[string]fab.OrganizationConfig)
	for orgName, orgConfig := range configEntity.Organizations {

		tlsKeyCertPairs := make(map[string]fab.CertKeyPair)
		for user, tlsKeyPair := range orgConfig.Users {
			tlsKeyCertPairs[user] = fab.CertKeyPair{
				Cert: tlsKeyPair.Cert.Bytes(),
				Key:  tlsKeyPair.Key.Bytes(),
			}
		}

		networkConfig.Organizations[orgName] = fab.OrganizationConfig{
			MSPID:      orgConfig.MSPID,
			CryptoPath: orgConfig.CryptoPath,
			Peers:      orgConfig.Peers,
			CertificateAuthorities: orgConfig.CertificateAuthorities,
			Users: tlsKeyCertPairs,
		}

	}

	//Orderers
	networkConfig.Orderers = make(map[string]fab.OrdererConfig)
	for name, ordererConfig := range configEntity.Orderers {
		tlsCert, _, err := ordererConfig.TLSCACerts.TLSCert()
		if err != nil {
			return errors.WithMessage(err, "failed to load orderer network config")
		}
		networkConfig.Orderers[name] = fab.OrdererConfig{
			URL:         ordererConfig.URL,
			GRPCOptions: ordererConfig.GRPCOptions,
			TLSCACert:   tlsCert,
		}
	}

	//Peers
	networkConfig.Peers = make(map[string]fab.PeerConfig)
	for name, peerConfig := range configEntity.Peers {
		tlsCert, _, err := peerConfig.TLSCACerts.TLSCert()
		if err != nil {
			return errors.WithMessage(err, "failed to load network config")
		}
		networkConfig.Peers[name] = fab.PeerConfig{
			URL:         peerConfig.URL,
			EventURL:    peerConfig.EventURL,
			GRPCOptions: peerConfig.GRPCOptions,
			TLSCACert:   tlsCert,
		}
	}

	c.networkConfig = &networkConfig
	return nil
}

//loadAllTLSConfig pre-loads all network TLS Configs
func (c *EndpointConfig) loadAllTLSConfig(configEntity *endpointConfigEntity) error {
	//resolve path and load bytes
	err := c.loadClientTLSConfig(configEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to load client TLSConfig ")
	}

	//resolve path and load bytes
	err = c.loadOrgTLSConfig(configEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to load org TLSConfig ")
	}

	//resolve path and load bytes
	err = c.loadOrdererPeerTLSConfig(configEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to load orderer/peer TLSConfig ")
	}

	//preload TLS client certs
	err = c.loadTLSClientCerts(configEntity)
	if err != nil {
		return errors.WithMessage(err, "failed to load TLS client certs ")
	}

	return nil
}

//loadClientTLSConfig pre-loads all TLSConfig bytes in client config
func (c *EndpointConfig) loadClientTLSConfig(configEntity *endpointConfigEntity) error {
	//Clients Config
	//resolve paths and org name
	configEntity.Client.Organization = strings.ToLower(configEntity.Client.Organization)
	configEntity.Client.TLSCerts.Client.Key.Path = pathvar.Subst(configEntity.Client.TLSCerts.Client.Key.Path)
	configEntity.Client.TLSCerts.Client.Cert.Path = pathvar.Subst(configEntity.Client.TLSCerts.Client.Cert.Path)

	//pre load client key and cert bytes
	err := configEntity.Client.TLSCerts.Client.Key.LoadBytes()
	if err != nil {
		return errors.WithMessage(err, "failed to load client key")
	}

	err = configEntity.Client.TLSCerts.Client.Cert.LoadBytes()
	if err != nil {
		return errors.WithMessage(err, "failed to load client cert")
	}

	return nil
}

//loadOrgTLSConfig pre-loads all TLSConfig bytes in organizations
func (c *EndpointConfig) loadOrgTLSConfig(configEntity *endpointConfigEntity) error {

	//Organizations Config
	for org, orgConfig := range configEntity.Organizations {
		for user, userConfig := range orgConfig.Users {
			//resolve paths
			userConfig.Key.Path = pathvar.Subst(userConfig.Key.Path)
			userConfig.Cert.Path = pathvar.Subst(userConfig.Cert.Path)
			//pre load key and cert bytes
			err := userConfig.Key.LoadBytes()
			if err != nil {
				return errors.WithMessage(err, "failed to load org key")
			}

			err = userConfig.Cert.LoadBytes()
			if err != nil {
				return errors.WithMessage(err, "failed to load org cert")
			}
			orgConfig.Users[user] = userConfig
		}
		configEntity.Organizations[org] = orgConfig
	}

	return nil
}

//loadTLSConfig pre-loads all TLSConfig bytes in Orderer and Peer configs
func (c *EndpointConfig) loadOrdererPeerTLSConfig(configEntity *endpointConfigEntity) error {

	//Orderers Config
	for orderer, ordererConfig := range configEntity.Orderers {
		//resolve paths
		ordererConfig.TLSCACerts.Path = pathvar.Subst(ordererConfig.TLSCACerts.Path)
		//pre load key and cert bytes
		err := ordererConfig.TLSCACerts.LoadBytes()
		if err != nil {
			return errors.WithMessage(err, "failed to load orderer cert")
		}
		configEntity.Orderers[orderer] = ordererConfig
	}

	//Peer Config
	for peer, peerConfig := range configEntity.Peers {
		//resolve paths
		peerConfig.TLSCACerts.Path = pathvar.Subst(peerConfig.TLSCACerts.Path)
		//pre load key and cert bytes
		err := peerConfig.TLSCACerts.LoadBytes()
		if err != nil {
			return errors.WithMessage(err, "failed to load peer cert")
		}
		configEntity.Peers[peer] = peerConfig
	}

	return nil
}

func (c *EndpointConfig) loadPeerConfigsByOrg() {

	c.peerConfigsByOrg = make(map[string][]fab.PeerConfig)

	for orgName, orgConfig := range c.networkConfig.Organizations {
		orgPeers := orgConfig.Peers
		peers := []fab.PeerConfig{}

		for _, peerName := range orgPeers {

			matchingPeerConfig := c.tryMatchingPeerConfig(peerName, false)
			if matchingPeerConfig != nil {
				peers = append(peers, *matchingPeerConfig)
				continue
			}

			p, ok := c.networkConfig.Peers[strings.ToLower(peerName)]
			if !ok {
				continue
			}
			if err := c.verifyPeerConfig(p, peerName, endpoint.IsTLSEnabled(p.URL)); err != nil {
				continue
			}
			peers = append(peers, p)
		}
		c.peerConfigsByOrg[strings.ToLower(orgName)] = peers
	}

}

func (c *EndpointConfig) loadNetworkPeers() {

	var netPeers []fab.NetworkPeer
	for org, peerConfigs := range c.peerConfigsByOrg {

		orgConfig, ok := c.networkConfig.Organizations[org]
		if !ok {
			continue
		}

		for _, peerConfig := range peerConfigs {
			netPeers = append(netPeers, fab.NetworkPeer{PeerConfig: peerConfig, MSPID: orgConfig.MSPID})
		}
	}

	c.networkPeers = netPeers
}

func (c *EndpointConfig) loadOrdererConfigs() error {

	ordererConfigs := []fab.OrdererConfig{}
	for name, ordererConfig := range c.networkConfig.Orderers {
		matchedOrderer := c.tryMatchingOrdererConfig(name, false)
		if matchedOrderer != nil {
			//if found in entity matcher then use the matched one
			ordererConfig = *matchedOrderer
		}

		if ordererConfig.TLSCACert == nil && !c.backend.GetBool("client.tlsCerts.systemCertPool") {
			//check for TLS config only if secured connection is enabled
			allowInSecure := ordererConfig.GRPCOptions["allow-insecure"] == true
			if endpoint.AttemptSecured(ordererConfig.URL, allowInSecure) {
				return errors.Errorf("Orderer has no certs configured. Make sure TLSCACerts.Pem or TLSCACerts.Path is set for %s", ordererConfig.URL)
			}
		}
		ordererConfigs = append(ordererConfigs, ordererConfig)
	}
	c.ordererConfigs = ordererConfigs
	return nil
}

func (c *EndpointConfig) loadChannelPeers() error {

	channelPeersByChannel := make(map[string][]fab.ChannelPeer)

	for channelID, channelConfig := range c.networkConfig.Channels {
		peers := []fab.ChannelPeer{}
		for peerName, chPeerConfig := range channelConfig.Peers {
			var p fab.PeerConfig
			matchedPeer := c.tryMatchingPeerConfig(strings.ToLower(peerName), false)
			if matchedPeer != nil {
				p = *matchedPeer
			} else {
				// Get generic peer configuration
				nwPeer, ok := c.networkConfig.Peers[strings.ToLower(peerName)]
				if !ok {
					continue
				}
				p = nwPeer
			}

			if err := c.verifyPeerConfig(p, peerName, endpoint.IsTLSEnabled(p.URL)); err != nil {
				logger.Debugf("Verify PeerConfig failed for peer [%s], cause : [%s]", peerName, err)
				return err
			}

			mspID, ok := c.peerMSPID(peerName)
			if !ok {
				return errors.Errorf("unable to find MSP ID for peer : %s", peerName)
			}

			networkPeer := fab.NetworkPeer{PeerConfig: p, MSPID: mspID}

			peer := fab.ChannelPeer{PeerChannelConfig: chPeerConfig, NetworkPeer: networkPeer}

			peers = append(peers, peer)
		}
		channelPeersByChannel[strings.ToLower(channelID)] = peers
	}

	c.channelPeersByChannel = channelPeersByChannel

	return nil
}

func (c *EndpointConfig) loadChannelOrderers() error {

	channelOrderersByChannel := make(map[string][]fab.OrdererConfig)

	for channelID, channelConfig := range c.networkConfig.Channels {
		orderers := []fab.OrdererConfig{}
		for _, ordererName := range channelConfig.Orderers {

			orderer := c.tryMatchingOrdererConfig(strings.ToLower(ordererName), false)
			if orderer != nil {
				orderers = append(orderers, *orderer)
				continue
			}

			logger.Debugf("Could not find Orderer for [%s] through Entity Matchers", ordererName)
			nwOrderer, ok := c.networkConfig.Orderers[strings.ToLower(ordererName)]
			if !ok {
				return errors.Errorf("Could not find Orderer Config for channel orderer [%s]", ordererName)
			}
			orderers = append(orderers, nwOrderer)

		}
		channelOrderersByChannel[strings.ToLower(channelID)] = orderers
	}

	c.channelOrderersByChannel = channelOrderersByChannel

	return nil
}

func (c *EndpointConfig) loadTLSCertPool() error {

	c.tlsCertPool = commtls.NewCertPool(c.backend.GetBool("client.tlsCerts.systemCertPool"))

	// preemptively add all TLS certs to cert pool as adding them at request time
	// is expensive
	certs, err := c.loadTLSCerts()
	if err != nil {
		logger.Infof("could not cache TLS certs: %s", err)
	}

	if _, err := c.tlsCertPool.Get(certs...); err != nil {
		return errors.WithMessage(err, "cert pool load failed")
	}
	return nil
}

// loadTLSClientCerts loads the client's certs for mutual TLS
// It checks the config for embedded pem files before looking for cert files
func (c *EndpointConfig) loadTLSClientCerts(configEntity *endpointConfigEntity) error {

	var clientCerts tls.Certificate
	cb := configEntity.Client.TLSCerts.Client.Cert.Bytes()
	if len(cb) == 0 {
		// if no cert found in the config, empty cert chain should be used
		c.tlsClientCerts = []tls.Certificate{clientCerts}
		return nil
	}

	// Load private key from cert using default crypto suite
	cs := cryptosuite.GetDefault()
	pk, err := cryptoutil.GetPrivateKeyFromCert(cb, cs)

	// If CryptoSuite fails to load private key from cert then load private key from config
	if err != nil || pk == nil {
		logger.Debugf("Reading pk from config, unable to retrieve from cert: %s", err)
		tlsClientCerts, err := c.loadPrivateKeyFromConfig(&configEntity.Client, clientCerts, cb)
		if err != nil {
			return errors.WithMessage(err, "failed to load TLS client certs")
		}
		c.tlsClientCerts = tlsClientCerts
		return nil
	}

	// private key was retrieved from cert
	clientCerts, err = cryptoutil.X509KeyPair(cb, pk, cs)
	if err != nil {
		return errors.WithMessage(err, "failed to load TLS client certs, failed to get X509KeyPair")
	}

	c.tlsClientCerts = []tls.Certificate{clientCerts}
	return nil
}

func (c *EndpointConfig) tryMatchingPeerConfig(peerName string, searchByURL bool) *fab.PeerConfig {

	//loop over peer entity matchers to find the matching peer
	for _, matcher := range c.peerMatchers {
		if matcher.regex.MatchString(peerName) {
			return c.matchPeer(peerName, matcher)
		}
		logger.Debugf("Peer [%s] did not match using matcher [%s]", peerName, matcher.regex.String())
	}

	//direct lookup if peer matchers are not configured or no matchers matched
	peerConfig, ok := c.networkConfig.Peers[strings.ToLower(peerName)]
	if ok {
		return &peerConfig
	}

	if searchByURL {
		//lookup by URL
		for _, staticPeerConfig := range c.networkConfig.Peers {
			if strings.EqualFold(staticPeerConfig.URL, peerName) {
				return &staticPeerConfig
			}
		}
	}

	return nil
}

func (c *EndpointConfig) matchPeer(peerName string, matcher matcherEntry) *fab.PeerConfig {

	matchedPeer := c.getMappedPeer(matcher.matchConfig.MappedHost)
	if matchedPeer == nil {
		logger.Debugf("Could not find mapped host [%s] for peer [%s]", matcher.matchConfig.MappedHost, peerName)
		return nil
	}

	//URLSubstitutionExp if found use from entity matcher otherwise use from mapped host
	if matcher.matchConfig.URLSubstitutionExp != "" {
		matchedPeer.URL = matcher.matchConfig.URLSubstitutionExp
		//check for regex replace '$'
		if strings.Contains(matchedPeer.URL, "$") {
			matchedPeer.URL = matcher.regex.ReplaceAllString(peerName, matchedPeer.URL)
		}
	}

	//EventURLSubstitutionExp if found use from entity matcher otherwise use from mapped host
	if matcher.matchConfig.EventURLSubstitutionExp != "" {
		matchedPeer.EventURL = matcher.matchConfig.EventURLSubstitutionExp
		//check for regex replace '$'
		if strings.Contains(matchedPeer.EventURL, "$") {
			matchedPeer.EventURL = matcher.regex.ReplaceAllString(peerName, matchedPeer.EventURL)
		}
	}

	//SSLTargetOverrideURLSubstitutionExp if found use from entity matcher otherwise use from mapped host
	if matcher.matchConfig.SSLTargetOverrideURLSubstitutionExp != "" {
		sslTargetOverride := matcher.matchConfig.SSLTargetOverrideURLSubstitutionExp
		if strings.Contains(sslTargetOverride, "$") {
			sslTargetOverride = matcher.regex.ReplaceAllString(peerName, sslTargetOverride)
		}
		matchedPeer.GRPCOptions["ssl-target-name-override"] = sslTargetOverride
	}

	return matchedPeer
}

func (c *EndpointConfig) getMappedPeer(host string) *fab.PeerConfig {
	//Get the peerConfig from mapped host
	peerConfig, ok := c.networkConfig.Peers[strings.ToLower(host)]
	if !ok {
		return nil
	}

	mappedConfig := fab.PeerConfig{
		URL:         peerConfig.URL,
		EventURL:    peerConfig.EventURL,
		TLSCACert:   peerConfig.TLSCACert,
		GRPCOptions: make(map[string]interface{}),
	}

	for key, val := range peerConfig.GRPCOptions {
		mappedConfig.GRPCOptions[key] = val
	}

	//TODO default grpc options

	return &mappedConfig
}

func (c *EndpointConfig) tryMatchingOrdererConfig(ordererName string, searchByURL bool) *fab.OrdererConfig {

	//loop over orderer entity matchers to find the matching orderer
	for _, matcher := range c.ordererMatchers {
		if matcher.regex.MatchString(ordererName) {
			return c.matchOrderer(ordererName, matcher)
		}
		logger.Debugf("Orderer [%s] did not match using matcher [%s]", ordererName, matcher.regex.String())
	}

	//direct lookup if orderer matchers are not configured or no matchers matched
	orderer, ok := c.networkConfig.Orderers[strings.ToLower(ordererName)]
	if ok {
		return &orderer
	}

	if searchByURL {
		//lookup by URL
		for _, ordererCfg := range c.OrderersConfig() {
			if strings.EqualFold(ordererCfg.URL, ordererName) {
				return &ordererCfg
			}
		}
	}

	return nil
}

func (c *EndpointConfig) matchOrderer(ordererName string, matcher matcherEntry) *fab.OrdererConfig {

	//Get the ordererConfig from mapped host
	matchedOrderer := c.getMappedOrderer(matcher.matchConfig.MappedHost)
	if matchedOrderer == nil {
		logger.Debugf("Could not find mapped host [%s] for orderer [%s]", matcher.matchConfig.MappedHost, ordererName)
		return nil
	}

	//URLSubstitutionExp if found use from entity matcher otherwise use from mapped host
	if matcher.matchConfig.URLSubstitutionExp != "" {
		matchedOrderer.URL = matcher.matchConfig.URLSubstitutionExp
		//check for regex replace '$'
		if strings.Contains(matchedOrderer.URL, "$") {
			matchedOrderer.URL = matcher.regex.ReplaceAllString(ordererName, matchedOrderer.URL)
		}
	}

	//SSLTargetOverrideURLSubstitutionExp if found use from entity matcher otherwise use from mapped host
	if matcher.matchConfig.SSLTargetOverrideURLSubstitutionExp != "" {
		sslTargetOverride := matcher.matchConfig.SSLTargetOverrideURLSubstitutionExp
		if strings.Contains(sslTargetOverride, "$") {
			sslTargetOverride = matcher.regex.ReplaceAllString(ordererName, sslTargetOverride)
		}
		matchedOrderer.GRPCOptions["ssl-target-name-override"] = sslTargetOverride
	}

	return matchedOrderer
}

func (c *EndpointConfig) getMappedOrderer(host string) *fab.OrdererConfig {
	//Get the peerConfig from mapped host
	ordererConfig, ok := c.networkConfig.Orderers[strings.ToLower(host)]
	if !ok {
		return nil
	}

	mappedConfig := fab.OrdererConfig{
		URL:         ordererConfig.URL,
		TLSCACert:   ordererConfig.TLSCACert,
		GRPCOptions: make(map[string]interface{}),
	}

	for key, val := range ordererConfig.GRPCOptions {
		mappedConfig.GRPCOptions[key] = val
	}

	//TODO default grpc options

	return &mappedConfig
}

func (c *EndpointConfig) compileMatchers() error {

	entityMatchers := entityMatchers{}

	err := c.backend.UnmarshalKey("entityMatchers", &entityMatchers.matchers)
	logger.Debugf("Matchers are: %+v", entityMatchers)
	if err != nil {
		return errors.WithMessage(err, "failed to parse 'entityMatchers' config item")
	}

	//return no error if entityMatchers is not configured
	if len(entityMatchers.matchers) == 0 {
		logger.Debug("Entity matchers are not configured")
		return nil
	}

	err = c.compileAllMatchers(&entityMatchers)
	if err != nil {
		return err
	}

	c.entityMatchers = &entityMatchers
	return nil
}

func (c *EndpointConfig) compileAllMatchers(matcherConfig *entityMatchers) error {

	var err error
	if len(matcherConfig.matchers["channel"]) > 0 {
		c.channelMatchers, err = c.groupAllMatchers(matcherConfig.matchers["channel"])
		if err != nil {
			return err
		}
	}

	if len(matcherConfig.matchers["orderer"]) > 0 {
		c.ordererMatchers, err = c.groupAllMatchers(matcherConfig.matchers["orderer"])
		if err != nil {
			return err
		}
	}

	if len(matcherConfig.matchers["peer"]) > 0 {
		c.peerMatchers, err = c.groupAllMatchers(matcherConfig.matchers["peer"])
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *EndpointConfig) groupAllMatchers(matchers []MatchConfig) ([]matcherEntry, error) {
	matcherEntries := make([]matcherEntry, len(matchers))
	for i, v := range matchers {
		regex, err := regexp.Compile(v.Pattern)
		if err != nil {
			return nil, err
		}
		matcherEntries[i] = matcherEntry{regex: regex, matchConfig: v}
	}
	return matcherEntries, nil
}

func (c *EndpointConfig) verifyPeerConfig(p fab.PeerConfig, peerName string, tlsEnabled bool) error {
	if p.URL == "" {
		return errors.Errorf("URL does not exist or empty for peer %s", peerName)
	}
	if tlsEnabled && p.TLSCACert == nil && !c.backend.GetBool("client.tlsCerts.systemCertPool") {
		return errors.Errorf("tls.certificate does not exist or empty for peer %s", peerName)
	}
	return nil
}

func (c *EndpointConfig) loadTLSCerts() ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	errs := multi.Errors{}

	for _, peer := range c.networkPeers {
		if peer.TLSCACert != nil {
			certs = append(certs, peer.TLSCACert)
		}
	}
	for _, orderer := range c.ordererConfigs {
		if orderer.TLSCACert != nil {
			certs = append(certs, orderer.TLSCACert)
		}
	}
	return certs, errs.ToError()
}

//ResetNetworkConfig clears network config cache
func (c *EndpointConfig) ResetNetworkConfig() error {
	c.networkConfig = nil
	return c.loadEndpointConfiguration()
}

// PeerMSPID returns msp that peer belongs to
func (c *EndpointConfig) peerMSPID(name string) (string, bool) {
	var mspID string
	// Find organisation/msp that peer belongs to
	for _, org := range c.networkConfig.Organizations {
		for i := 0; i < len(org.Peers); i++ {
			if strings.EqualFold(org.Peers[i], name) {
				// peer belongs to this org add org msp
				mspID = org.MSPID
				break
			} else {
				peer, ok := c.findMatchingPeer(org.Peers[i])
				if ok && strings.EqualFold(peer, name) {
					mspID = org.MSPID
					break
				}
			}
		}
	}

	return mspID, mspID != ""
}

func (c *EndpointConfig) findMatchingPeer(peerName string) (string, bool) {

	//Return if no peerMatchers are configured
	if len(c.peerMatchers) == 0 {
		return "", false
	}

	//loop over peerentityMatchers to find the matching peer
	for _, matcher := range c.peerMatchers {
		if matcher.regex.MatchString(peerName) {
			return matcher.matchConfig.MappedHost, true
		}
	}

	return "", false
}

//peerChannelConfigHookFunc returns hook function for unmarshalling 'fab.PeerChannelConfig'
// Rule : default set to 'true' if not provided in config
func peerChannelConfigHookFunc() mapstructure.DecodeHookFunc {
	return func(
		f reflect.Type,
		t reflect.Type,
		data interface{}) (interface{}, error) {

		//If target is of type 'fab.PeerChannelConfig', then only hook should work
		if t == reflect.TypeOf(PeerChannelConfig{}) {
			dataMap, ok := data.(map[string]interface{})
			if ok {
				setDefault(dataMap, "endorsingpeer", true)
				setDefault(dataMap, "chaincodequery", true)
				setDefault(dataMap, "ledgerquery", true)
				setDefault(dataMap, "eventsource", true)

				return dataMap, nil
			}
		}

		return data, nil
	}
}

//setDefault sets default value provided to map if given key not found
func setDefault(dataMap map[string]interface{}, key string, defaultVal bool) {
	_, ok := dataMap[key]
	if !ok {
		dataMap[key] = true
	}
}

//detectDeprecatedConfigOptions detects deprecated config options and prints warnings
// currently detects: if channels.orderers are defined
func detectDeprecatedNetworkConfig(endpointConfig *EndpointConfig) {

	if endpointConfig.networkConfig == nil {
		return
	}

	//detect if channels orderers are mentioned
	for _, v := range endpointConfig.networkConfig.Channels {
		if len(v.Orderers) > 0 {
			logger.Warn("Getting orderers from endpoint config channels.orderer is deprecated, use entity matchers to override orderer configuration")
			logger.Warn("visit https://github.com/hyperledger/fabric-sdk-go/blob/master/test/fixtures/config/overrides/local_entity_matchers.yaml for samples")
			break
		}
	}
}
