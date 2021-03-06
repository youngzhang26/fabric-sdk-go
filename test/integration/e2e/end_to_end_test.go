/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package e2e

import (
	"testing"

	"github.com/hyperledger/fabric-sdk-go/pkg/core/config"
	"github.com/hyperledger/fabric-sdk-go/test/integration"
)

func TestE2E(t *testing.T) {
	configPath := integration.GetConfigPath("config_test.yaml")
	//End to End testing
	Run(t, config.FromFile(configPath))

	//Using setup done set above by end to end test, run below test with new config which has no orderer config inside
	runWithNoOrdererConfig(t, config.FromFile(integration.GetConfigPath("config_test_no_orderer.yaml")))
}
